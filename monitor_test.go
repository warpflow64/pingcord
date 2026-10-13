package main

import (
	"errors"
	"testing"
	"time"
)

func TestProbeStateTransitions(t *testing.T) {
	th := thresholds{fail: 3, recover: 2, repeat: 10 * time.Minute}
	var s probeState
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fail := errors.New("timeout")
	step := func(err error) *Event {
		now = now.Add(30 * time.Second)
		return s.record(now, time.Millisecond, err, th)
	}

	if ev := step(nil); ev != nil || s.status != StatusUp {
		t.Fatalf("first success should be silent UP, got %v %v", ev, s.status)
	}
	firstFail := now.Add(30 * time.Second)
	if step(fail) != nil || step(fail) != nil {
		t.Fatal("should not alert before fail_threshold")
	}
	ev := step(fail)
	if ev == nil || ev.Kind != EventDown || !ev.DownSince.Equal(firstFail) {
		t.Fatalf("expected DOWN since first failure, got %+v", ev)
	}
	if step(fail) != nil {
		t.Fatal("should not repeat before repeat_interval")
	}
	if step(nil) != nil {
		t.Fatal("one success must not recover with recover_threshold=2")
	}
	if step(fail) != nil {
		t.Fatal("flap while down must not re-alert")
	}
	for i := 0; i < 20; i++ {
		if ev := step(fail); ev != nil {
			if ev.Kind != EventStillDown {
				t.Fatalf("expected STILL DOWN, got %v", ev.Kind)
			}
			break
		}
		if i == 19 {
			t.Fatal("expected STILL DOWN after repeat_interval")
		}
	}
	step(nil)
	ev = step(nil)
	if ev == nil || ev.Kind != EventRecovered || !ev.DownSince.Equal(firstFail) || ev.Err != "timeout" {
		t.Fatalf("expected RECOVERED, got %+v", ev)
	}
}

func TestDownAtStartup(t *testing.T) {
	var s probeState
	th := thresholds{fail: 2, recover: 1}
	now := time.Now()
	if s.record(now, 0, errors.New("x"), th) != nil {
		t.Fatal("unexpected event")
	}
	if ev := s.record(now, 0, errors.New("x"), th); ev == nil || ev.Kind != EventDown {
		t.Fatal("target down at startup should alert")
	}
}

func TestNextSummary(t *testing.T) {
	loc := time.FixedZone("JST", 9*3600)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, loc)
	got := nextSummary(now, []string{"09:00", "18:00"})
	if want := time.Date(2026, 10, 14, 18, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got = nextSummary(now, []string{"09:00"})
	if want := time.Date(2026, 10, 15, 9, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
