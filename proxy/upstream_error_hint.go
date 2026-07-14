package proxy

import "strings"

// improperlyFormedMarker is the substring AWS returns in the 400 response body
// when it rejects the request payload itself ("Improperly formed request.").
// Matched case-insensitively so a minor upstream wording/casing change does not
// silently bypass the client-facing rewrite. Ported from ngh1105 / kiro-tutu.
const improperlyFormedMarker = "improperly formed request"

// isImproperlyFormedRejection reports whether an upstream error body marks the
// request as improperly formed.
func isImproperlyFormedRejection(errBody string) bool {
	return strings.Contains(strings.ToLower(errBody), improperlyFormedMarker)
}

// improperlyFormedClientMessage translates the upstream's opaque "Improperly
// formed request" rejection into a readable client hint. In practice the vast
// majority of these rejections occur when the request structure is perfectly
// valid but the tool definitions are too numerous or the payload too large
// (upstream limits on large requests); the raw wording makes users think the
// gateway is buggy. Other errors pass through unchanged.
//
// Complements tool_compression.go (compressToolsIfNeeded): compression prevents
// the rejection when it can; when it cannot, this surfaces an actionable message
// instead of the raw upstream text.
func improperlyFormedClientMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if isImproperlyFormedRejection(msg) {
		return "upstream rejected the request (commonly caused by too many tool definitions or an oversized payload); try reducing the number of tools or shortening the context"
	}
	return msg
}
