package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// streamKeepaliveInterval is how often an idle SSE response may emit a comment
// keepalive (`: keepalive`). Kept as a package var so tests can inject a short
// interval without waiting the production ~10s gap.
var streamKeepaliveInterval = 10 * time.Second

// streamSSE serializes all writes to an SSE ResponseWriter and optionally emits
// Stream Keepalive comments while the connection is idle. Real event writes and
// the keepalive goroutine share mu so concurrent flushes never interleave.
type streamSSE struct {
	w       http.ResponseWriter
	flusher http.Flusher

	mu            sync.Mutex
	lastWriteNano int64
	stopped       bool
	committed     bool // true after any body write (real event or keepalive)
	done          chan struct{}
	stopOnce      sync.Once
}

// startStreamSSE wraps w/flusher with a keepalive ticker. Callers must route
// every client-visible write through WriteRaw/WriteEvent/WriteData and call
// Stop when the stream ends (defer is fine). Before falling back to JSON error
// helpers that call WriteHeader, Stop first and check Committed — a keepalive
// may already have flushed the 200 SSE body.
func startStreamSSE(w http.ResponseWriter, flusher http.Flusher) *streamSSE {
	s := &streamSSE{
		w:             w,
		flusher:       flusher,
		lastWriteNano: time.Now().UnixNano(),
		done:          make(chan struct{}),
	}
	go s.keepaliveLoop()
	return s
}

func (s *streamSSE) keepaliveLoop() {
	ticker := time.NewTicker(streamKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.stopped && time.Since(time.Unix(0, s.lastWriteNano)) >= streamKeepaliveInterval {
				fmt.Fprint(s.w, ": keepalive\n\n")
				s.flusher.Flush()
				s.committed = true
				s.lastWriteNano = time.Now().UnixNano()
			}
			s.mu.Unlock()
		}
	}
}

// Stop ends the keepalive goroutine. Safe to call more than once.
func (s *streamSSE) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		close(s.done)
	})
}

// Committed reports whether any body bytes (event or keepalive) have been
// written. After that point the HTTP status is fixed and clients expect SSE.
func (s *streamSSE) Committed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// WriteRaw writes an already-framed SSE payload and records the write time so
// keepalive only fires after a true idle gap.
func (s *streamSSE) WriteRaw(payload string) {
	s.mu.Lock()
	fmt.Fprint(s.w, payload)
	s.flusher.Flush()
	s.committed = true
	s.lastWriteNano = time.Now().UnixNano()
	s.mu.Unlock()
}

// WriteEvent emits a named SSE event (`event:` + `data:`) used by Claude
// Messages and Responses streams.
func (s *streamSSE) WriteEvent(event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	s.WriteRaw(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(jsonData)))
}

// WriteData emits an OpenAI-style `data:` SSE line (chat completions / [DONE]).
func (s *streamSSE) WriteData(data string) {
	s.WriteRaw(fmt.Sprintf("data: %s\n\n", data))
}

type thinkingStreamSource int

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
