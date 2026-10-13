// pingcord は MGMT 機器などの死活を監視し、Discord Webhook に通知する。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // distroless でも TZ=Asia/Tokyo を使えるようにする
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/pingcord/config.yaml", "path to config file")
	checkOnly := flag.Bool("check", false, "validate the config and exit")
	testNotify := flag.Bool("test", false, "send a test message to Discord and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	setupLogger()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("invalid config", "path", *configPath, "err", err)
		os.Exit(1)
	}
	if *checkOnly {
		fmt.Printf("config OK: %d targets, %d deadman checks\n", len(cfg.Targets), len(cfg.Deadman))
		return
	}

	notifier := NewNotifier(cfg.Discord, cfg.Notify.GroupWait)
	if *testNotify {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := notifier.Send(ctx, testPayload(cfg)); err != nil {
			slog.Error("test notification failed", "err", err)
			os.Exit(1)
		}
		fmt.Println("test notification sent")
		return
	}

	if err := run(cfg, notifier); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *Config, notifier *Notifier) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 権限不足で全 ICMP 対象が DOWN 扱いになるのを避けるため、起動時に確認して止める
	if slices.ContainsFunc(cfg.Targets, func(t Target) bool { return t.Type == "icmp" }) {
		conn, _, err := listenICMP(false)
		if err != nil {
			return err
		}
		conn.Close()
	}

	monitor := NewMonitor(cfg, notifier.Notify)
	deadman := NewDeadman(cfg, notifier.Notify)

	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(ctx)
		}()
	}

	if cfg.HTTP.Listen != "" {
		srv := &server{cfg: cfg.HTTP, monitor: monitor, deadman: deadman}
		ln, err := srv.listen()
		if err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
		start(func(ctx context.Context) { srv.serve(ctx, ln) })
	}

	notifierCtx, stopNotifier := context.WithCancel(context.Background())
	go notifier.Run(notifierCtx)
	if cfg.Notify.Startup == nil || *cfg.Notify.Startup {
		notifier.Post(startupPayload(cfg))
	}

	slog.Info("pingcord started", "version", version, "targets", len(cfg.Targets), "deadman", len(cfg.Deadman))
	start(monitor.Run)
	start(deadman.Run)
	if cfg.Heartbeat.URL != "" {
		start(func(ctx context.Context) { runHeartbeat(ctx, cfg.Heartbeat) })
	}
	if len(cfg.Notify.SummaryAt) > 0 {
		snapshot := func() []TargetStatus { return append(monitor.Snapshot(), deadman.Snapshot()...) }
		start(func(ctx context.Context) { runSummary(ctx, cfg.Notify.SummaryAt, snapshot, notifier.Post) })
	}

	<-ctx.Done()
	slog.Info("shutting down")
	wg.Wait()
	if cfg.Notify.Shutdown == nil || *cfg.Notify.Shutdown {
		notifier.Post(shutdownPayload())
	}
	stopNotifier()
	notifier.Wait()
	return nil
}

func setupLogger() {
	level := slog.LevelInfo
	level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}
