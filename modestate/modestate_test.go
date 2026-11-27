package modestate

import (
	"os"
	"path/filepath"
	"testing"
)

func useTempState(t *testing.T) string {
	t.Helper()
	orig := statePath
	statePath = filepath.Join(t.TempDir(), "mode.json")
	t.Cleanup(func() { statePath = orig })
	return statePath
}

func TestCamerasAllowedIn(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{OperatingModeAway, true},
		{OperatingModeAwayLite, false},
		{OperatingModeHome, false},
		{"", false},
		{"Away", false},
		{"vacation", false},
	} {
		if got := CamerasAllowedIn(tc.mode); got != tc.want {
			t.Errorf("CamerasAllowedIn(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestCameraCaptureAllowed(t *testing.T) {
	t.Run("no state file defaults to home", func(t *testing.T) {
		useTempState(t)
		if CameraCaptureAllowed() {
			t.Fatal("cameras allowed with no persisted mode; want denied (default is home)")
		}
	})
	for _, mode := range []string{OperatingModeAway, OperatingModeAwayLite, OperatingModeHome} {
		t.Run(mode, func(t *testing.T) {
			useTempState(t)
			if err := SetOperatingMode(mode); err != nil {
				t.Fatal(err)
			}
			if got, want := CameraCaptureAllowed(), mode == OperatingModeAway; got != want {
				t.Fatalf("CameraCaptureAllowed() in %s = %v, want %v", mode, got, want)
			}
		})
	}
	t.Run("unreadable state fails closed", func(t *testing.T) {
		path := useTempState(t)
		if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if CameraCaptureAllowed() {
			t.Fatal("cameras allowed with a corrupt state file; want denied")
		}
	})
}
