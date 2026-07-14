package cmd

import "github.com/alecthomas/kong"

// CLI is the top-level kong grammar.
type CLI struct {
	Version kong.VersionFlag `name:"version" help:"Show version."`

	Serve  ServeCmd  `cmd:"" default:"withargs" help:"Start the server (default command)."`
	Deploy   DeployCmd   `cmd:"" help:"Deploy aw-manager to a Kubernetes cluster."`
	Cleanup CleanupCmd `cmd:"" help:"Remove aw-manager and agent pods from Kubernetes."`
	Build    BuildCmd    `cmd:"" help:"Build the container image locally."`
}

// ServeCmd starts the chat bot server.
type ServeCmd struct {
	Adapter       string `name:"adapter" env:"CHAT_ADAPTER" help:"Chat adapter (slack or discord)." default:"slack" enum:"slack,discord"`
	SlackBotToken string `name:"slack-bot-token" env:"SLACK_BOT_TOKEN" help:"Slack bot token (required for slack adapter)."`
	SlackAppToken string `name:"slack-app-token" env:"SLACK_APP_TOKEN" help:"Slack app token (required for slack adapter)."`
	DiscordToken  string `name:"discord-token" env:"DISCORD_TOKEN" help:"Discord bot token (required for discord adapter)."`
	RedisURL      string `name:"redis-url" env:"REDIS_URL" help:"Redis URL." default:"redis://localhost:6379"`
	AwProfile     string `name:"aw-profile" env:"AW_PROFILE" help:"aw profile for agent pods." default:"claude-k8s"`
	AwNamespace   string `name:"aw-namespace" env:"AW_NAMESPACE" help:"Namespace for agent pods." default:"aw"`
	AwBinary      string `name:"aw-binary" env:"AW_BINARY" help:"Path to aw binary." default:"aw"`
	AwConfigDir   string `name:"aw-config-dir" env:"AW_CONFIG_DIR" help:"Directory containing .aw.yml."`
	AwTool        string `name:"aw-tool" env:"AW_TOOL" help:"AI tool to use." default:"claude" enum:"claude,codex,opencode,cursor"`
	IdleTimeout    string `name:"idle-timeout" env:"IDLE_TIMEOUT" help:"Idle timeout for agent pods." default:"1h"`
	MaxConcurrent  int    `name:"max-concurrent" env:"MAX_CONCURRENT" help:"Maximum concurrent message handlers." default:"10"`
	MetricsAddr    string `name:"metrics-addr" env:"METRICS_ADDR" help:"Metrics server listen address." default:":9090"`
}

// DeployCmd deploys aw-manager and its dependencies to Kubernetes.
type DeployCmd struct {
	Adapter       string `name:"adapter" env:"CHAT_ADAPTER" required:"" enum:"slack,discord" help:"Chat adapter (slack or discord)."`
	SlackBotToken string `name:"slack-bot-token" env:"SLACK_BOT_TOKEN" help:"Slack bot token (required for slack adapter)."`
	SlackAppToken string `name:"slack-app-token" env:"SLACK_APP_TOKEN" help:"Slack app token (required for slack adapter)."`
	DiscordToken  string `name:"discord-token" env:"DISCORD_TOKEN" help:"Discord bot token (required for discord adapter)."`
	Image         string `name:"image" env:"AW_MANAGER_IMAGE" help:"aw-manager container image." default:"ghcr.io/konono/aw-manager:latest"`
	AwProfile     string `name:"aw-profile" env:"AW_PROFILE" help:"aw profile for agent pods." default:"claude-k8s"`
	AwNamespace   string `name:"aw-namespace" env:"AW_NAMESPACE" help:"Namespace for agent pods." default:"aw"`
	Namespace     string `name:"namespace" env:"AW_SYSTEM_NAMESPACE" help:"Namespace for aw-manager itself." default:"aw-system"`
	RedisURL      string `name:"redis-url" env:"REDIS_URL" help:"External Redis URL. If empty, Redis is deployed alongside."`
	IdleTimeout   string `name:"idle-timeout" env:"IDLE_TIMEOUT" help:"Idle timeout for agent pods." default:"1h"`
	AwTool        string `name:"aw-tool" env:"AW_TOOL" help:"AI tool to use." default:"claude" enum:"claude,codex,opencode,cursor"`
	MaxConcurrent int               `name:"max-concurrent" env:"MAX_CONCURRENT" help:"Maximum concurrent message handlers." default:"10"`
	AwConfig      string            `name:"aw-config" env:"AW_CONFIG" help:"Path to .aw.yml to mount in the server pod." type:"existingfile"`
	Env           map[string]string `name:"env" help:"Extra env vars to pass to the server pod (KEY=VAL, repeatable)."`
	SecretFiles   []string          `name:"secret-file" help:"Mount a host file as a secret (src:mountPath[:ENV_VAR], repeatable)."`
}

// CleanupCmd removes aw-manager and all agent resources from Kubernetes.
type CleanupCmd struct {
	AwNamespace string `name:"aw-namespace" env:"AW_NAMESPACE" help:"Namespace for agent pods." default:"aw"`
	Namespace   string `name:"namespace" env:"AW_SYSTEM_NAMESPACE" help:"Namespace for aw-manager itself." default:"aw-system"`
	All         bool   `name:"all" help:"Also delete the namespaces themselves."`
}

// BuildCmd builds the container image locally.
type BuildCmd struct {
	Push     bool   `name:"push" help:"Push the image after building."`
	Registry string `name:"registry" env:"AW_MANAGER_REGISTRY" help:"Registry to push to (e.g. ghcr.io/konono)." placeholder:"REGISTRY"`
	Tag      string `name:"tag" help:"Image tag (defaults to version)."`
}
