package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const ackPrefix = "ack:"

// Bot は Bot トークンで通知を送り、/status コマンドと ack ボタンを処理する。
// 送信は REST、コマンドとボタンは Gateway (外向き WebSocket) で受け取るため、
// VyOS 側でポートを開ける必要はない。
type Bot struct {
	s   *discordgo.Session
	cfg DiscordConfig
	reg *Registry

	mu sync.Mutex
	// 対象ごとの、ack ボタンを付けて送ったメッセージ ID (復旧時にボタンを閉じ、返信先にする)
	alertMsgs map[string][]string
}

func NewBot(cfg DiscordConfig, reg *Registry) (*Bot, error) {
	s, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		return nil, err
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	s.UserAgent = "DiscordBot (https://github.com/warpflow64/pingcord, " + version + ")"
	b := &Bot{s: s, cfg: cfg, reg: reg, alertMsgs: map[string][]string{}}
	s.AddHandler(b.onInteraction)
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		slog.Info("discord gateway connected", "user", r.User.Username)
	})
	return b, nil
}

// CheckToken はトークンが無効なら error を返す。Discord に届かないだけなら起動は続ける。
func (b *Bot) CheckToken(ctx context.Context) error {
	u, err := b.s.User("@me", discordgo.WithContext(ctx))
	var re *discordgo.RESTError
	switch {
	case err == nil:
		slog.Info("discord bot authenticated", "user", u.Username, "channel", b.cfg.ChannelID)
	case errors.As(err, &re) && re.Response != nil && re.Response.StatusCode == 401:
		return fmt.Errorf("discord bot token is invalid: %w", err)
	default:
		slog.Warn("could not verify discord bot token, continuing", "err", err)
	}
	return nil
}

// Run は Gateway に接続して /status を登録し、ctx が終わるまで待つ。
// 起動時に Discord へ届かなくても監視は止めず、接続できるまで再試行する。
func (b *Bot) Run(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		err := b.s.Open()
		if err == nil {
			break
		}
		slog.Warn("discord gateway connect failed, retrying", "err", err, "wait", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Minute)
	}
	defer b.s.Close()

	if err := b.registerCommands(ctx); err != nil {
		slog.Error("register /status command failed", "err", err)
	}
	<-ctx.Done()
}

func (b *Bot) registerCommands(ctx context.Context) error {
	app, err := b.s.Application("@me")
	if err != nil {
		return err
	}
	cmds := []*discordgo.ApplicationCommand{{
		Name:        "status",
		Description: "監視対象の現在の状態を表示します",
		Contexts:    &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild},
		Options: []*discordgo.ApplicationCommandOption{{
			Type:        discordgo.ApplicationCommandOptionBoolean,
			Name:        "share",
			Description: "チャンネルの全員に見えるように表示する (既定は自分だけ)",
		}},
	}}
	_, err = b.s.ApplicationCommandBulkOverwrite(app.ID, b.cfg.GuildID, cmds, discordgo.WithContext(ctx))
	if err == nil {
		slog.Info("registered /status command", "guild", b.cfg.GuildID)
	}
	return err
}

func (b *Bot) send(ctx context.Context, m message) (time.Duration, error) {
	msg := &discordgo.MessageSend{
		Content: m.Content,
		Embeds:  toDiscordEmbeds(m.Embeds),
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{
			discordgo.AllowedMentionTypeUsers, discordgo.AllowedMentionTypeRoles, discordgo.AllowedMentionTypeEveryone,
		}},
		Components: ackComponents(m.alerts),
	}
	// 1 件だけの復旧通知は元の DOWN 通知への返信にして、流れを追いやすくする
	if len(m.recovered) == 1 {
		b.mu.Lock()
		if ids := b.alertMsgs[m.recovered[0]]; len(ids) > 0 {
			fail := false
			msg.Reference = &discordgo.MessageReference{MessageID: ids[0], ChannelID: b.cfg.ChannelID, FailIfNotExists: &fail}
		}
		b.mu.Unlock()
	}

	sent, err := b.s.ChannelMessageSendComplex(b.cfg.ChannelID, msg, discordgo.WithContext(ctx))
	if err != nil {
		var re *discordgo.RESTError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode >= 400 && re.Response.StatusCode < 500 && re.Response.StatusCode != 429 {
			return 0, permanentError{err}
		}
		return 0, err
	}

	closed := map[string][]string{}
	b.mu.Lock()
	for _, a := range m.alerts {
		b.alertMsgs[a.name] = append(b.alertMsgs[a.name], sent.ID)
	}
	for _, name := range m.recovered {
		closed[name] = b.alertMsgs[name]
		delete(b.alertMsgs, name)
	}
	b.mu.Unlock()

	for name, ids := range closed {
		for _, id := range ids {
			b.updateButtons(ctx, id, name, "🟢 復旧: "+name, discordgo.SecondaryButton)
		}
	}
	return 0, nil
}

// updateButtons は送信済みメッセージ内の対象の ack ボタンを無効化してラベルを変える。
func (b *Bot) updateButtons(ctx context.Context, msgID, name, label string, style discordgo.ButtonStyle) {
	m, err := b.s.ChannelMessage(b.cfg.ChannelID, msgID, discordgo.WithContext(ctx))
	if err != nil {
		slog.Warn("fetch alert message failed", "message", msgID, "err", err)
		return
	}
	comps := m.Components
	if !setButtons(comps, name, label, style) {
		return
	}
	_, err = b.s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: msgID, Channel: b.cfg.ChannelID, Components: &comps}, discordgo.WithContext(ctx))
	if err != nil {
		slog.Warn("update alert message failed", "message", msgID, "err", err)
	}
}

func (b *Bot) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		if i.ApplicationCommandData().Name == "status" {
			b.handleStatus(i)
		}
	case discordgo.InteractionMessageComponent:
		if id := i.MessageComponentData().CustomID; strings.HasPrefix(id, ackPrefix) {
			b.handleAck(i, id)
		}
	}
}

func (b *Bot) handleStatus(i *discordgo.InteractionCreate) {
	share := false
	for _, o := range i.ApplicationCommandData().Options {
		if o.Name == "share" {
			share = o.BoolValue()
		}
	}
	data := &discordgo.InteractionResponseData{
		Embeds:          toDiscordEmbeds(statusPayload(b.reg.Snapshot(), "📊 現在の状態").Embeds),
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}
	if !share {
		data.Flags = discordgo.MessageFlagsEphemeral
	}
	b.respond(i, discordgo.InteractionResponseChannelMessageWithSource, data)
}

func (b *Bot) handleAck(i *discordgo.InteractionCreate, customID string) {
	name, since, ok := parseAckID(customID)
	user := i.User
	if i.Member != nil {
		user = i.Member.User
	}
	if !ok || user == nil {
		return
	}
	if !b.canAck(i.Member) {
		b.ephemeral(i, "ack する権限がありません。")
		return
	}

	err := b.reg.Ack(name, since, "<@"+user.ID+">")
	var already *alreadyAckedError
	switch {
	case err == nil:
		who := user.DisplayName()
		if i.Member != nil {
			who = i.Member.DisplayName()
		}
		comps := i.Message.Components
		setButtons(comps, name, "✅ "+name+" — "+who, discordgo.SuccessButton)
		// 更新レスポンスでは content / embeds を渡さないと消えるため、元の内容をそのまま返す
		b.respond(i, discordgo.InteractionResponseUpdateMessage, &discordgo.InteractionResponseData{
			Content:    i.Message.Content,
			Embeds:     i.Message.Embeds,
			Components: comps,
		})
	case errors.As(err, &already):
		b.ephemeral(i, fmt.Sprintf("**%s** は %s が <t:%d:R> に ack 済みです。", name, already.by, already.at.Unix()))
	case errors.Is(err, errNotDown):
		b.ephemeral(i, fmt.Sprintf("**%s** はすでに復旧しています。", name))
	case errors.Is(err, errStaleAlert):
		b.ephemeral(i, fmt.Sprintf("この通知は **%s** の以前のダウンのものです。最新の通知のボタンを使ってください。", name))
	default:
		b.ephemeral(i, "ack できませんでした: "+err.Error())
	}
}

func (b *Bot) canAck(m *discordgo.Member) bool {
	if len(b.cfg.AckRoles) == 0 {
		return true
	}
	if m == nil {
		return false
	}
	return slices.ContainsFunc(m.Roles, func(r string) bool { return slices.Contains(b.cfg.AckRoles, r) })
}

func (b *Bot) ephemeral(i *discordgo.InteractionCreate, content string) {
	b.respond(i, discordgo.InteractionResponseChannelMessageWithSource, &discordgo.InteractionResponseData{
		Content:         content,
		Flags:           discordgo.MessageFlagsEphemeral,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
}

func (b *Bot) respond(i *discordgo.InteractionCreate, typ discordgo.InteractionResponseType, data *discordgo.InteractionResponseData) {
	if err := b.s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: typ, Data: data}); err != nil {
		slog.Warn("interaction respond failed", "err", err)
	}
}

func ackID(a alertRef) string {
	return fmt.Sprintf("%s%s:%d", ackPrefix, a.name, a.downSince)
}

func parseAckID(id string) (name string, downSince int64, ok bool) {
	rest := strings.TrimPrefix(id, ackPrefix)
	sep := strings.LastIndex(rest, ":")
	if sep <= 0 {
		return "", 0, false
	}
	downSince, err := strconv.ParseInt(rest[sep+1:], 10, 64)
	return rest[:sep], downSince, err == nil
}

// ackComponents は DOWN / STILL DOWN ごとに ack ボタンを作る (1 行 5 個まで)。
func ackComponents(alerts []alertRef) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	var row *discordgo.ActionsRow
	for _, a := range alerts {
		if row == nil || len(row.Components) == 5 {
			row = &discordgo.ActionsRow{}
			rows = append(rows, row)
		}
		row.Components = append(row.Components, &discordgo.Button{
			Label:    truncate("✋ Ack: "+a.name, 80),
			Style:    discordgo.DangerButton,
			CustomID: ackID(a),
		})
	}
	return rows
}

// setButtons は対象の ack ボタンを無効化してラベルとスタイルを変える。変更があれば true。
func setButtons(comps []discordgo.MessageComponent, name, label string, style discordgo.ButtonStyle) bool {
	changed := false
	for _, c := range comps {
		row, ok := c.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, rc := range row.Components {
			btn, ok := rc.(*discordgo.Button)
			if !ok || !strings.HasPrefix(btn.CustomID, ackPrefix+name+":") {
				continue
			}
			btn.Label, btn.Style, btn.Disabled = truncate(label, 80), style, true
			changed = true
		}
	}
	return changed
}

// toDiscordEmbeds は embed (Discord API と同じ JSON 形) を discordgo の型に変換する。
func toDiscordEmbeds(embeds []embed) []*discordgo.MessageEmbed {
	raw, _ := json.Marshal(embeds)
	var out []*discordgo.MessageEmbed
	json.Unmarshal(raw, &out)
	return out
}
