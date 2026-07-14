package slack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/konono/aw-manager/internal/chat"
)

const maxMessageLen = 4000

// Adapter connects to Slack via Socket Mode and dispatches messages.
type Adapter struct {
	api       *slack.Client
	socket    *socketmode.Client
	logger    *slog.Logger
	botUserID string
	wg        sync.WaitGroup
}

// New creates a Slack adapter with the given bot and app tokens.
func New(botToken, appToken string, logger *slog.Logger) *Adapter {
	api := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)
	socket := socketmode.New(api)

	return &Adapter{
		api:    api,
		socket: socket,
		logger: logger,
	}
}

func (a *Adapter) Name() string { return "slack" }

func (a *Adapter) Run(ctx context.Context, handler chat.MessageHandler) error {
	authResp, err := a.api.AuthTest()
	if err != nil {
		return fmt.Errorf("slack auth test: %w", err)
	}
	a.botUserID = authResp.UserID
	a.logger.Info("slack bot connected", "botUser", a.botUserID, "team", authResp.Team)

	go a.handleEvents(ctx, handler)

	err = a.socket.RunContext(ctx)
	a.wg.Wait()
	return err
}

func (a *Adapter) handleEvents(ctx context.Context, handler chat.MessageHandler) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-a.socket.Events:
			switch event.Type {
			case socketmode.EventTypeEventsAPI:
				a.handleEventsAPI(ctx, event, handler)
			case socketmode.EventTypeConnecting:
				a.logger.Info("slack connecting...")
			case socketmode.EventTypeConnected:
				a.logger.Info("slack connected")
			case socketmode.EventTypeConnectionError:
				a.logger.Error("slack connection error")
			}
		}
	}
}

func (a *Adapter) handleEventsAPI(ctx context.Context, event socketmode.Event, handler chat.MessageHandler) {
	eventsAPIEvent, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	a.socket.Ack(*event.Request)

	switch ev := eventsAPIEvent.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		msg := chat.Message{
			UserID:   ev.User,
			Channel:  ev.Channel,
			Text:     a.stripMention(ev.Text),
			ThreadID: ev.ThreadTimeStamp,
			IsThread: ev.ThreadTimeStamp != "",
		}
		if msg.ThreadID == "" {
			msg.ThreadID = ev.TimeStamp
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			handler(ctx, msg, a.newResponder(ev.Channel, msg.ThreadID))
		}()

	case *slackevents.MessageEvent:
		if ev.ChannelType == "im" && ev.BotID == "" && ev.SubType == "" {
			threadID := ev.ThreadTimeStamp
			isThread := threadID != ""
			if threadID == "" {
				threadID = ev.TimeStamp
			}
			msg := chat.Message{
				UserID:   ev.User,
				Channel:  ev.Channel,
				Text:     strings.TrimSpace(ev.Text),
				ThreadID: threadID,
				IsThread: isThread,
			}
			a.wg.Add(1)
			go func() {
				defer a.wg.Done()
				handler(ctx, msg, a.newResponder(ev.Channel, threadID))
			}()
		}
	}
}

func (a *Adapter) stripMention(text string) string {
	mention := fmt.Sprintf("<@%s>", a.botUserID)
	return strings.TrimSpace(strings.ReplaceAll(text, mention, ""))
}

func (a *Adapter) newResponder(channel, threadTS string) *responder {
	return &responder{api: a.api, channel: channel, threadTS: threadTS, logger: a.logger}
}

type responder struct {
	api      *slack.Client
	channel  string
	threadTS string
	logger   *slog.Logger
}

func (r *responder) Send(ctx context.Context, text string) error {
	chunks := chat.SplitMessage(text, maxMessageLen)
	for _, chunk := range chunks {
		_, _, err := r.api.PostMessageContext(ctx, r.channel,
			slack.MsgOptionText(chunk, false),
			slack.MsgOptionTS(r.threadTS),
		)
		if err != nil {
			r.logger.Error("failed to post message", "error", err)
			return err
		}
	}
	return nil
}

func (r *responder) SendError(ctx context.Context, errMsg string) error {
	_, _, err := r.api.PostMessageContext(ctx, r.channel,
		slack.MsgOptionText(":warning: "+errMsg, false),
		slack.MsgOptionTS(r.threadTS),
	)
	return err
}

func (r *responder) SendTyping(ctx context.Context) error {
	// Slack has no ephemeral typing indicator API. Posting a real message
	// would leave a permanent "Processing..." in the thread, so we skip it.
	return nil
}

