package privacycam

import (
	"testing"

	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/utils"
)

// TestConfigDecodesWebcamAttributes decodes attributes the way viam-server
// does. If the embedded webcam config is not flattened, video_path is lost
// and the camera silently opens whichever webcam it finds first.
func TestConfigDecodesWebcamAttributes(t *testing.T) {
	attrs := utils.AttributeMap{
		"video_path": "usb-EMEET_EMEET_SmartCam_C950_24082114152-video-index0",
		"format":     "MJPEG",
		"width_px":   640,
		"height_px":  480,
		"frame_rate": 30,
	}
	cfg, err := resource.TransformAttributeMap[*Config](attrs)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Path != attrs["video_path"] {
		t.Errorf("video_path = %q, want %q: tag the embedded WebcamConfig with json:\",squash\" so its fields decode", cfg.Path, attrs["video_path"])
	}
	if cfg.Format != "MJPEG" || cfg.Width != 640 || cfg.Height != 480 || cfg.FrameRate != 30 {
		t.Errorf("format/size/rate = %q %dx%d @%v, want MJPEG 640x480 @30", cfg.Format, cfg.Width, cfg.Height, cfg.FrameRate)
	}
}
