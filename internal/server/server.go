package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/konono/aw-manager/internal/chat"
	"github.com/konono/aw-manager/internal/chat/discord"
	chatslack "github.com/konono/aw-manager/internal/chat/slack"
	"github.com/konono/aw-manager/internal/config"
	"github.com/konono/aw-manager/internal/pod"
	"github.com/konono/aw-manager/internal/session"
)

// Server ties together the chat adapter, pod manager, and session store.
type Server struct {
	cfg        *config.Config
	sessions   *session.Store
	podManager *pod.Manager
	adapter    chat.Adapter
	handler    *chat.Handler
	logger     *slog.Logger
}

// New creates a Server with all dependencies initialized.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	sessions, err := session.NewStore(cfg.RedisURL, cfg.IdleTimeout)
	if err != nil {
		return nil, err
	}

	podManager, err := pod.NewManager(cfg, sessions, logger)
	if err != nil {
		_ = sessions.Close()
		return nil, err
	}

	handler := chat.NewHandler(cfg, podManager, sessions, logger)

	adapter, err := buildAdapter(cfg, logger)
	if err != nil {
		_ = sessions.Close()
		return nil, fmt.Errorf("creating chat adapter: %w", err)
	}

	return &Server{
		cfg:        cfg,
		sessions:   sessions,
		podManager: podManager,
		adapter:    adapter,
		handler:    handler,
		logger:     logger,
	}, nil
}

func buildAdapter(cfg *config.Config, logger *slog.Logger) (chat.Adapter, error) {
	switch cfg.ChatAdapter {
	case "slack":
		return chatslack.New(cfg.SlackBotToken, cfg.SlackAppToken, logger), nil
	case "discord":
		return discord.New(cfg.DiscordToken, logger)
	default:
		return nil, fmt.Errorf("unsupported adapter: %s", cfg.ChatAdapter)
	}
}

// Run starts the metrics server, idle cleanup, and chat adapter. Blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.sessions.Ping(r.Context()); err != nil {
			s.logger.Warn("healthz: redis ping failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	metricsServer := &http.Server{
		Addr:    s.cfg.MetricsAddr,
		Handler: mux,
	}

	go func() {
		s.logger.Info("metrics server starting", "addr", s.cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("metrics server error", "error", err)
		}
	}()

	go s.podManager.StartIdleCleanup(ctx)

	s.logger.Info("starting chat adapter", "adapter", s.adapter.Name())
	err := s.adapter.Run(ctx, s.handler.Handle)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = metricsServer.Shutdown(shutdownCtx)
	_ = s.sessions.Close()

	return err
}
