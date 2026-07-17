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
	if _, err := newToolInput("id", "name", "", toolInputAppend); err == nil {
		t.Fatal("expected empty tool input to fail")
	}
	if _, err := newToolStop("", "name"); err == nil {
		t.Fatal("expected empty tool stop id to fail")
	}
	if _, err := newToolStop("id", ""); err == nil {
		t.Fatal("expected empty tool stop name to fail")
	}
	if _, err := newCreditDelta(0); err == nil {
		t.Fatal("expected zero credit delta to fail")
	}
	if _, err := newUsageSnapshot(-1, 0); err == nil {
		t.Fatal("expected negative input tokens to fail")
	}
	if _, err := newUsageSnapshot(0, -1); err == nil {
		t.Fatal("expected negative output tokens to fail")
	}
	if _, err := newContextUsageSnapshot(-0.1); err == nil {
		t.Fatal("expected context percentage below 0 to fail")
	}
	if _, err := newContextUsageSnapshot(100.1); err == nil {
		t.Fatal("expected context percentage above 100 to fail")
	}
	if _, err := newStopMetadata(""); err == nil {
		t.Fatal("expected empty stop metadata reason to fail")
	}
	if _, err := newStreamError(kiroErrorUpstream, nil); err == nil {
		t.Fatal("expected nil stream error to fail")
	}
	if _, err := newStreamError(kiroErrorClass(99), errors.New("x")); err == nil {
		t.Fatal("expected invalid error class to fail")
	}
	if ev, err := newStreamError(kiroErrorUpstream, errors.New("x")); err != nil {
		t.Fatalf("expected non-nil error event, got %v", err)
	} else if ev.errClass != kiroErrorUpstream {
		t.Fatalf("upstream class: got %v want %v", ev.errClass, kiroErrorUpstream)
	}
	if ev, err := newStreamError(kiroErrorModelOutput, errors.New("y")); err != nil {
		t.Fatalf("expected model-output error event, got %v", err)
	} else if ev.errClass != kiroErrorModelOutput {
		t.Fatalf("model-output class: got %v want %v", ev.errClass, kiroErrorModelOutput)
	}
	if ev := newTerminalBoundary(); ev.kind != kiroKindTerminal {
		t.Fatalf("terminal kind: got %v", ev.kind)
	}
}
