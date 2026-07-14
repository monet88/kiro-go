package proxy

import (
	"encoding/json"
	"unicode/utf8"

	"kiro-go/config"
)

// truncationPlaceholder is inserted in history where older turns were dropped to
// fit within the payload limit.
const truncationPlaceholder = "[Earlier conversation history was truncated to fit the model's input limit. Older messages and tool activity have been omitted.]"

// minRecentHistoryTurns is the number of most-recent history entries always kept
// (in addition to system priming and the active tool turn) when truncating.
const minRecentHistoryTurns = 4

// truncatePayloadToLimit drops the oldest conversation history turns until the
// serialized payload fits within the operator-configured MaxPayloadBytes cap
// (config.GetMaxPayloadBytes). Kiro's upstream rejects oversized requests with
// HTTP 400 "Input is too long." (CONTENT_LENGTH_EXCEEDS_THRESHOLD), so the cap
// is kept below the observed upstream threshold to leave room for headers and
// minor serialization overhead. It preserves, in order:
//   - the system priming pair (if present) at the front of history,
//   - the most recent turns (at least minRecentHistoryTurns, and always the
//     active tool turn that pairs with the current message),
//   - the current message itself.
//
// A single placeholder note (truncationPlaceholder) is inserted where older
// turns were removed so the model is aware context was elided. hasPriming
// indicates whether history begins with the 2-entry system priming pair.
func truncatePayloadToLimit(payload *KiroPayload, hasPriming bool) {
	truncatePayloadToLimitBytes(payload, hasPriming, config.GetMaxPayloadBytes())
}

// truncatePayloadToLimitBytes is the byte-limit-parameterized core of
// truncatePayloadToLimit. It is separated so tests can exercise truncation with
// an explicit cap without depending on global config state.
func truncatePayloadToLimitBytes(payload *KiroPayload, hasPriming bool, maxPayloadBytes int) {
	if payload == nil {
		return
	}
	if maxPayloadBytes <= 0 {
		maxPayloadBytes = config.DefaultMaxPayloadBytes
	}
	if payloadByteSize(payload) <= maxPayloadBytes {
		return
	}

	history := payload.ConversationState.History
	primingCount := 0
	if hasPriming && len(history) >= 2 {
		primingCount = 2
	}

	priming := history[:primingCount]
	conversation := history[primingCount:]

	// Compute the fixed overhead (everything except the trimmable conversation):
	// priming, current message, inference config, profileArn, etc. We estimate by
	// measuring the payload with an empty conversation tail, then add a budget for
	// the placeholder and retained tail turns.
	placeholderEntry := KiroHistoryMessage{
		UserInputMessage: &KiroUserInputMessage{
			Content: truncationPlaceholder,
			ModelID: currentMessageModelID(payload),
			Origin:  "AI_EDITOR",
		},
	}

	// Precompute byte size of each conversation entry once (O(n)).
	entrySizes := make([]int, len(conversation))
	for i := range conversation {
		entrySizes[i] = historyEntryByteSize(conversation[i])
	}

	// Base size: payload with priming only (no conversation), plus placeholder.
	payload.ConversationState.History = priming
	baseSize := payloadByteSize(payload) + historyEntryByteSize(placeholderEntry)

	// Keep the largest suffix of the conversation that fits, but never fewer than
	// minRecentHistoryTurns entries (so recent context is preserved).
	keepFrom := len(conversation)
	running := baseSize
	for i := len(conversation) - 1; i >= 0; i-- {
		running += entrySizes[i]
		kept := len(conversation) - i
		if running > maxPayloadBytes && kept > minRecentHistoryTurns {
			break
		}
		keepFrom = i
	}

	tail := conversation[keepFrom:]
	tail = dropLeadingAssistant(tail)

	rebuilt := make([]KiroHistoryMessage, 0, len(priming)+1+len(tail))
	rebuilt = append(rebuilt, priming...)
	if keepFrom > 0 { // older turns were dropped → note the elision
		rebuilt = append(rebuilt, placeholderEntry)
	}
	rebuilt = append(rebuilt, tail...)
	payload.ConversationState.History = rebuilt

	// If still too large (current message or retained tail alone exceeds the
	// limit), shrink the current message content as a last resort.
	if payloadByteSize(payload) > maxPayloadBytes {
		truncateCurrentMessage(payload, maxPayloadBytes)
	}
}

// historyEntryByteSize returns the serialized size of a single history entry,
// including the surrounding JSON array delimiter overhead (1 byte for the comma).
func historyEntryByteSize(entry KiroHistoryMessage) int {
	raw, err := json.Marshal(entry)
	if err != nil {
		return 0
	}
	return len(raw) + 1
}

// dropLeadingAssistant removes a leading assistant message from a history tail so
// it does not directly follow the placeholder user turn with a broken pairing.
func dropLeadingAssistant(tail []KiroHistoryMessage) []KiroHistoryMessage {
	for len(tail) > 0 && tail[0].AssistantResponseMessage != nil {
		tail = tail[1:]
	}
	return tail
}

// payloadByteSize returns the serialized size of the payload in bytes.
func payloadByteSize(payload *KiroPayload) int {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	return len(raw)
}

func currentMessageModelID(payload *KiroPayload) string {
	return payload.ConversationState.CurrentMessage.UserInputMessage.ModelID
}

// truncateCurrentMessage hard-truncates the current message content as a last
// resort when even the minimal retained history plus current message exceeds the
// limit.
func truncateCurrentMessage(payload *KiroPayload, maxPayloadBytes int) {
	cur := &payload.ConversationState.CurrentMessage.UserInputMessage
	overhead := payloadByteSize(payload) - len(cur.Content)
	budget := maxPayloadBytes - overhead
	if budget < 0 {
		budget = 0
	}
	if len(cur.Content) > budget {
		if budget == 0 {
			cur.Content = minimalFallbackUserContent
			return
		}
		// Back off to the nearest UTF-8 rune boundary at or below budget so a
		// multi-byte character (e.g. CJK, emoji) is never split, which would
		// produce an invalid string and break JSON marshalling / upstream.
		cut := budget
		for cut > 0 && !utf8.RuneStart(cur.Content[cut]) {
			cut--
		}
		cur.Content = cur.Content[:cut]
	}
}
