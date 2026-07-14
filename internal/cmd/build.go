package cmd

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/konono/aw-manager/internal/version"
)

// Run builds the container image locally and optionally pushes it.
func (b *BuildCmd) Run() error {
	tag := b.Tag
	if tag == "" {
		tag = version.Version
	}

	image := fmt.Sprintf("aw-manager:%s", tag)
	if b.Registry != "" {
		image = fmt.Sprintf("%s/aw-manager:%s", b.Registry, tag)
	}

	runtime := detectRuntime()

	fmt.Fprintf(os.Stderr, "Building %s with %s...\n", image, runtime)
	buildCmd := exec.Command(runtime, "build", "-t", image, "-f", "Dockerfile", ".")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	if b.Push {
		if b.Registry == "" {
			return fmt.Errorf("--push requires --registry")
		}
		fmt.Fprintf(os.Stderr, "Pushing %s...\n", image)
		pushCmd := exec.Command(runtime, "push", image)
		pushCmd.Stdout = os.Stdout
		pushCmd.Stderr = os.Stderr
		if err := pushCmd.Run(); err != nil {
			return fmt.Errorf("push failed: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "Done: %s\n", image)
	return nil
}

func detectRuntime() string {
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	return "docker"
}
