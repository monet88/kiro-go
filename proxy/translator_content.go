package proxy

import (
	"encoding/base64"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Precompiled regexes used on the request hot path (avoid recompiling per call).
var (
	imagePlaceholderPattern = regexp.MustCompile(`\[Image\s+\d+\]`)
	dataURLPattern          = regexp.MustCompile(`^data:image/([a-zA-Z0-9+.-]+)(;[a-zA-Z0-9=._:+-]+)*;base64,(.+)$`)
)

// truncateRunes truncates s to at most n runes, never splitting a multi-byte
// character (which byte slicing s[:n] would do, producing invalid UTF-8).
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func buildToolResultsContinuation(toolResults []KiroToolResult) string {
	if len(toolResults) == 0 {
		return minimalFallbackUserContent
	}

	parts := make([]string, 0, len(toolResults))
	for _, tr := range toolResults {
		if len(tr.Content) == 0 {
			continue
		}
		for _, c := range tr.Content {
			if strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
	}

	if len(parts) == 0 {
		return minimalFallbackUserContent
	}

	joined := toolResultsContinuationPrefix + "\n\n" + strings.Join(parts, "\n\n")
	return truncateRunes(joined, 4000)
}

func trimLeadingAssistantHistory(history []KiroHistoryMessage) []KiroHistoryMessage {
	idx := 0
	for idx < len(history) && history[idx].AssistantResponseMessage != nil {
		idx++
	}
	if idx == 0 {
		return history
	}
	if idx >= len(history) {
		return nil
	}
	return history[idx:]
}

func buildConversationID(modelID, systemPrompt, anchor string) string {
	anchor = strings.TrimSpace(anchor)
	if isSyntheticConversationAnchor(anchor) {
		return uuid.New().String()
	}
	seed := strings.Join([]string{modelID, strings.TrimSpace(systemPrompt), anchor}, "\n")
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
}

func isSyntheticConversationAnchor(anchor string) bool {
	if strings.TrimSpace(anchor) == "" {
		return true
	}

	normalized := strings.ToLower(strings.Join(strings.Fields(anchor), " "))
	switch normalized {
	case ".", "begin conversation", "please analyze the attached image.", strings.ToLower(minimalFallbackUserContent):
		return true
	default:
		return false
	}
}

func sanitizeImagePlaceholders(text string) string {
	cleaned := imagePlaceholderPattern.ReplaceAllString(text, "")
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	return strings.TrimSpace(cleaned)
}

func normalizeUserContent(text string, hasImages bool) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" && hasImages {
		return "Please analyze the attached image."
	}
	return trimmed
}

func parseDataURL(url string) *KiroImage {
	cleaned := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(url, "\n", ""), "\r", ""))
	if strings.Contains(cleaned, "[Image") {
		return nil
	}
	// dataURLPattern has 3 capture groups, so a match yields exactly 4 elements
	// (full match + 3 groups); anything else means no match.
	matches := dataURLPattern.FindStringSubmatch(cleaned)
	if len(matches) != 4 {
		return nil
	}
	return parseBase64Image(matches[3], matches[1])
}

func parseBase64Image(data, format string) *KiroImage {
	format = strings.ToLower(format)
	if format == "jpg" {
		format = "jpeg"
	}

	// 验证 base64
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		if _, errRaw := base64.RawStdEncoding.DecodeString(data); errRaw != nil {
			if _, errURL := base64.URLEncoding.DecodeString(data); errURL != nil {
				if _, errRawURL := base64.RawURLEncoding.DecodeString(data); errRawURL != nil {
					return nil
				}
			}
		}
	}

	if format == "" {
		format = "png"
	}

	return &KiroImage{
		Format: format,
		Source: struct {
			Bytes string `json:"bytes"`
		}{Bytes: data},
	}
}

// extractThinkingFromContent separates real <thinking>...</thinking> reasoning
// blocks from the plain assistant text. It uses the same quote/fence/blockquote
// aware tag detection as the streaming thinkingSplitter, so a literal
// `<thinking>` inside inline code, a code fence, quotes, or a Markdown
// blockquote is left in the output instead of being mistaken for a reasoning
// boundary (the naive strings.Index approach would strip the rest as thinking).
func extractThinkingFromContent(content string) (string, string) {
	if findRealThinkingStartTag(content, 0) == -1 {
		return strings.TrimSpace(content), ""
	}

	var out strings.Builder
	var reasoning strings.Builder
	pos := 0
	for pos < len(content) {
		start := findRealThinkingStartTag(content, pos)
		if start == -1 {
			out.WriteString(content[pos:])
			break
		}
		if start > pos {
			out.WriteString(content[pos:start])
		}
		end := findRealThinkingEndTag(content, start+len(thinkingStartTag))
		if end == -1 {
			// Unterminated real start tag: keep the remainder as plain text so
			// no output is silently dropped.
			out.WriteString(content[start:])
			break
		}
		reasoning.WriteString(content[start+len(thinkingStartTag) : end])
		pos = end + len(thinkingEndTag)
		// A real thinking block is followed by "\n\n"; consume it so it does not
		// leak into the plain text.
		if strings.HasPrefix(content[pos:], "\n\n") {
			pos += len("\n\n")
		}
	}

	return strings.TrimSpace(out.String()), reasoning.String()
}
