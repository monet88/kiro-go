package config

import (
	"path/filepath"
	"testing"
)

// TestMaxPayloadBytesDefault verifies the built-in default is returned when the
// knob is unset (0), matching ticket #5's 2_000_000 requirement.
func TestMaxPayloadBytesDefault(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != DefaultMaxPayloadBytes {
		t.Fatalf("expected default %d, got %d", DefaultMaxPayloadBytes, got)
	}
	if DefaultMaxPayloadBytes != 2_000_000 {
		t.Fatalf("ticket #5 requires default 2_000_000, got %d", DefaultMaxPayloadBytes)
	}
}

// TestMaxPayloadBytesUpdatePersistsAndFloors verifies POST-style updates persist
// and that non-positive resets to default while sub-floor values are raised.
func TestMaxPayloadBytesUpdatePersistsAndFloors(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}

	if err := UpdateMaxPayloadBytes(5_000_000); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != 5_000_000 {
		t.Fatalf("expected 5_000_000 after update, got %d", got)
	}

	// Reload from disk to confirm persistence.
	if err := Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != 5_000_000 {
		t.Fatalf("expected 5_000_000 to persist across reload, got %d", got)
	}

	// Non-positive resets to default.
	if err := UpdateMaxPayloadBytes(0); err != nil {
		t.Fatalf("update zero: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != DefaultMaxPayloadBytes {
		t.Fatalf("expected default after zero update, got %d", got)
	}

	// Sub-floor value is raised to the floor.
	if err := UpdateMaxPayloadBytes(1); err != nil {
		t.Fatalf("update tiny: %v", err)
	}
	if got := GetMaxPayloadBytes(); got != minMaxPayloadBytes {
		t.Fatalf("expected floor %d for tiny value, got %d", minMaxPayloadBytes, got)
	}
}
