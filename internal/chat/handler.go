package chat

import (
	"context"
	"log/slog"
	"time"

	"github.com/konono/aw-manager/internal/config"
	"github.com/konono/aw-manager/internal/metrics"
	"github.com/konono/aw-manager/internal/pod"
	"github.com/konono/aw-manager/internal/session"
)

// Handler processes incoming chat messages by routing them to agent pods.
type Handler struct {
	podManager  *pod.Manager
	sessions    *session.Store
	cfg         *config.Config
	logger      *slog.Logger
	tool        ToolCommander
	adapterName string
	sem         chan struct{}
}

// NewHandler creates a Handler with the given dependencies.
func NewHandler(cfg *config.Config, podManager *pod.Manager, sessions *session.Store, logger *slog.Logger) *Handler {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	return &Handler{
		podManager:  podManager,
		sessions:    sessions,
		cfg:         cfg,
		logger:      logger,
		tool:        LookupTool(cfg.AwTool),
		adapterName: cfg.ChatAdapter,
		sem:         make(chan struct{}, maxConcurrent),
	}
}

// Handle processes a single chat message: ensures a pod exists, executes the tool, and sends the response.
func (h *Handler) Handle(ctx context.Context, msg Message, respond Responder) {
	if msg.Text == "" {
		return
	}

	metrics.MessagesTotal.WithLabelValues(h.adapterName).Inc()

	h.logger.Info("received message",
		"user", msg.UserID,
		"channel", msg.Channel,
		"isThread", msg.IsThread,
	)

	// Limit concurrent handlers to prevent resource exhaustion
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		metrics.MessagesRejected.Inc()
		h.logger.Warn("concurrent handler limit reached", "user", msg.UserID, "channel", msg.Channel)
		_ = respond.SendError(ctx, "The server is currently busy. Please try again in a moment.")
		return
	}

	_ = respond.SendTyping(ctx)

	key := session.SessionKey{
		UserID:    msg.UserID,
		ChannelID: msg.Channel,
	}

	mu := h.podManager.LockKey(key)
	mu.Lock()
	defer mu.Unlock()

	start := time.Now()
	podName, reused, err := h.podManager.EnsurePod(ctx, key)
	if err != nil {
		h.logger.Error("failed to ensure pod", "user", msg.UserID, "channel", msg.Channel, "error", err)
		_ = respond.SendError(ctx, "Failed to prepare the agent. Please try again later.")
		return
	}
	if !reused {
		metrics.PodCreateDuration.Observe(time.Since(start).Seconds())
	}

	// Touch session before exec to prevent idle cleanup from deleting the pod mid-exec
	if err := h.sessions.TouchSession(ctx, key); err != nil {
		h.logger.Warn("failed to touch session before exec", "error", err)
	}

	continueSession := reused
	command := h.tool.PromptCommand(continueSession)

	execStart := time.Now()
	response, err := h.podManager.ExecTool(ctx, podName, h.cfg.AwNamespace, command, msg.Text)
	metrics.ExecDuration.Observe(time.Since(execStart).Seconds())

	if err != nil {
		metrics.ExecTotal.WithLabelValues("error").Inc()
		h.logger.Error("exec failed", "user", msg.UserID, "pod", podName, "error", err)

		if !h.podManager.IsHealthy(ctx, podName, h.cfg.AwNamespace) {
			h.logger.Warn("pod unhealthy, recreating on next message", "user", msg.UserID, "pod", podName)
			_ = h.podManager.DeleteInstance(ctx, key)
		}

		_ = respond.SendError(ctx, "The agent encountered an error. Please try again.")
		return
	}
	metrics.ExecTotal.WithLabelValues("success").Inc()

	if err := h.sessions.TouchSession(ctx, key); err != nil {
		h.logger.Warn("failed to touch session", "error", err)
	}

	if response == "" {
		response = "(no output)"
	}

	if err := respond.Send(ctx, response); err != nil {
		h.logger.Error("failed to send response", "error", err)
	}
}
