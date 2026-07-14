package chat

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/konono/aw-manager/internal/session"
)

// Stats tracks internal counters for the /stats command.
var Stats struct {
	MessagesHandled  atomic.Int64
	MessagesRejected atomic.Int64
}

// handleCommand processes a /command message. Returns true if it was a command.
// Works with both Slack and Discord — commands are detected by message prefix.
func (h *Handler) handleCommand(ctx context.Context, msg Message, respond Responder) bool {
	if !strings.HasPrefix(msg.Text, "/") {
		return false
	}

	parts := strings.Fields(msg.Text)
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/clear":
		h.cmdClear(ctx, msg, respond)
	case "/status":
		h.cmdStatus(ctx, msg, respond)
	case "/stats":
		h.cmdStats(ctx, respond)
	case "/help":
		h.cmdHelp(ctx, respond)
	default:
		return false
	}
	return true
}

func (h *Handler) cmdClear(ctx context.Context, msg Message, respond Responder) {
	key := session.SessionKey{
		UserID:    msg.UserID,
		ChannelID: msg.Channel,
	}

	mu := h.podManager.LockKey(key)
	mu.Lock()
	defer mu.Unlock()

	if err := h.podManager.DeleteInstance(ctx, key); err != nil {
		h.logger.Error("failed to clear session", "user", msg.UserID, "channel", msg.Channel, "error", err)
		_ = respond.SendError(ctx, "Failed to clear the session.")
		return
	}

	h.logger.Info("session cleared", "user", msg.UserID, "channel", msg.Channel)
	_ = respond.Send(ctx, "Session cleared. The next message will start a fresh agent.")
}

func (h *Handler) cmdStatus(ctx context.Context, msg Message, respond Responder) {
	key := session.SessionKey{
		UserID:    msg.UserID,
		ChannelID: msg.Channel,
	}

	sess, err := h.sessions.GetSession(ctx, key)
	if err != nil {
		_ = respond.SendError(ctx, "Failed to check session status.")
		return
	}
	if sess == nil {
		_ = respond.Send(ctx, "No active session for this channel. Send a message to start one.")
		return
	}

	lastActive, _ := h.sessions.GetLastActive(ctx, key)
	healthy := h.podManager.IsHealthy(ctx, sess.PodName, sess.Namespace)

	status := "Running"
	if !healthy {
		status = "Unhealthy"
	}

	var idle string
	if !lastActive.IsZero() {
		idle = time.Since(lastActive).Round(time.Second).String()
	} else {
		idle = "unknown"
	}

	text := fmt.Sprintf("```\nPod:       %s\nNamespace: %s\nStatus:    %s\nIdle:      %s\nTool:      %s\nProfile:   %s\n```",
		sess.PodName, sess.Namespace, status, idle, h.cfg.AwTool, h.cfg.AwProfile)

	_ = respond.Send(ctx, text)
}

func (h *Handler) cmdStats(ctx context.Context, respond Responder) {
	text := fmt.Sprintf("```\nMessages Handled:    %d\nMessages Rejected:   %d\nMax Concurrent:      %d\nIdle Timeout:        %s\nAdapter:             %s\n```",
		Stats.MessagesHandled.Load(),
		Stats.MessagesRejected.Load(),
		h.cfg.MaxConcurrent,
		h.cfg.IdleTimeout,
		h.adapterName)

	_ = respond.Send(ctx, text)
}

func (h *Handler) cmdHelp(ctx context.Context, respond Responder) {
	text := "```\n" +
		"Available commands:\n" +
		"  /status  — Show current session and pod info\n" +
		"  /clear   — Delete the agent pod and start fresh\n" +
		"  /stats   — Show server statistics\n" +
		"  /help    — Show this help\n" +
		"```"
	_ = respond.Send(ctx, text)
}
