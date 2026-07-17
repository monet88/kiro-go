package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
)

// narrationLineRe matches a complete standalone narration line:
// [Called <name> with args: {...}]
// Name is non-empty, non-whitespace-leading; args must start with '{'.
var narrationLineRe = regexp.MustCompile(`(?s)^\[Called\s+(\S+)\s+with\s+args:\s*(\{.*\})\s*\]$`)

const narrationCapBytes = 64 * 1024

// tryParseCompleteNarrationLine returns name + raw JSON object text when line is
// a complete narration form. Surrounding whitespace is trimmed by the caller.
func tryParseCompleteNarrationLine(line string) (name, argsJSON string, ok bool) {
	line = strings.TrimSpace(line)
	m := narrationLineRe.FindStringSubmatch(line)
	if m == nil {
		return "", "", false
	}
	name = m[1]
	argsJSON = m[2]
	// Must already be valid JSON object — no repair for narration.
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil || obj == nil {
		return "", "", false
	}
	return name, argsJSON, true
}

// fenceTracker tracks whether plain text is currently inside a fenced code block.
type fenceTracker struct {
	inFence bool
}

// feedLines splits text into complete lines + remainder, updating fence state.
// Incomplete trailing line (no newline) is returned as rest.
func splitCompleteLines(text string) (lines []string, rest string) {
	if text == "" {
		return nil, ""
	}
	parts := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		// last empty from split is discarded; all parts except final empty are lines with implicit NL
		// Actually Split("a\n") => ["a", ""]; we want lines=["a"] rest=""
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		return parts, ""
	}
	if len(parts) == 1 {
		return nil, parts[0]
	}
	return parts[:len(parts)-1], parts[len(parts)-1]
}

func (f *fenceTracker) lineInFence(line string) (inside bool) {
	trim := strings.TrimSpace(line)
	if strings.HasPrefix(trim, "```") {
		f.inFence = !f.inFence
		// The fence line itself is never a narration candidate.
		return true
	}
	return f.inFence
}

// looksLikeInlineCode reports crude inline-code wrapping for a whole line.
func looksLikeInlineCode(line string) bool {
	trim := strings.TrimSpace(line)
	return strings.HasPrefix(trim, "`") && strings.HasSuffix(trim, "`") && len(trim) >= 2
}
