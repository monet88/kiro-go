package proxy

import (
	"net/http"
)

// writeModelOutputErrorJSON writes a sanitized pre-commit HTTP 502 for a
// Model Output Error. Raw tool arguments must never appear in the body.
func writeModelOutputErrorJSON(w http.ResponseWriter, protocol string) {
	msg := "upstream model output error"
	switch protocol {
	case "claude":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream model output error"}}`))
	case "openai", "responses":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream model output error","type":"server_error"}}`))
	default:
		http.Error(w, msg, http.StatusBadGateway)
	}
}

func sanitizedModelOutputMessage(err error) string {
	if err == nil {
		return "upstream model output error"
	}
	// modelOutputError messages are already sanitized by construction.
	if moe, ok := err.(*modelOutputError); ok && moe.Message != "" {
		return moe.Message
	}
	return "upstream model output error"
}
