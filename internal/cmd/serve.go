package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/konono/aw-manager/internal/config"
	"github.com/konono/aw-manager/internal/server"
)

// Run starts the chat bot server.
func (s *ServeCmd) Run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := s.toConfig()
	if err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	srv, err := server.New(cfg, logger)
	if err != nil {
		return fmt.Errorf("creating server: %w", err)
	}

	logger.Info("aw-manager starting")
	return srv.Run(ctx)
}

func (s *ServeCmd) toConfig() (*config.Config, error) {
	idleTimeout, err := time.ParseDuration(s.IdleTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid idle-timeout %q: %w", s.IdleTimeout, err)
	}

	cfg := &config.Config{
		ChatAdapter:   s.Adapter,
		SlackBotToken: s.SlackBotToken,
		SlackAppToken: s.SlackAppToken,
		DiscordToken:  s.DiscordToken,
		RedisURL:      s.RedisURL,
		AwProfile:     s.AwProfile,
		AwNamespace:   s.AwNamespace,
		AwBinary:      s.AwBinary,
		AwConfigDir:   s.AwConfigDir,
		AwTool:        s.AwTool,
		IdleTimeout:   idleTimeout,
		MaxConcurrent: s.MaxConcurrent,
		MetricsAddr:   s.MetricsAddr,
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
