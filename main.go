package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/konono/aw-manager/internal/cmd"
	"github.com/konono/aw-manager/internal/version"
)

func main() {
	loadDotEnv(".env")

	var cli cmd.CLI
	ctx := kong.Parse(&cli,
		kong.Name("aw-manager"),
		kong.Description("AI agent server for chat platforms"),
		kong.Vars{"version": version.Version},
		kong.UsageOnError(),
	)

	if err := ctx.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// loadDotEnv reads a .env file and sets environment variables that are not already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		val = strings.Trim(val, `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
