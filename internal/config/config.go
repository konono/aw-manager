package config

import (
	"fmt"
	"time"
)

// Config holds the server configuration.
type Config struct {
	ChatAdapter   string
	SlackBotToken string
	SlackAppToken string
	DiscordToken  string
	RedisURL      string
	AwProfile     string
	AwNamespace   string
	AwBinary      string
	AwConfigDir   string
	AwTool        string
	IdleTimeout   time.Duration
	MaxConcurrent int
	MetricsAddr   string
}

// Validate checks that required fields are set for the selected adapter.
func (c *Config) Validate() error {
	switch c.ChatAdapter {
	case "slack":
		if c.SlackBotToken == "" {
			return fmt.Errorf("SLACK_BOT_TOKEN is required for slack adapter")
		}
		if c.SlackAppToken == "" {
			return fmt.Errorf("SLACK_APP_TOKEN is required for slack adapter")
		}
	case "discord":
		if c.DiscordToken == "" {
			return fmt.Errorf("DISCORD_TOKEN is required for discord adapter")
		}
	default:
		return fmt.Errorf("unsupported CHAT_ADAPTER %q (supported: slack, discord)", c.ChatAdapter)
	}
	return nil
}
