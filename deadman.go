package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type deadmanEntry struct {
	cfg   DeadmanCheck
	th    thresholds
	state probeState

	mu       sync.Mutex
	lastPing time.Time
}

// Deadman は外部から定期的に届く ping が途絶えたことを検知する。
type Deadman struct {
	entries map[string]*deadmanEntry
	order   []*deadmanEntry
	notify  func(Event)
	started time.Time
}

func NewDeadman(cfg *Config, notify func(Event)) *Deadman {
	d := &Deadman{entries: map[string]*deadmanEntry{}, notify: notify, started: time.Now()}
	for _, c := range cfg.Deadman {
		e := &deadmanEntry{cfg: c, th: thresholds{fail: 1, recover: 1, repeat: cfg.Defaults.RepeatInterval}}
		d.entries[c.Name] = e
		d.order = append(d.order, e)
	}
	return d
}

// Ping は name の ping を受け付ける。未知の name なら false。
func (d *Deadman) Ping(name string) bool {
	e, ok := d.entries[name]
	if !ok {
		return false
	}
	now := time.Now()
	e.mu.Lock()
	e.lastPing = now
	e.mu.Unlock()
	d.emit(e, e.state.record(now, 0, nil, e.th))
	return true
}

func (d *Deadman) Run(ctx context.Context) {
	if len(d.order) == 0 {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			for _, e := range d.order {
				if err := d.overdue(e, now); err != nil {
					d.emit(e, e.state.record(now, 0, err, e.th))
				}
			}
		}
	}
}

func (d *Deadman) overdue(e *deadmanEntry, now time.Time) error {
	e.mu.Lock()
	last := e.lastPing
	e.mu.Unlock()
	if last.IsZero() {
		// 起動直後は起動時刻から timeout の猶予を与える
		if now.Sub(d.started) > e.cfg.Timeout {
			return fmt.Errorf("no ping received since startup (timeout %s)", e.cfg.Timeout)
		}
		return nil
	}
	if now.Sub(last) > e.cfg.Timeout {
		return fmt.Errorf("no ping for %s (last ping %s, timeout %s)",
			now.Sub(last).Round(time.Second), last.Format(time.RFC3339), e.cfg.Timeout)
	}
	return nil
}

func (d *Deadman) emit(e *deadmanEntry, ev *Event) {
	if ev == nil {
		return
	}
	ev.Name, ev.Type, ev.Address, ev.Description, ev.Mention = e.cfg.Name, "deadman", "/ping/"+e.cfg.Name, e.cfg.Description, e.cfg.Mention
	logEvent(ev)
	d.notify(*ev)
}

func (d *Deadman) state(name string) *probeState {
	if e, ok := d.entries[name]; ok {
		return &e.state
	}
	return nil
}

func (d *Deadman) Snapshot() []TargetStatus {
	out := make([]TargetStatus, 0, len(d.order))
	for _, e := range d.order {
		ts := e.state.snapshot()
		ts.Name, ts.Type, ts.Address, ts.Description = e.cfg.Name, "deadman", "/ping/"+e.cfg.Name, e.cfg.Description
		out = append(out, ts)
	}
	return out
}
