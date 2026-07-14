package chat

// ToolCommander builds exec commands for a specific AI coding tool.
type ToolCommander interface {
	// PromptCommand returns the command args. The message is always passed via stdin.
	PromptCommand(continueSession bool) []string
}

// LookupTool returns a ToolCommander for the given tool name.
func LookupTool(name string) ToolCommander {
	switch name {
	case "codex":
		return codexTool{}
	case "opencode":
		return opencodeTool{}
	case "cursor":
		return cursorTool{}
	default:
		return claudeTool{}
	}
}

type claudeTool struct{}

func (claudeTool) PromptCommand(continueSession bool) []string {
	cmd := []string{"claude", "-p", "--permission-mode", "bypassPermissions", "--output-format", "text"}
	if continueSession {
		cmd = append(cmd, "--continue")
	}
	return cmd
}

type codexTool struct{}

func (codexTool) PromptCommand(continueSession bool) []string {
	return []string{"codex", "-q", "-a", "never"}
}

type opencodeTool struct{}

func (opencodeTool) PromptCommand(continueSession bool) []string {
	return []string{"opencode", "-p"}
}

type cursorTool struct{}

func (cursorTool) PromptCommand(continueSession bool) []string {
	cmd := []string{"agent", "-p", "--force", "--approve-mcps"}
	if continueSession {
		cmd = append(cmd, "--continue")
	}
	return cmd
}
