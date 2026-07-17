package proxy

import (
	"errors"
	"testing"
)

func TestKiroSemanticEventConstructorsEnforceInvariants(t *testing.T) {
	if _, err := newPlainTextDelta(""); err == nil {
		t.Fatal("expected empty plain text to fail")
	}
	if _, err := newReasoningDelta(""); err == nil {
		t.Fatal("expected empty reasoning to fail")
	}
	if _, err := newToolStart("", "name"); err == nil {
		t.Fatal("expected empty tool id to fail")
	}
	if _, err := newToolStart("id", ""); err == nil {
		t.Fatal("expected empty tool name to fail")
	}
	if _, err := newToolInput("id", "name", "", false); err == nil {
		t.Fatal("expected empty tool input to fail")
	}
	if _, err := newCreditDelta(0); err == nil {
		t.Fatal("expected zero credit delta to fail")
	}
	if _, err := newStreamError(nil); err == nil {
		t.Fatal("expected nil stream error to fail")
	}
	if _, err := newStreamError(errors.New("x")); err != nil {
		t.Fatalf("expected non-nil error event, got %v", err)
	}
	if ev := newTerminalBoundary(); ev.kind != kiroKindTerminal {
		t.Fatalf("terminal kind: got %v", ev.kind)
	}
}
