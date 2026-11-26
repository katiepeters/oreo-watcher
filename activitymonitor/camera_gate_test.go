package activitymonitor

import (
	"context"
	"image"
	"testing"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/testutils/inject"
	"go.viam.com/rdk/vision/objectdetection"

	modecontroller "github.com/katie-viam/oreo-watcher/modecontroller"
)

type fixedMode modecontroller.Mode

func (f fixedMode) Mode() modecontroller.Mode { return modecontroller.Mode(f) }

func TestTickReadsNoCamerasOutsideAway(t *testing.T) {
	for _, tc := range []struct {
		name     string
		modeCtrl modecontroller.ModeController
	}{
		{"home via mode_controller", fixedMode(modecontroller.ModeHome)},
		{"away_lite via mode_controller", fixedMode(modecontroller.ModeAwayLite)},
		{"no mode_controller, persisted mode denies", nil},
	} {
		mode := tc.name
		t.Run(tc.name, func(t *testing.T) {
			cam := inject.NewCamera("cam")
			cam.ImagesFunc = func(context.Context, []string, map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
				t.Errorf("camera Images called (%s)", mode)
				return nil, resource.ResponseMetadata{}, nil
			}
			vis := inject.NewVisionService("vis")
			vis.DetectionsFromCameraFunc = func(context.Context, string, map[string]interface{}) ([]objectdetection.Detection, error) {
				t.Errorf("vision DetectionsFromCamera called (%s)", mode)
				return nil, nil
			}
			a := &activityMonitor{
				oreoVisSvc:  vis,
				sceneVisSvc: vis,
				cam1:        cam,
				cam2:        cam,
				cam1Name:    "cam",
				cam2Name:    "cam",
				modeCtrl:    tc.modeCtrl,
				logger:      logging.NewTestLogger(t),
				// Mode controllers above deny on their own; for the nil case
				// this stands in for a persisted home mode.
				persistedCamerasAllowed: func() bool { return false },
				referenceImg1:           image.NewGray(image.Rect(0, 0, 1, 1)),
				referenceImg2:           image.NewGray(image.Rect(0, 0, 1, 1)),
			}
			a.tick(context.Background())
			if a.referenceImg1 != nil || a.referenceImg2 != nil {
				t.Error("reference frames still held in memory after switching off away mode")
			}
		})
	}
}

// TestModeControllerCannotOverridePersistedMode checks that an away-mode
// controller still can't enable cameras when the persisted mode denies them.
func TestModeControllerCannotOverridePersistedMode(t *testing.T) {
	a := &activityMonitor{
		modeCtrl:                fixedMode(modecontroller.ModeAway),
		persistedCamerasAllowed: func() bool { return false },
	}
	if a.camerasAllowed() {
		t.Fatal("cameras allowed while the persisted mode denies them")
	}
	a.persistedCamerasAllowed = func() bool { return true }
	if !a.camerasAllowed() {
		t.Fatal("cameras denied in away mode with persisted mode allowing them")
	}
}
