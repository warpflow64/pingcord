package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

func footer() *embedFooter {
	host, _ := os.Hostname()
	return &embedFooter{Text: fmt.Sprintf("pingcord %s @ %s", version, host)}
}

func startupPayload(cfg *Config) message {
	return message{Embeds: []embed{{
		Title:       "ℹ️ pingcord 起動",
		Description: fmt.Sprintf("監視対象 %d 件 / deadman %d 件 の監視を開始しました。", len(cfg.Targets), len(cfg.Deadman)),
		Color:       colorBlue,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Footer:      footer(),
	}}}
}

func shutdownPayload() message {
	return message{Embeds: []embed{{
		Title:       "⚠️ pingcord 停止",
		Description: "監視プロセスが停止しました。意図しない停止であればコンテナの状態を確認してください。",
		Color:       colorOrange,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Footer:      footer(),
	}}}
}

func testPayload(cfg *Config) message {
	p := message{Embeds: []embed{{
		Title:       "🧪 pingcord テスト通知",
		Description: "Webhook の設定は正常です。DOWN 時のメンションは本文に付与されます。",
		Color:       colorBlue,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Footer:      footer(),
	}}}
	if cfg.Discord.Mention != "" {
		p.Content = cfg.Discord.Mention + " (メンションのテスト)"
	}
	return p
}

// statusPayload は全監視対象の現在の状態をまとめる (定期レポートと /status で使う)。
// 定期レポートは毎日届くことで「pingcord 自体が生きている」ことの確認 (Watchdog) にもなる。
func statusPayload(statuses []TargetStatus, title string) message {
	var up, unknown int
	var down []string
	for _, s := range statuses {
		switch s.Status {
		case "up":
			up++
		case "down":
			line := fmt.Sprintf("• **%s** `%s`", s.Name, s.Address)
			if s.DownSince != nil {
				line += fmt.Sprintf(" — <t:%d:R> から", s.DownSince.Unix())
			}
			if s.AckedBy != "" {
				line += " ✋ " + s.AckedBy
			}
			down = append(down, line)
		default:
			unknown++
		}
	}

	e := embed{
		Title:     title,
		Color:     colorGreen,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Footer:    footer(),
	}
	desc := fmt.Sprintf("🟢 UP %d / 🔴 DOWN %d / ⚪ UNKNOWN %d", up, len(down), unknown)
	if len(down) > 0 {
		e.Color = colorRed
		desc += "\n\n**ダウン中**\n" + truncate(strings.Join(down, "\n"), 3500)
	} else if unknown > 0 {
		e.Color = colorGrey
	}
	e.Description = desc
	return message{Embeds: []embed{e}}
}

// nextSummary は times ("HH:MM") のうち now より後で最も近い時刻を返す。
func nextSummary(now time.Time, times []string) time.Time {
	var next time.Time
	for _, s := range times {
		hm, _ := time.Parse("15:04", s)
		t := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	return next
}

func runSummary(ctx context.Context, times []string, snapshot func() []TargetStatus, post func(message)) {
	for {
		next := nextSummary(time.Now(), times)
		slog.Debug("next summary scheduled", "at", next)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
			post(statusPayload(snapshot(), "📊 定期レポート"))
		}
	}
}

// runHeartbeat は外部の deadman サービスへ定期的に GET を送る。
// これが途絶えると外部サービス側でアラートが上がる (VyOS ごと落ちた場合の検知用)。
func runHeartbeat(ctx context.Context, cfg HeartbeatConfig) {
	client := &http.Client{Timeout: 10 * time.Second}
	beat := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
		if err != nil {
			slog.Error("heartbeat request", "err", err)
			return
		}
		req.Header.Set("User-Agent", "pingcord/"+version)
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("heartbeat failed", "err", err)
			}
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			slog.Warn("heartbeat failed", "status", resp.Status)
			return
		}
		slog.Debug("heartbeat sent")
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		beat()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
