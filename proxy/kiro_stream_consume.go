package proxy

import (
	"context"
	"io"
	"kiro-go/logger"
)

// consumeEventStream decodes AWS Event Stream frames into Kiro Semantic Events
// and invokes onEvent for each. onEvent may return a non-nil error to abort.
func consumeEventStream(ctx context.Context, body io.Reader, onEvent func(kiroSemanticEvent) error) error {
	if onEvent == nil {
		onEvent = func(kiroSemanticEvent) error { return nil }
	}
	extractor := newKiroSemanticExtractor()
	var contentEventCount int

	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}

		prelude := make([]byte, 12)
		preludeN, err := io.ReadFull(body, prelude)
		if err == io.EOF {
			break
		}
		if err != nil {
			logger.Warnf("[EventStream] Prelude read failed at byte %d after %d content events: %v", preludeN, contentEventCount, err)
			return err
		}
		contentEventCount++

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])
		if totalLength < 16 {
			continue
		}
		remaining := totalLength - 12
		msgBuf := make([]byte, remaining)
		_, err = io.ReadFull(body, msgBuf)
		if err != nil {
			logger.Warnf("[EventStream] Message body read failed at event %d (totalLen=%d headersLen=%d): %v", contentEventCount, totalLength, headersLength, err)
			return err
		}
		if headersLength > len(msgBuf)-4 {
			continue
		}
		eventType := extractEventType(msgBuf[0:headersLength])
		payloadBytes := msgBuf[headersLength : len(msgBuf)-4]
		for _, ev := range extractor.ingestJSONPayload(eventType, payloadBytes) {
			if err := onEvent(ev); err != nil {
				return err
			}
		}
	}
	for _, ev := range extractor.finish() {
		if err := onEvent(ev); err != nil {
			return err
		}
	}
	return nil
}

// parseEventStream preserves the legacy callback surface for parity/unit tests
// by routing the single decoder/extractor path through the compatibility adapter.
// Production handlers use consumeEventStream + Assistant Normalizer only.
func parseEventStream(ctx context.Context, body io.Reader, callback *KiroStreamCallback) error {
	adapter := newKiroCallbackAdapter(callback)
	return consumeEventStream(ctx, body, func(ev kiroSemanticEvent) error {
		adapter.handle(ev)
		return nil
	})
}
