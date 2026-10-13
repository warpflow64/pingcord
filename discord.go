package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	colorRed    = 0xE74C3C
	colorOrange = 0xE67E22
	colorGreen  = 0x2ECC71
	colorBlue   = 0x3498DB
	colorGrey   = 0x95A5A6

	maxEmbedsPerMessage = 10
	// 回線断などで Discord に届かない間もしばらく再送を続ける
	maxRetryElapsed = 15 * time.Minute
	// 停止要求後、残りのメッセージを送るために待つ最大時間 (podman stop の猶予 10s 以内)
	shutdownGrace = 8 * time.Second
)

// message は送信するメッセージ。Webhook / Bot どちらの送信方式でも共通で使う。
type message struct {
	Content string
	Embeds  []embed
	// Bot モード用: ack ボタンを付ける DOWN / STILL DOWN と、ボタンを閉じる復旧済みの対象
	alerts    []alertRef
	recovered []string
}

type alertRef struct {
	name      string
	downSince int64
}

// transport は 1 回分の送信を行う。retryAfter が 0 以外ならその時間待ってから再送する。
type transport interface {
	send(ctx context.Context, m message) (retryAfter time.Duration, err error)
}

type embed struct {
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Color       int          `json:"color"`
	Fields      []embedField `json:"fields,omitempty"`
	Timestamp   string       `json:"timestamp,omitempty"`
	Footer      *embedFooter `json:"footer,omitempty"`
}

type embedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type embedFooter struct {
	Text string `json:"text"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
}

type webhookBody struct {
	Content         string           `json:"content,omitempty"`
	Username        string           `json:"username,omitempty"`
	AvatarURL       string           `json:"avatar_url,omitempty"`
	Embeds          []embed          `json:"embeds,omitempty"`
	AllowedMentions *allowedMentions `json:"allowed_mentions,omitempty"`
}

// queued はイベント (まとめて送る) か、そのまま送るメッセージのどちらか。
type queued struct {
	event   *Event
	payload *message
}

// Notifier は Discord への送信を 1 goroutine に集約し、順序を保ったまま
// group_wait の間に発生したイベントを 1 メッセージにまとめて送る。
type Notifier struct {
	tr        transport
	groupWait time.Duration
	queue     chan queued
	done      chan struct{}
}

func NewNotifier(tr transport, groupWait time.Duration) *Notifier {
	return &Notifier{
		tr:        tr,
		groupWait: groupWait,
		queue:     make(chan queued, 1024),
		done:      make(chan struct{}),
	}
}

func (n *Notifier) Notify(ev Event) { n.enqueue(queued{event: &ev}) }

func (n *Notifier) Post(p message) { n.enqueue(queued{payload: &p}) }

func (n *Notifier) enqueue(q queued) {
	select {
	case n.queue <- q:
	default:
		slog.Error("notification queue full, dropping message")
	}
}

// Wait は Run が残りのメッセージを送り終えるまで待つ。
func (n *Notifier) Wait() { <-n.done }

// Run は ctx がキャンセルされるとキューに残ったものを送ってから終了する。
// 送信 (再送を含む) は ctx のキャンセル後も shutdownGrace の間だけ続ける。
func (n *Notifier) Run(ctx context.Context) {
	defer close(n.done)
	sendCtx, cancelSend := context.WithCancel(context.Background())
	defer cancelSend()
	defer context.AfterFunc(ctx, func() { time.AfterFunc(shutdownGrace, cancelSend) })()

	var pending []Event
	var timer <-chan time.Time

	handle := func(q queued) {
		if q.event != nil {
			pending = append(pending, *q.event)
			if n.groupWait == 0 {
				n.flush(sendCtx, &pending)
			} else if timer == nil {
				timer = time.After(n.groupWait)
			}
			return
		}
		n.flush(sendCtx, &pending)
		if err := n.Send(sendCtx, *q.payload); err != nil {
			slog.Error("discord send failed", "err", err)
		}
	}

	for {
		select {
		case q := <-n.queue:
			handle(q)
		case <-timer:
			timer = nil
			n.flush(sendCtx, &pending)
		case <-ctx.Done():
			for {
				select {
				case q := <-n.queue:
					handle(q)
					continue
				default:
				}
				break
			}
			n.flush(sendCtx, &pending)
			return
		}
	}
}

func (n *Notifier) flush(ctx context.Context, pending *[]Event) {
	events := *pending
	*pending = nil
	for len(events) > 0 {
		chunk := events[:min(maxEmbedsPerMessage, len(events))]
		events = events[len(chunk):]
		if err := n.Send(ctx, eventsPayload(chunk)); err != nil {
			slog.Error("discord send failed", "err", err, "events", len(chunk))
		}
	}
}

func eventsPayload(events []Event) message {
	var p message
	var mentions []string
	counts := map[EventKind]int{}
	for _, ev := range events {
		counts[ev.Kind]++
		p.Embeds = append(p.Embeds, eventEmbed(ev))
		if ev.Kind == EventRecovered {
			p.recovered = append(p.recovered, ev.Name)
		} else {
			p.alerts = append(p.alerts, alertRef{name: ev.Name, downSince: ev.DownSince.Unix()})
		}
		if ev.Kind != EventRecovered && ev.Mention != "" && !slices.Contains(mentions, ev.Mention) {
			mentions = append(mentions, ev.Mention)
		}
	}
	var parts []string
	if len(mentions) > 0 {
		parts = append(parts, strings.Join(mentions, " "))
	}
	if len(events) > 1 {
		var c []string
		if v := counts[EventDown]; v > 0 {
			c = append(c, fmt.Sprintf("🔴 DOWN %d", v))
		}
		if v := counts[EventStillDown]; v > 0 {
			c = append(c, fmt.Sprintf("🟠 STILL DOWN %d", v))
		}
		if v := counts[EventRecovered]; v > 0 {
			c = append(c, fmt.Sprintf("🟢 RECOVERED %d", v))
		}
		parts = append(parts, strings.Join(c, " / "))
	}
	p.Content = strings.Join(parts, "\n")
	return p
}

func eventEmbed(ev Event) embed {
	e := embed{
		Description: ev.Description,
		Timestamp:   ev.At.UTC().Format(time.RFC3339),
		Fields: []embedField{
			{Name: "種別", Value: ev.Type, Inline: true},
			{Name: "宛先", Value: "`" + ev.Address + "`", Inline: true},
		},
	}
	switch ev.Kind {
	case EventDown:
		e.Title = "🔴 DOWN: " + ev.Name
		e.Color = colorRed
		e.Fields = append(e.Fields, embedField{Name: "検知開始", Value: discordTime(ev.DownSince)})
	case EventStillDown:
		e.Title = "🟠 STILL DOWN: " + ev.Name
		e.Color = colorOrange
		e.Fields = append(e.Fields,
			embedField{Name: "ダウン継続", Value: humanDuration(ev.At.Sub(ev.DownSince)), Inline: true},
			embedField{Name: "検知開始", Value: discordTime(ev.DownSince), Inline: true})
	case EventRecovered:
		e.Title = "🟢 RECOVERED: " + ev.Name
		e.Color = colorGreen
		e.Fields = append(e.Fields,
			embedField{Name: "ダウン時間", Value: humanDuration(ev.At.Sub(ev.DownSince)), Inline: true},
			embedField{Name: "ダウン開始", Value: discordTime(ev.DownSince), Inline: true})
		if ev.RTT > 0 {
			e.Fields = append(e.Fields, embedField{Name: "応答時間", Value: ev.RTT.Round(10 * time.Microsecond).String(), Inline: true})
		}
		if ev.AckedBy != "" {
			e.Fields = append(e.Fields, embedField{Name: "Ack", Value: ev.AckedBy, Inline: true})
		}
		return e
	}
	if ev.Err != "" {
		e.Fields = append(e.Fields, embedField{Name: "エラー", Value: "```" + truncate(ev.Err, 900) + "```"})
	}
	return e
}

// Send は Discord にメッセージを送る。429 や 5xx、通信エラーの場合はバックオフして再送する。
func (n *Notifier) Send(ctx context.Context, m message) error {
	start := time.Now()
	backoff := 2 * time.Second
	for attempt := 1; ; attempt++ {
		wait, err := n.tr.send(ctx, m)
		if err == nil {
			return nil
		}
		var perm permanentError
		if errors.As(err, &perm) || time.Since(start) > maxRetryElapsed {
			return err
		}
		if wait == 0 {
			wait = backoff
			backoff = min(backoff*2, time.Minute)
		}
		slog.Warn("discord send failed, retrying", "attempt", attempt, "wait", wait, "err", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (gave up: %v)", err, ctx.Err())
		case <-time.After(wait):
		}
	}
}

type permanentError struct{ error }

type webhookTransport struct {
	cfg    DiscordConfig
	client *http.Client
}

func newWebhookTransport(cfg DiscordConfig) *webhookTransport {
	return &webhookTransport{cfg: cfg, client: &http.Client{Timeout: 15 * time.Second}}
}

func (w *webhookTransport) send(ctx context.Context, m message) (retryAfter time.Duration, err error) {
	body, err := json.Marshal(webhookBody{
		Content:         m.Content,
		Username:        w.cfg.Username,
		AvatarURL:       w.cfg.AvatarURL,
		Embeds:          m.Embeds,
		AllowedMentions: &allowedMentions{Parse: []string{"users", "roles", "everyone"}},
	})
	if err != nil {
		return 0, permanentError{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return 0, permanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pingcord/"+version)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode < 300:
		return 0, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(respBody, &rl) == nil && rl.RetryAfter > 0 {
			retryAfter = time.Duration(rl.RetryAfter*float64(time.Second)) + 100*time.Millisecond
		} else if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			retryAfter = time.Duration(s) * time.Second
		}
		return retryAfter, fmt.Errorf("discord rate limited")
	case resp.StatusCode >= 500:
		return 0, fmt.Errorf("discord returned %s", resp.Status)
	}
	return 0, permanentError{fmt.Errorf("discord returned %s: %s", resp.Status, truncate(string(respBody), 300))}
}

// discordTime は閲覧者のローカルタイムで表示される Discord のタイムスタンプ記法を返す。
func discordTime(t time.Time) string {
	return fmt.Sprintf("<t:%d:f> (<t:%d:R>)", t.Unix(), t.Unix())
}

func humanDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
