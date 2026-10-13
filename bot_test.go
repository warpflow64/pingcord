package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestAckSuppressesRepeatAndIsReportedOnRecovery(t *testing.T) {
	th := thresholds{fail: 1, recover: 1, repeat: time.Minute}
	var s probeState
	now := time.Now()
	down := s.record(now, 0, errors.New("x"), th)
	since := down.DownSince.Unix()

	if err := s.ack(since-1, "<@1>", now); !errors.Is(err, errStaleAlert) {
		t.Fatalf("want stale, got %v", err)
	}
	if err := s.ack(since, "<@1>", now); err != nil {
		t.Fatal(err)
	}
	var already *alreadyAckedError
	if err := s.ack(since, "<@2>", now); !errors.As(err, &already) || already.by != "<@1>" {
		t.Fatalf("want already acked by <@1>, got %v", err)
	}
	if ev := s.record(now.Add(2*time.Minute), 0, errors.New("x"), th); ev != nil {
		t.Fatalf("acked alert must not repeat, got %v", ev.Kind)
	}
	ev := s.record(now.Add(3*time.Minute), time.Millisecond, nil, th)
	if ev == nil || ev.Kind != EventRecovered || ev.AckedBy != "<@1>" {
		t.Fatalf("recovery should carry ack, got %+v", ev)
	}
	if err := s.ack(since, "<@1>", now); !errors.Is(err, errNotDown) {
		t.Fatalf("want not down, got %v", err)
	}

	// 次のダウンでは ack がリセットされ、再通知も復活する
	now = now.Add(time.Hour)
	s.record(now, 0, errors.New("x"), th)
	if ev := s.record(now.Add(2*time.Minute), 0, errors.New("x"), th); ev == nil || ev.Kind != EventStillDown {
		t.Fatal("new outage should repeat again")
	}
}

func TestAckIDRoundTrip(t *testing.T) {
	id := ackID(alertRef{name: "14f-ap-01", downSince: 1791905879})
	name, since, ok := parseAckID(id)
	if !ok || name != "14f-ap-01" || since != 1791905879 {
		t.Fatalf("got %q %d %v", name, since, ok)
	}
	if len(id) > 100 {
		t.Fatal("custom_id must be <= 100 chars")
	}
}

func TestAckComponentsAndSetButtons(t *testing.T) {
	var alerts []alertRef
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "ab"} {
		alerts = append(alerts, alertRef{name: n, downSince: 1})
	}
	comps := ackComponents(alerts)
	if len(comps) != 2 || len(comps[0].(*discordgo.ActionsRow).Components) != 5 {
		t.Fatalf("want 2 rows (5+2), got %d", len(comps))
	}
	if !setButtons(comps, "a", "done", discordgo.SuccessButton) {
		t.Fatal("expected change")
	}
	a := comps[0].(*discordgo.ActionsRow).Components[0].(*discordgo.Button)
	ab := comps[1].(*discordgo.ActionsRow).Components[1].(*discordgo.Button)
	if !a.Disabled || a.Label != "done" || ab.Disabled {
		t.Fatal("only the button for target \"a\" (not \"ab\") should change")
	}
}

func TestBotConfigValidation(t *testing.T) {
	p := writeConfig(t, `
discord:
  bot_token: x
  ack_roles: ["abc"]
targets:
  - {name: a, host: 10.0.0.1}
`)
	_, err := loadConfig(p)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"channel_id is required", "not a numeric role ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
