package chat

import (
	"strings"
	"testing"
)

func TestLookupTool_Default(t *testing.T) {
	tool := LookupTool("claude")
	cmd := tool.PromptCommand(false)
	if cmd[0] != "claude" {
		t.Errorf("expected claude, got %s", cmd[0])
	}
	if !contains(cmd, "--permission-mode") {
		t.Error("expected --permission-mode flag")
	}
}

func TestLookupTool_Continue(t *testing.T) {
	tool := LookupTool("claude")
	cmd := tool.PromptCommand(true)
	if !contains(cmd, "--continue") {
		t.Error("expected --continue flag when continueSession=true")
	}
}

func TestLookupTool_NoContinueWhenFalse(t *testing.T) {
	tool := LookupTool("claude")
	cmd := tool.PromptCommand(false)
	if contains(cmd, "--continue") {
		t.Error("expected no --continue flag when continueSession=false")
	}
}

func TestLookupTool_Codex(t *testing.T) {
	tool := LookupTool("codex")
	cmd := tool.PromptCommand(false)
	if cmd[0] != "codex" {
		t.Errorf("expected codex, got %s", cmd[0])
	}
}

func TestLookupTool_Unknown(t *testing.T) {
	tool := LookupTool("unknown")
	cmd := tool.PromptCommand(false)
	if cmd[0] != "claude" {
		t.Errorf("unknown tool should default to claude, got %s", cmd[0])
	}
}

func contains(args []string, target string) bool {
	for _, a := range args {
		if strings.Contains(a, target) {
			return true
		}
	}
	return false
}
