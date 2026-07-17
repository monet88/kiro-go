package proxy

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// thinkingStartTag / thinkingEndTag are the inline markers the upstream emits
// around implicit reasoning when a reasoningContentEvent is not sent (e.g. Opus
// 4.7/4.8 with thinking enabled). Real thinking blocks are wrapped as
// `<thinking>...</thinking>\n\n`.
const (
	thinkingStartTag = "<thinking>"
	thinkingEndTag   = "</thinking>"
)

// thinkingSplitter separates plain assistant text from inline
// <thinking>...</thinking> blocks in a streamed token feed.
//
// It uses quote/fence/blockquote-aware tag detection (ported from the team's
// sub2api parser) so a literal `<thinking>` inside a code fence, inline code,
// quotes, or a Markdown blockquote is NOT mistaken for a real reasoning
// boundary — the naive strings.Index approach used previously would flip into
// thinking mode on those literals and drop the rest of the message.
//
// It also keeps a safe UTF-8 byte tail buffered so a tag split across two chunks
// is still detected on the next push, and only closes a thinking block on
// `</thinking>` followed by `\n\n` during streaming (the real end-tag shape),
// deferring whitespace-terminated closes to flush.
//
// The splitter is output-format agnostic: callers wire the four callbacks to
// their own rendering (OpenAI chunks, Claude blocks) and own source arbitration.
type thinkingSplitter struct {
	buffer          string
	inThinkingBlock bool
	stripLeadingNL  bool

	// onPlain receives a run of plain assistant text (never empty).
	onPlain func(text string)
	// onOpen fires when a real <thinking> tag opens a block. Fires before any
	// onThinking for that block, so callers can resolve source arbitration.
	onOpen func()
	// onThinking receives reasoning text inside an open block (never empty).
	onThinking func(text string)
	// onClose fires when the block ends (</thinking> or flush boundary).
	onClose func()
}

// push feeds a chunk of assistant text through the splitter.
func (s *thinkingSplitter) push(text string) {
	if text == "" {
		return
	}
	s.buffer += text
	for {
		if !s.inThinkingBlock {
			start := findRealThinkingStartTag(s.buffer, 0)
			if start != -1 {
				if before := s.buffer[:start]; strings.TrimSpace(before) != "" {
					s.onPlain(before)
				}
				s.inThinkingBlock = true
				s.stripLeadingNL = true
				s.buffer = s.buffer[start+len(thinkingStartTag):]
				if s.onOpen != nil {
					s.onOpen()
				}
				continue
			}
			// No start tag yet: emit everything except a trailing tag-prefix
			// (kept so a <thinking> tag split across chunks is detected next
			// push). Plain text with no partial tag flows through contiguously.
			keep := tagPrefixTailLen(s.buffer, thinkingStartTag)
			if emitLen := len(s.buffer) - keep; emitLen > 0 {
				if safe := s.buffer[:emitLen]; strings.TrimSpace(safe) != "" {
					s.onPlain(safe)
					s.buffer = s.buffer[emitLen:]
				}
			}
			break
		}

		if s.stripLeadingNL {
			if strings.HasPrefix(s.buffer, "\n") {
				s.buffer = s.buffer[1:]
				s.stripLeadingNL = false
			} else if s.buffer != "" {
				s.stripLeadingNL = false
			}
		}

		end := findStreamThinkingEndTagStrict(s.buffer, 0)
		if end != -1 {
			if t := s.buffer[:end]; t != "" {
				s.onThinking(t)
			}
			s.inThinkingBlock = false
			if s.onClose != nil {
				s.onClose()
			}
			s.buffer = s.buffer[end+len(thinkingEndTag)+len("\n\n"):]
			continue
		}
		// No strict end tag yet: emit thinking except a trailing prefix of
		// `</thinking>\n\n` (kept so a real close split across chunks is still
		// detected; a literal </thinking> not followed by \n\n stays reasoning).
		keep := tagPrefixTailLen(s.buffer, thinkingEndTag+"\n\n")
		if emitLen := len(s.buffer) - keep; emitLen > 0 {
			s.onThinking(s.buffer[:emitLen])
			s.buffer = s.buffer[emitLen:]
		}
		break
	}
}

// flush drains the remaining buffer at a boundary (tool use or stream EOF).
// Inside a thinking block it accepts a whitespace-terminated (not just \n\n)
// end tag, then closes; leftover buffer is emitted as plain text or thinking.
func (s *thinkingSplitter) flush() {
	if s.buffer == "" && !s.inThinkingBlock {
		return
	}
	if s.inThinkingBlock {
		end := findStreamThinkingEndTagAtBufferEnd(s.buffer, 0)
		if end != -1 {
			if t := s.buffer[:end]; t != "" {
				s.onThinking(t)
			}
			remaining := strings.TrimLeftFunc(s.buffer[end+len(thinkingEndTag):], unicode.IsSpace)
			s.buffer = ""
			s.inThinkingBlock = false
			if s.onClose != nil {
				s.onClose()
			}
			if remaining != "" {
				s.onPlain(remaining)
			}
			return
		}
		if s.buffer != "" {
			s.onThinking(s.buffer)
		}
		s.buffer = ""
		s.inThinkingBlock = false
		if s.onClose != nil {
			s.onClose()
		}
		return
	}
	if s.buffer != "" {
		s.onPlain(s.buffer)
	}
	s.buffer = ""
}

// --- Tag detection (ported verbatim from sub2api backend/internal/pkg/kiro) ---

func findRealThinkingStartTag(content string, from int) int {
	return findRealThinkingTag(content, thinkingStartTag, from, false)
}

// findRealThinkingEndTag finds a closing tag that is followed by `\n\n` or is at
// end of content (whitespace only after). Used for non-streaming extraction.
func findRealThinkingEndTag(content string, from int) int {
	searchFrom := from
	for {
		pos := findRealThinkingTag(content, thinkingEndTag, searchFrom, true)
		if pos == -1 {
			return -1
		}
		after := pos + len(thinkingEndTag)
		if strings.HasPrefix(content[after:], "\n\n") || strings.TrimSpace(content[after:]) == "" {
			return pos
		}
		searchFrom = pos + 1
	}
}

// findStreamThinkingEndTagStrict requires `\n\n` after the closing tag so a
// literal `</thinking>` mid-sentence during streaming does not close the block.
func findStreamThinkingEndTagStrict(content string, from int) int {
	searchFrom := from
	for {
		pos := findRealThinkingTag(content, thinkingEndTag, searchFrom, true)
		if pos == -1 {
			return -1
		}
		after := pos + len(thinkingEndTag)
		if strings.HasPrefix(content[after:], "\n\n") {
			return pos
		}
		searchFrom = pos + 1
	}
}

// findStreamThinkingEndTagAtBufferEnd accepts a closing tag with only whitespace
// after it (used at flush/EOF where the trailing `\n\n` may be absent).
func findStreamThinkingEndTagAtBufferEnd(content string, from int) int {
	searchFrom := from
	for {
		pos := findRealThinkingTag(content, thinkingEndTag, searchFrom, true)
		if pos == -1 {
			return -1
		}
		after := pos + len(thinkingEndTag)
		if strings.TrimSpace(content[after:]) == "" {
			return pos
		}
		searchFrom = pos + 1
	}
}

// tagPrefixTailLen returns how many trailing bytes of content must stay buffered
// because they form a non-empty proper prefix of tag (a partial tag that could
// complete on the next chunk). It returns 0 when no suffix of content begins a
// tag, so plain text flows through contiguously instead of being fragmented by
// a fixed-size tail. The returned length never starts inside a UTF-8 rune.
//
// Example: content ending in "...done<thi" and tag "<thinking>" returns 4 (the
// "<thi" is held back); content ending in "...all done." returns 0.
func tagPrefixTailLen(content, tag string) int {
	// Longest k such that the last k bytes of content equal the first k bytes of
	// tag. Only proper prefixes matter (a full tag would already have matched).
	max := len(tag) - 1
	if max > len(content) {
		max = len(content)
	}
	for k := max; k > 0; k-- {
		if strings.HasSuffix(content, tag[:k]) {
			// Never hold back a partial rune: if the kept tail starts mid-rune,
			// the tag bytes are all ASCII so this only guards multi-byte content
			// bytes immediately before an ASCII '<'; RuneStart keeps us safe.
			start := len(content) - k
			if utf8.RuneStart(content[start]) {
				return k
			}
		}
	}
	return 0
}

// findRealThinkingTag finds the next occurrence of tag that is a real structural
// marker: not quoted (backtick/quote/backslash adjacent), not inside a Markdown
// code fence, and not on a blockquote line.
func findRealThinkingTag(content, tag string, from int, allowEndBoundary bool) int {
	if from < 0 {
		from = 0
	}
	isStartTag := tag == thinkingStartTag
	searchFrom := from
	for searchFrom < len(content) {
		rel := strings.Index(content[searchFrom:], tag)
		if rel == -1 {
			return -1
		}
		pos := searchFrom + rel
		after := pos + len(tag)
		if !isThinkingTagQuoted(content, pos, after, isStartTag) &&
			!isInsideMarkdownFence(content, pos) &&
			!isLineBlockQuote(content, pos) &&
			(!allowEndBoundary || after <= len(content)) {
			return pos
		}
		searchFrom = pos + 1
	}
	return -1
}

func isThinkingTagQuoted(content string, start, after int, isStartTag bool) bool {
	if isStartTag && start > 0 && isThinkingQuoteChar(content[start-1]) {
		return true
	}
	return !isStartTag && after < len(content) && isThinkingQuoteChar(content[after])
}

func isThinkingQuoteChar(ch byte) bool {
	switch ch {
	case '`', '"', '\'', '\\':
		return true
	default:
		return false
	}
}

func isInsideMarkdownFence(content string, pos int) bool {
	inFence := false
	lineStart := 0
	for lineStart < pos {
		lineEnd := strings.IndexByte(content[lineStart:], '\n')
		if lineEnd == -1 {
			lineEnd = len(content)
		} else {
			lineEnd += lineStart
		}
		line := strings.TrimSpace(content[lineStart:lineEnd])
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
		}
		lineStart = lineEnd + 1
	}
	return inFence
}

func isLineBlockQuote(content string, pos int) bool {
	lineStart := strings.LastIndexByte(content[:pos], '\n') + 1
	return strings.HasPrefix(strings.TrimLeftFunc(content[lineStart:pos], unicode.IsSpace), ">")
}
