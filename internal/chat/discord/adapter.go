package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bwmarrin/discordgo"

	"github.com/konono/aw-manager/internal/chat"
)

const maxMessageLen = 2000

// Adapter connects to Discord and dispatches messages from DMs and mentions.
type Adapter struct {
	session      *discordgo.Session
	logger       *slog.Logger
	botUserID    atomic.Value // stored as string; safe for concurrent read/write
	handler      chat.MessageHandler
	ctx          context.Context
	wg           sync.WaitGroup
	ready        chan struct{}
	memberCache  sync.Map // guildID → *discordgo.Member
}

// New creates a Discord adapter with the given bot token.
func New(token string, logger *slog.Logger) (*Adapter, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("creating discord session: %w", err)
	}

	session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	return &Adapter{
		session: session,
		logger:  logger,
		ready:   make(chan struct{}),
	}, nil
}

func (a *Adapter) Name() string { return "discord" }

func (a *Adapter) getBotUserID() string {
	v, _ := a.botUserID.Load().(string)
	return v
}

// Run connects to Discord and blocks until ctx is cancelled.
func (a *Adapter) Run(ctx context.Context, handler chat.MessageHandler) error {
	a.handler = handler
	a.ctx = ctx

	a.session.AddHandler(a.onReady)
	a.session.AddHandler(a.onMessageCreate)

	if err := a.session.Open(); err != nil {
		return fmt.Errorf("opening discord session: %w", err)
	}

	// Wait for Ready event or context cancellation
	select {
	case <-a.ready:
		a.logger.Info("discord bot ready", "botUser", a.getBotUserID())
	case <-ctx.Done():
		return a.session.Close()
	}

	<-ctx.Done()

	a.wg.Wait()
	return a.session.Close()
}

func (a *Adapter) onReady(s *discordgo.Session, r *discordgo.Ready) {
	a.botUserID.Store(r.User.ID)
	a.logger.Info("discord ready event", "botUser", r.User.ID, "username", r.User.Username)
	select {
	case <-a.ready:
	default:
		close(a.ready)
	}
}

func (a *Adapter) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	botID := a.getBotUserID()
	if botID == "" {
		return
	}

	if m.Author == nil {
		return
	}

	a.logger.Info("discord event received",
		"author", m.Author.Username,
		"authorID", m.Author.ID,
		"guildID", m.GuildID,
		"channelID", m.ChannelID,
	)

	if m.Author.ID == botID {
		return
	}
	if m.Author.Bot {
		return
	}

	isDM := m.GuildID == ""
	isMentioned := false
	for _, mention := range m.Mentions {
		if mention.ID == botID {
			isMentioned = true
			break
		}
	}
	if !isMentioned && m.GuildID != "" {
		member, err := a.getBotMember(s, m.GuildID, botID)
		if err == nil {
			for _, roleID := range m.MentionRoles {
				for _, botRole := range member.Roles {
					if roleID == botRole {
						isMentioned = true
						break
					}
				}
				if isMentioned {
					break
				}
			}
		}
	}

	if !isDM && !isMentioned {
		return
	}

	text := a.stripMention(m.Content, m.MentionRoles, botID)
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	isThread := m.MessageReference != nil
	threadID := m.ID
	if isThread {
		threadID = m.MessageReference.MessageID
	}

	msg := chat.Message{
		UserID:   m.Author.ID,
		Channel:  m.ChannelID,
		Text:     text,
		ThreadID: threadID,
		IsThread: isThread,
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.handler(a.ctx, msg, a.newResponder(m.ChannelID, m.ID))
	}()
}

// getBotMember returns the bot's guild member, cached per guild to avoid API rate limits.
func (a *Adapter) getBotMember(s *discordgo.Session, guildID, botID string) (*discordgo.Member, error) {
	if v, ok := a.memberCache.Load(guildID); ok {
		return v.(*discordgo.Member), nil
	}
	member, err := s.GuildMember(guildID, botID)
	if err != nil {
		return nil, err
	}
	a.memberCache.Store(guildID, member)
	return member, nil
}

func (a *Adapter) stripMention(text string, mentionRoles []string, botID string) string {
	mention := fmt.Sprintf("<@%s>", botID)
	mentionNick := fmt.Sprintf("<@!%s>", botID)
	text = strings.ReplaceAll(text, mention, "")
	text = strings.ReplaceAll(text, mentionNick, "")
	for _, roleID := range mentionRoles {
		text = strings.ReplaceAll(text, fmt.Sprintf("<@&%s>", roleID), "")
	}
	return strings.TrimSpace(text)
}

func (a *Adapter) newResponder(channelID, replyToID string) *responder {
	return &responder{session: a.session, channelID: channelID, replyToID: replyToID, logger: a.logger}
}

type responder struct {
	session   *discordgo.Session
	channelID string
	replyToID string
	logger    *slog.Logger
}

func (r *responder) Send(ctx context.Context, text string) error {
	chunks := chat.SplitMessage(text, maxMessageLen)
	for _, chunk := range chunks {
		_, err := r.session.ChannelMessageSendReply(r.channelID, chunk, &discordgo.MessageReference{
			MessageID: r.replyToID,
			ChannelID: r.channelID,
		})
		if err != nil {
			r.logger.Error("failed to send discord message", "error", err)
			return err
		}
	}
	return nil
}

func (r *responder) SendError(ctx context.Context, errMsg string) error {
	_, err := r.session.ChannelMessageSendReply(r.channelID, "⚠️ "+errMsg, &discordgo.MessageReference{
		MessageID: r.replyToID,
		ChannelID: r.channelID,
	})
	return err
}

func (r *responder) SendTyping(ctx context.Context) error {
	return r.session.ChannelTyping(r.channelID)
}
