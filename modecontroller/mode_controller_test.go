package modecontroller

import (
	"context"
	"testing"

	"go.viam.com/rdk/logging"
)

func TestReadingsDisablesCamerasOutsideAway(t *testing.T) {
	for _, tc := range []struct {
		mode        Mode
		wantEnabled bool
	}{
		{ModeAway, true},
		{ModeAwayLite, false},
		{ModeHome, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			m := &modeController{
				mode:             tc.mode,
				cameraComponents: []ComponentCapture{{Name: "cam", Method: "GetImages"}, {Name: "cam2"}},
				controlKey:       defaultControlKey,
				logger:           logging.NewTestLogger(t),
			}
			r, err := m.Readings(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			entries := r[defaultControlKey].([]captureEntry)
			if len(entries) != 2 {
				t.Fatalf("got %d entries, want 2", len(entries))
			}
			for _, e := range entries {
				disabled := e.FreqHz != nil && *e.FreqHz == 0
				if disabled == tc.wantEnabled {
					t.Errorf("%s/%s in %s: disabled=%v, want enabled=%v", e.ResourceName, e.Method, tc.mode, disabled, tc.wantEnabled)
				}
			}
		})
	}
}
