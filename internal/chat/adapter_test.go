package chat

import (
	"strings"
	"testing"
)

func TestSplitMessage_Short(t *testing.T) {
	chunks := SplitMessage("hello", 100)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected single chunk 'hello', got %v", chunks)
	}
}

func TestSplitMessage_ExactLimit(t *testing.T) {
	msg := strings.Repeat("a", 100)
	chunks := SplitMessage(msg, 100)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestSplitMessage_SplitsAtNewline(t *testing.T) {
	msg := strings.Repeat("a", 60) + "\n" + strings.Repeat("b", 60)
	chunks := SplitMessage(msg, 100)
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	if chunks[0] != strings.Repeat("a", 60) {
		t.Errorf("first chunk mismatch: %q", chunks[0])
	}
}

func TestSplitMessage_MultiByte(t *testing.T) {
	msg := strings.Repeat("あ", 200) // 3 bytes each = 600 bytes
	chunks := SplitMessage(msg, 100)
	for i, chunk := range chunks {
		if !isValidUTF8(chunk) {
			t.Errorf("chunk %d is invalid UTF-8", i)
		}
	}
	joined := strings.Join(chunks, "")
	if joined != msg {
		t.Errorf("joined chunks don't match original (len %d vs %d)", len(joined), len(msg))
	}
}

func TestSplitMessage_InvalidUTF8NoInfiniteLoop(t *testing.T) {
	// All continuation bytes — would cause infinite loop without the splitIdx==0 guard
	msg := string([]byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80})
	chunks := SplitMessage(msg, 4)
	if len(chunks) == 0 {
		t.Error("expected at least one chunk")
	}
}

func isValidUTF8(s string) bool {
	for i := 0; i < len(s); {
		r, size := rune(s[i]), 1
		if s[i] < 0x80 {
			// ASCII
		} else if s[i]&0xE0 == 0xC0 {
			size = 2
		} else if s[i]&0xF0 == 0xE0 {
			size = 3
		} else if s[i]&0xF8 == 0xF0 {
			size = 4
		} else {
			return false
		}
		if i+size > len(s) {
			return false
		}
		_ = r
		i += size
	}
	return true
}
