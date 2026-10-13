package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Discord   DiscordConfig   `yaml:"discord"`
	Defaults  Defaults        `yaml:"defaults"`
	Notify    NotifyConfig    `yaml:"notify"`
	Heartbeat HeartbeatConfig `yaml:"heartbeat"`
	HTTP      HTTPConfig      `yaml:"http"`
	Deadman   []DeadmanCheck  `yaml:"deadman"`
	Targets   []Target        `yaml:"targets"`
}

type DiscordConfig struct {
	WebhookURL string `yaml:"webhook_url"`
	Username   string `yaml:"username"`
	AvatarURL  string `yaml:"avatar_url"`
	// DOWN 通知時に本文へ付けるメンション (例: "<@&ROLE_ID>", "@here")
	Mention string `yaml:"mention"`
}

type Defaults struct {
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailThreshold    int           `yaml:"fail_threshold"`
	RecoverThreshold int           `yaml:"recover_threshold"`
	RepeatInterval   time.Duration `yaml:"repeat_interval"`
	ICMPCount        int           `yaml:"icmp_count"`
}

type NotifyConfig struct {
	// 同時に発生したイベントを 1 メッセージにまとめるための待ち時間
	GroupWait time.Duration `yaml:"group_wait"`
	Startup   *bool         `yaml:"startup"`
	Shutdown  *bool         `yaml:"shutdown"`
	// 定期レポートを送る時刻 (TZ 環境変数のタイムゾーンで "HH:MM")
	SummaryAt []string `yaml:"summary_at"`
}

// HeartbeatConfig は外部の deadman サービス (healthchecks.io, Uptime Kuma の Push 等) へ
// 定期的にリクエストを送り、pingcord 自身や VyOS が落ちたことを外から検知させるための設定。
type HeartbeatConfig struct {
	URL      string        `yaml:"url"`
	Interval time.Duration `yaml:"interval"`
}

type HTTPConfig struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

// DeadmanCheck は機器やスクリプトから /ping/<name> が timeout 以内に届かなければ DOWN とする監視。
type DeadmanCheck struct {
	Name        string        `yaml:"name"`
	Timeout     time.Duration `yaml:"timeout"`
	Description string        `yaml:"description"`
	Mention     string        `yaml:"mention"`
}

type Target struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // icmp | tcp | http
	Host        string `yaml:"host"` // icmp: IP/ホスト名, tcp: host:port
	URL         string `yaml:"url"`  // http
	Description string `yaml:"description"`
	Mention     string `yaml:"mention"`

	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailThreshold    int           `yaml:"fail_threshold"`
	RecoverThreshold int           `yaml:"recover_threshold"`
	ICMPCount        int           `yaml:"icmp_count"`
	// "0s" で明示的に再通知を無効化できるよう、未指定 (nil) と区別する
	RepeatIntervalOverride *time.Duration `yaml:"repeat_interval"`
	RepeatInterval         time.Duration  `yaml:"-"`

	ExpectStatus       int  `yaml:"expect_status"`
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

func (t *Target) Address() string {
	if t.Type == "http" {
		return t.URL
	}
	return t.Host
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := envRef.ReplaceAllStringFunc(string(raw), func(m string) string {
		return os.Getenv(envRef.FindStringSubmatch(m)[1])
	})

	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Discord.WebhookURL == "" {
		c.Discord.WebhookURL = os.Getenv("DISCORD_WEBHOOK_URL")
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// resolveMention は個別設定が空なら全体設定を使い、"none" ならメンションしない。
func resolveMention(own, global string) string {
	switch own {
	case "":
		return global
	case "none":
		return ""
	}
	return own
}

func (c *Config) applyDefaults() {
	setDefault(&c.Discord.Username, "pingcord")

	d := &c.Defaults
	setDefault(&d.Interval, 30*time.Second)
	setDefault(&d.Timeout, 2*time.Second)
	setDefault(&d.FailThreshold, 3)
	setDefault(&d.RecoverThreshold, 2)
	setDefault(&d.ICMPCount, 3)

	setDefault(&c.Notify.GroupWait, 10*time.Second)
	setDefault(&c.Heartbeat.Interval, time.Minute)

	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Type == "" {
			if t.URL != "" {
				t.Type = "http"
			} else {
				t.Type = "icmp"
			}
		}
		t.Type = strings.ToLower(t.Type)
		setDefault(&t.Interval, d.Interval)
		setDefault(&t.Timeout, d.Timeout)
		setDefault(&t.FailThreshold, d.FailThreshold)
		setDefault(&t.RecoverThreshold, d.RecoverThreshold)
		t.RepeatInterval = d.RepeatInterval
		if t.RepeatIntervalOverride != nil {
			t.RepeatInterval = *t.RepeatIntervalOverride
		}
		setDefault(&t.ICMPCount, d.ICMPCount)
		t.Mention = resolveMention(t.Mention, c.Discord.Mention)
	}
	for i := range c.Deadman {
		c.Deadman[i].Mention = resolveMention(c.Deadman[i].Mention, c.Discord.Mention)
	}
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Discord.WebhookURL == "" {
		add("discord.webhook_url (or DISCORD_WEBHOOK_URL) is required")
	} else if u, err := url.Parse(c.Discord.WebhookURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		add("discord.webhook_url is not a valid URL")
	}
	if len(c.Targets)+len(c.Deadman) == 0 {
		add("no targets or deadman checks configured")
	}
	if c.Defaults.RepeatInterval < 0 {
		add("defaults.repeat_interval must not be negative")
	}
	if c.Notify.GroupWait < 0 {
		add("notify.group_wait must not be negative")
	}
	for _, s := range c.Notify.SummaryAt {
		if _, err := time.Parse("15:04", s); err != nil {
			add("notify.summary_at: %q is not HH:MM", s)
		}
	}
	if c.Heartbeat.URL != "" {
		if u, err := url.Parse(c.Heartbeat.URL); err != nil || u.Host == "" {
			add("heartbeat.url is not a valid URL")
		}
	}

	seen := map[string]bool{}
	checkName := func(kind, name string) {
		switch {
		case name == "":
			add("%s: name is required", kind)
		case strings.ContainsAny(name, "/ ?#"):
			add("%s %q: name must not contain '/', ' ', '?' or '#'", kind, name)
		case seen[name]:
			add("%s %q: duplicate name", kind, name)
		}
		seen[name] = true
	}

	for _, t := range c.Targets {
		checkName("target", t.Name)
		switch t.Type {
		case "icmp":
			if t.Host == "" {
				add("target %q: host is required", t.Name)
			}
			if t.ICMPCount < 1 {
				add("target %q: icmp_count must be >= 1", t.Name)
			}
		case "tcp":
			if _, _, err := net.SplitHostPort(t.Host); err != nil {
				add("target %q: tcp host must be host:port (%v)", t.Name, err)
			}
		case "http":
			if u, err := url.Parse(t.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("target %q: url must be http(s)://...", t.Name)
			}
		default:
			add("target %q: unknown type %q (icmp, tcp, http)", t.Name, t.Type)
		}
		if t.Interval <= 0 || t.Timeout <= 0 {
			add("target %q: interval and timeout must be positive", t.Name)
		} else if t.Timeout > t.Interval {
			add("target %q: timeout (%s) must not exceed interval (%s)", t.Name, t.Timeout, t.Interval)
		}
		if t.FailThreshold < 1 || t.RecoverThreshold < 1 {
			add("target %q: fail_threshold and recover_threshold must be >= 1", t.Name)
		}
	}

	if len(c.Deadman) > 0 && c.HTTP.Listen == "" {
		add("deadman checks require http.listen")
	}
	for _, d := range c.Deadman {
		checkName("deadman", d.Name)
		if d.Timeout <= 0 {
			add("deadman %q: timeout is required", d.Name)
		}
	}
	return errors.Join(errs...)
}
