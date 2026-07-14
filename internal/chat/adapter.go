package chat

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Message represents a chat message received from any platform.
type Message struct {
	UserID   string
	Channel  string
	Text     string
	ThreadID string
	IsThread bool
}

// MessageHandler is a callback invoked when a new message arrives.
type MessageHandler func(ctx context.Context, msg Message, respond Responder)

// Responder sends replies back to the originating chat platform.
type Responder interface {
	Send(ctx context.Context, text string) error
	SendError(ctx context.Context, errMsg string) error
	SendTyping(ctx context.Context) error
}

// Adapter connects to a chat platform and dispatches incoming messages.
type Adapter interface {
	Name() string
	Run(ctx context.Context, handler MessageHandler) error
}

// SplitMessage splits text into chunks that fit within maxLen bytes,
// splitting on rune boundaries to avoid corrupting multi-byte UTF-8.
func SplitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find a split point at or before maxLen bytes on a rune boundary
		splitIdx := maxLen
		for splitIdx > 0 && !utf8.RuneStart(text[splitIdx]) {
			splitIdx--
		}
		if splitIdx == 0 {
			splitIdx = maxLen
		}

		// Prefer splitting at a newline within the safe range
		if nlIdx := strings.LastIndex(text[:splitIdx], "\n"); nlIdx > splitIdx/2 {
			splitIdx = nlIdx
		}

		chunks = append(chunks, text[:splitIdx])
		text = strings.TrimLeft(text[splitIdx:], "\n")
	}
	return chunks
}
