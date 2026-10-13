package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type Status int

const (
	StatusUnknown Status = iota
	StatusUp
	StatusDown
)

func (s Status) String() string {
	switch s {
	case StatusUp:
		return "up"
	case StatusDown:
		return "down"
	}
	return "unknown"
}

type EventKind int

const (
	EventDown EventKind = iota
	EventStillDown
	EventRecovered
)

type Event struct {
	Kind        EventKind
	Name        string
	Type        string
	Address     string
	Description string
	Mention     string
	Err         string
	At          time.Time
	DownSince   time.Time
	RTT         time.Duration
	AckedBy     string
}

type thresholds struct {
	fail, recover int
	repeat        time.Duration
}

// probeState は 1 監視対象の状態遷移を管理する。
type probeState struct {
	mu           sync.Mutex
	status       Status
	failStreak   int
	okStreak     int
	firstFail    time.Time
	downSince    time.Time
	lastNotified time.Time
	lastErr      string
	lastRTT      time.Duration
	lastCheck    time.Time
	ackedBy      string
	ackedAt      time.Time
}

var (
	errUnknownTarget = errors.New("unknown target")
	errNotDown       = errors.New("target is not down")
	errStaleAlert    = errors.New("alert is for an earlier outage")
)

type alreadyAckedError struct {
	by string
	at time.Time
}

func (e *alreadyAckedError) Error() string { return fmt.Sprintf("already acked by %s", e.by) }

// ack は DOWN 中のアラートを確認済みにし、復旧するまで STILL DOWN の再通知を止める。
// downSince でどのダウンに対する ack かを確認し、古い通知のボタンで新しいダウンを ack しないようにする。
func (s *probeState) ack(downSince int64, by string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusDown {
		return errNotDown
	}
	if s.downSince.Unix() != downSince {
		return errStaleAlert
	}
	if s.ackedBy != "" {
		return &alreadyAckedError{by: s.ackedBy, at: s.ackedAt}
	}
	s.ackedBy, s.ackedAt = by, now
	return nil
}

// record は 1 回のチェック結果を反映し、通知が必要なら Event (対象情報は未設定) を返す。
func (s *probeState) record(now time.Time, rtt time.Duration, err error, th thresholds) *Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCheck = now

	if err == nil {
		s.lastRTT = rtt
		s.failStreak = 0
		s.okStreak++
		switch s.status {
		case StatusUnknown:
			// 起動直後に生きている対象は通知せず UP とする
			s.status = StatusUp
			s.lastErr = ""
		case StatusDown:
			if s.okStreak >= th.recover {
				s.status = StatusUp
				ev := &Event{Kind: EventRecovered, At: now, DownSince: s.downSince, RTT: rtt, Err: s.lastErr, AckedBy: s.ackedBy}
				s.lastErr = ""
				s.ackedBy, s.ackedAt = "", time.Time{}
				return ev
			}
		default:
			s.lastErr = ""
		}
		return nil
	}

	s.lastErr = err.Error()
	s.okStreak = 0
	if s.failStreak == 0 {
		s.firstFail = now
	}
	s.failStreak++
	switch s.status {
	case StatusDown:
		if th.repeat > 0 && s.ackedBy == "" && now.Sub(s.lastNotified) >= th.repeat {
			s.lastNotified = now
			return &Event{Kind: EventStillDown, At: now, DownSince: s.downSince, Err: s.lastErr}
		}
	default:
		if s.failStreak >= th.fail {
			s.status = StatusDown
			s.downSince = s.firstFail
			s.lastNotified = now
			return &Event{Kind: EventDown, At: now, DownSince: s.downSince, Err: s.lastErr}
		}
	}
	return nil
}

type TargetStatus struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Address     string     `json:"address"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	DownSince   *time.Time `json:"down_since,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	RTTMillis   float64    `json:"rtt_ms,omitempty"`
	LastCheck   *time.Time `json:"last_check,omitempty"`
	AckedBy     string     `json:"acked_by,omitempty"`
}

func (s *probeState) snapshot() TargetStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts := TargetStatus{Status: s.status.String(), LastError: s.lastErr, AckedBy: s.ackedBy}
	if s.status == StatusDown {
		t := s.downSince
		ts.DownSince = &t
	}
	if s.status == StatusUp {
		ts.RTTMillis = float64(s.lastRTT.Microseconds()) / 1000
	}
	if !s.lastCheck.IsZero() {
		t := s.lastCheck
		ts.LastCheck = &t
	}
	return ts
}

type monitored struct {
	target Target
	probe  prober
	th     thresholds
	state  probeState
}

type Monitor struct {
	targets []*monitored
	notify  func(Event)
}

func NewMonitor(cfg *Config, notify func(Event)) *Monitor {
	m := &Monitor{notify: notify}
	for _, t := range cfg.Targets {
		m.targets = append(m.targets, &monitored{
			target: t,
			probe:  newProber(&t),
			th:     thresholds{fail: t.FailThreshold, recover: t.RecoverThreshold, repeat: t.RepeatInterval},
		})
	}
	return m
}

func (m *Monitor) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i, mt := range m.targets {
		// 起動直後に全対象へ一斉にプローブしないよう開始時刻をずらす
		stagger := min(mt.target.Interval, 5*time.Second) * time.Duration(i) / time.Duration(len(m.targets))
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.loop(ctx, mt, stagger)
		}()
	}
	wg.Wait()
}

func (m *Monitor) loop(ctx context.Context, mt *monitored, stagger time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(stagger):
	}
	ticker := time.NewTicker(mt.target.Interval)
	defer ticker.Stop()
	for {
		m.check(ctx, mt)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Monitor) check(ctx context.Context, mt *monitored) {
	t := &mt.target
	rtt, err := mt.probe(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		slog.Debug("probe failed", "target", t.Name, "err", err)
	} else {
		slog.Debug("probe ok", "target", t.Name, "rtt", rtt)
	}
	ev := mt.state.record(time.Now(), rtt, err, mt.th)
	if ev == nil {
		return
	}
	ev.Name, ev.Type, ev.Address, ev.Description, ev.Mention = t.Name, t.Type, t.Address(), t.Description, t.Mention
	logEvent(ev)
	m.notify(*ev)
}

func (m *Monitor) Snapshot() []TargetStatus {
	out := make([]TargetStatus, 0, len(m.targets))
	for _, mt := range m.targets {
		ts := mt.state.snapshot()
		ts.Name, ts.Type, ts.Address, ts.Description = mt.target.Name, mt.target.Type, mt.target.Address(), mt.target.Description
		out = append(out, ts)
	}
	return out
}

func (m *Monitor) state(name string) *probeState {
	for _, mt := range m.targets {
		if mt.target.Name == name {
			return &mt.state
		}
	}
	return nil
}

// Registry は ping 監視と deadman をまとめて名前で引けるようにする (/status と ack 用)。
type Registry struct {
	monitor *Monitor
	deadman *Deadman
}

func (r *Registry) Snapshot() []TargetStatus {
	return append(r.monitor.Snapshot(), r.deadman.Snapshot()...)
}

func (r *Registry) Ack(name string, downSince int64, by string) error {
	st := r.monitor.state(name)
	if st == nil {
		st = r.deadman.state(name)
	}
	if st == nil {
		return errUnknownTarget
	}
	if err := st.ack(downSince, by, time.Now()); err != nil {
		return err
	}
	slog.Info("alert acked", "target", name, "by", by)
	return nil
}

func logEvent(ev *Event) {
	switch ev.Kind {
	case EventDown:
		slog.Warn("target down", "target", ev.Name, "address", ev.Address, "err", ev.Err)
	case EventStillDown:
		slog.Warn("target still down", "target", ev.Name, "since", ev.DownSince)
	case EventRecovered:
		slog.Info("target recovered", "target", ev.Name, "downtime", ev.At.Sub(ev.DownSince).Round(time.Second))
	}
}
