package proxy

import (
	"strings"
	"testing"
)

// splitterSink records the callback stream from a thinkingSplitter so tests can
// assert on the ordered plain/thinking segments and block open/close events.
type splitterSink struct {
	plain    strings.Builder
	thinking strings.Builder
	events   []string // ordered log: "open", "close", "plain:...", "think:..."
}

func newTestSplitter() (*thinkingSplitter, *splitterSink) {
	sink := &splitterSink{}
	s := &thinkingSplitter{
		onPlain: func(t string) {
			sink.plain.WriteString(t)
			sink.events = append(sink.events, "plain:"+t)
		},
		onOpen: func() { sink.events = append(sink.events, "open") },
		onThinking: func(t string) {
			sink.thinking.WriteString(t)
			sink.events = append(sink.events, "think:"+t)
		},
		onClose: func() { sink.events = append(sink.events, "close") },
	}
	return s, sink
}

func (sink *splitterSink) hasEvent(prefix string) bool {
	for _, e := range sink.events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// The core regression: a literal </thinking> mid-sentence (not followed by
// \n\n) must NOT be treated as a real reasoning boundary, and the whole message
// must be emitted as plain text. The naive strings.Index parser flipped into
// thinking mode here and dropped the remainder — the observed truncation.
func TestThinkingSplitterLiteralEndTagMidSentenceStaysPlain(t *testing.T) {
	s, sink := newTestSplitter()
	msg := "To close a thinking block you write </thinking> and then continue writing the rest of your answer normally."
	s.push(msg)
	s.flush()

	if sink.hasEvent("open") || sink.hasEvent("think:") {
		t.Fatalf("literal </thinking> must not open a thinking block; events=%v", sink.events)
	}
	if got := sink.plain.String(); got != msg {
		t.Fatalf("plain output truncated/altered:\n got=%q\nwant=%q", got, msg)
	}
}

// A literal <thinking> inside inline code / quotes / blockquote / code fence
// must be left in the plain text, mirroring sub2api's IgnoresLiteralTags test.
func TestThinkingSplitterIgnoresLiteralTagsInMarkdown(t *testing.T) {
	s, sink := newTestSplitter()
	content := strings.Join([]string{
		"Use `<thinking>` literally.",
		"Quote \"<thinking>\" and '</thinking>'.",
		"> <thinking>quoted</thinking>",
		"```",
		"<thinking>code</thinking>",
		"```",
	}, "\n")
	s.push(content)
	s.flush()

	if sink.hasEvent("open") || sink.hasEvent("think:") {
		t.Fatalf("literal tags in markdown must stay plain; events=%v", sink.events)
	}
	if got := sink.plain.String(); got != content {
		t.Fatalf("plain output altered:\n got=%q\nwant=%q", got, content)
	}
}

// A real block (<thinking>...</thinking>\n\n) in one chunk splits into thinking
// then plain, and the tags themselves never leak into either stream.
func TestThinkingSplitterParsesRealBlockSingleChunk(t *testing.T) {
	s, sink := newTestSplitter()
	s.push("<thinking>\nreason</thinking>\n\nfinal")
	s.flush()

	if r := sink.thinking.String(); r != "reason" {
		t.Fatalf("thinking = %q, want %q", r, "reason")
	}
	if p := sink.plain.String(); p != "final" {
		t.Fatalf("plain = %q, want %q", p, "final")
	}
	if !sink.hasEvent("open") || !sink.hasEvent("close") {
		t.Fatalf("expected open+close events; events=%v", sink.events)
	}
	// Ordering: open -> think -> close -> plain.
	order := strings.Join(sink.events, "|")
	if !strings.Contains(order, "open|think:reason|close") {
		t.Fatalf("unexpected event order: %v", sink.events)
	}
}

// A start tag split across two pushes must still be detected (the safe tail is
// buffered), not emitted as literal "<think" plain text.
func TestThinkingSplitterBuffersSplitStartTag(t *testing.T) {
	s, sink := newTestSplitter()
	for _, chunk := range []string{"<think", "ing>\nreason</thinking>\n\nfinal"} {
		s.push(chunk)
	}
	s.flush()

	if sink.hasEvent("plain:<think") {
		t.Fatalf("split start tag leaked as plain text; events=%v", sink.events)
	}
	if r := sink.thinking.String(); r != "reason" {
		t.Fatalf("thinking = %q, want %q", r, "reason")
	}
	if p := sink.plain.String(); p != "final" {
		t.Fatalf("plain = %q, want %q", p, "final")
	}
}

// An end tag split across two pushes must still close the block correctly.
func TestThinkingSplitterBuffersSplitEndTag(t *testing.T) {
	s, sink := newTestSplitter()
	for _, chunk := range []string{"<thinking>\nreason</think", "ing>\n\nfinal"} {
		s.push(chunk)
	}
	s.flush()

	if r := sink.thinking.String(); r != "reason" {
		t.Fatalf("thinking = %q, want %q", r, "reason")
	}
	if p := sink.plain.String(); p != "final" {
		t.Fatalf("plain = %q, want %q", p, "final")
	}
	if strings.Contains(sink.thinking.String(), "</think") || strings.Contains(sink.plain.String(), "</think") {
		t.Fatalf("end tag leaked into output; events=%v", sink.events)
	}
}

// A thinking block that reaches EOF with only whitespace (a single \n, not the
// full \n\n) after </thinking> must still close cleanly at flush.
func TestThinkingSplitterFlushClosesWhitespaceTerminatedBlock(t *testing.T) {
	s, sink := newTestSplitter()
	s.push("<thinking>reason</thinking>\n")
	s.flush()

	if r := sink.thinking.String(); r != "reason" {
		t.Fatalf("thinking = %q, want %q", r, "reason")
	}
	if sink.plain.String() != "" {
		t.Fatalf("plain = %q, want empty", sink.plain.String())
	}
	if !sink.hasEvent("close") {
		t.Fatalf("expected close on flush; events=%v", sink.events)
	}
	if strings.Contains(sink.thinking.String(), "</think") {
		t.Fatalf("end tag leaked into thinking; events=%v", sink.events)
	}
}

// An unterminated real start tag at EOF must flush the buffered reasoning (no
// silent drop) and close the block.
func TestThinkingSplitterFlushUnterminatedBlock(t *testing.T) {
	s, sink := newTestSplitter()
	s.push("<thinking>reason that never closes")
	s.flush()

	if r := sink.thinking.String(); r != "reason that never closes" {
		t.Fatalf("thinking = %q, want full unterminated reasoning", r)
	}
	if !sink.hasEvent("close") {
		t.Fatalf("expected close on flush; events=%v", sink.events)
	}
}

// Leading-apostrophe thinking content (sub2api regression) must be preserved.
func TestThinkingSplitterPreservesLeadingApostrophe(t *testing.T) {
	s, sink := newTestSplitter()
	for _, chunk := range []string{"<thinking>'re working with.", "</thinking>\n\n", "final"} {
		s.push(chunk)
	}
	s.flush()

	if r := sink.thinking.String(); !strings.Contains(r, "'re working with.") {
		t.Fatalf("thinking = %q, want to contain leading apostrophe", r)
	}
	if p := sink.plain.String(); p != "final" {
		t.Fatalf("plain = %q, want %q", p, "final")
	}
}

func TestExtractThinkingFromContentRealBlock(t *testing.T) {
	plain, reasoning := extractThinkingFromContent("<thinking>\nreason</thinking>\n\nfinal text")
	if plain != "final text" {
		t.Fatalf("plain = %q, want %q", plain, "final text")
	}
	if reasoning != "\nreason" {
		t.Fatalf("reasoning = %q, want %q", reasoning, "\nreason")
	}
}

// Literal tags in code fences / quotes must stay in the plain output and NOT be
// stripped as reasoning — the non-stream twin of the truncation bug.
func TestExtractThinkingFromContentKeepsLiteralTags(t *testing.T) {
	content := strings.Join([]string{
		"Use `<thinking>` literally.",
		"```",
		"<thinking>code</thinking>",
		"```",
	}, "\n")
	plain, reasoning := extractThinkingFromContent(content)
	if plain != content {
		t.Fatalf("plain output altered:\n got=%q\nwant=%q", plain, content)
	}
	if reasoning != "" {
		t.Fatalf("reasoning = %q, want empty", reasoning)
	}
}

// A literal </thinking> mid-sentence with no real start tag must return the
// content unchanged with no reasoning.
func TestExtractThinkingFromContentLiteralEndTagOnly(t *testing.T) {
	content := "You write </thinking> to close and then keep going."
	plain, reasoning := extractThinkingFromContent(content)
	if plain != content {
		t.Fatalf("plain = %q, want unchanged %q", plain, content)
	}
	if reasoning != "" {
		t.Fatalf("reasoning = %q, want empty", reasoning)
	}
}
