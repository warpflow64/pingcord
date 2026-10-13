package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExampleConfig(t *testing.T) {
	t.Setenv("DISCORD_WEBHOOK_URL", "https://discord.com/api/webhooks/1/abc")
	t.Setenv("PINGCORD_TOKEN", "secret")
	cfg, err := loadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Discord.WebhookURL != "https://discord.com/api/webhooks/1/abc" || cfg.HTTP.Token != "secret" {
		t.Fatal("env vars not expanded")
	}
	for _, tg := range cfg.Targets {
		if tg.Interval == 0 || tg.FailThreshold == 0 {
			t.Fatalf("defaults not applied to %s", tg.Name)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	p := writeConfig(t, `
discord:
  webhook_url: https://example.com/hook
targets:
  - name: a
    host: 10.0.0.1
  - name: a
    type: tcp
    host: 10.0.0.2
  - name: c
    url: ftp://x
deadman:
  - name: d
`)
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"duplicate name", "host:port", "url must be", "require http.listen", "timeout is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestMentionResolution(t *testing.T) {
	p := writeConfig(t, `
discord:
  webhook_url: https://example.com/hook
  mention: "<@&1>"
targets:
  - {name: a, host: 10.0.0.1}
  - {name: b, host: 10.0.0.2, mention: none}
  - {name: c, host: 10.0.0.3, mention: "<@2>"}
`)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{cfg.Targets[0].Mention, cfg.Targets[1].Mention, cfg.Targets[2].Mention}
	if got[0] != "<@&1>" || got[1] != "" || got[2] != "<@2>" {
		t.Fatalf("got %q", got)
	}
}
