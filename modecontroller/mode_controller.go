package modecontroller

import (
	"context"
	"fmt"
	"sync"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/katie-viam/oreo-watcher/modestate"
)

// Model is the model triple for the mode-controller sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "mode-controller")

// Mode represents the operating mode of the monitoring system.
type Mode string

const (
	ModeAway     Mode = "away"      // cameras + mic, data captured
	ModeAwayLite Mode = "away_lite" // mic only, data captured
	ModeHome     Mode = "home"      // bark warnings only, no data captured
)

// ModeController is the interface activity-monitor and bark-monitor use to
// read the current operating mode.
type ModeController interface {
	Mode() Mode
}

const defaultControlKey = "controls"

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newModeController,
	})
}

// ComponentCapture identifies a component/method pair to include in capture control.
type ComponentCapture struct {
	// Name is the resource name of the component.
	Name string `json:"name"`
	// Method is the capture method name. Defaults to "Readings" if empty.
	// Use "GetImages" for camera components.
	Method string `json:"method"`
}

// Config holds the component configuration.
type Config struct {
	// CameraComponents lists camera components whose GetImages capture is disabled
	// in away_lite and home modes (e.g. cam, real-sense-cam with method "GetImages").
	CameraComponents []ComponentCapture `json:"camera_components"`
	// ControlKey is the key used in Readings for the capture control array.
	// Must match capture_control_sensor.key in the data manager config. Defaults to "controls".
	ControlKey string `json:"control_key"`
}

func (c *Config) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

type modeController struct {
	resource.Named
	resource.AlwaysRebuild
	mu               sync.RWMutex
	mode             Mode
	cameraComponents []ComponentCapture
	controlKey       string
	logger           logging.Logger
}

func newModeController(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	controlKey := cfg.ControlKey
	if controlKey == "" {
		controlKey = defaultControlKey
	}
	mode := ModeHome
	if persisted, err := modestate.GetOperatingMode(); err != nil {
		logger.Warnw("failed to read persisted operating mode, defaulting to home", "error", err)
	} else if persisted != "" {
		mode = Mode(persisted)
	}
	return &modeController{
		Named:            conf.ResourceName().AsNamed(),
		mode:             mode,
		cameraComponents: cfg.CameraComponents,
		controlKey:       controlKey,
		logger:           logger,
	}, nil
}

// Mode returns the current operating mode. Safe to call concurrently.
func (m *modeController) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// captureEntry mirrors datamanager.CaptureConfigReading for JSON serialization.
// FreqHz nil = use robot config frequency; FreqHz 0 = capture disabled.
type captureEntry struct {
	ResourceName string   `json:"resource_name"`
	Method       string   `json:"method"`
	FreqHz       *float32 `json:"capture_frequency_hz,omitempty"`
}

func disabledHz() *float32 { v := float32(0); return &v }

// Readings returns the capture control array consumed by the data manager's
// capture_control_sensor. The data manager polls this every 100ms and applies
// the returned overrides to each resource/method pair.
//
// Camera components are disabled in away_lite and home modes.
func (m *modeController) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	m.mu.RLock()
	mode := m.mode
	m.mu.RUnlock()

	entries := make([]captureEntry, 0, len(m.cameraComponents))
	for _, c := range m.cameraComponents {
		method := c.Method
		if method == "" {
			method = "Readings"
		}
		e := captureEntry{ResourceName: c.Name, Method: method}
		if mode == ModeAwayLite || mode == ModeHome {
			e.FreqHz = disabledHz()
		}
		entries = append(entries, e)
	}

	return map[string]interface{}{m.controlKey: entries}, nil
}

// DoCommand supports set_mode/get_mode (away/away_lite/home, persisted) and
// set_detection_mode/get_detection_mode (learning/trained, persisted) commands.
//
//	{"command": "set_mode", "mode": "away"|"away_lite"|"home"}
//	{"command": "get_mode"}
//	{"command": "set_detection_mode", "mode": "learning"|"trained"}
//	{"command": "get_detection_mode"}
func (m *modeController) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "set_mode":
		modeStr, _ := cmd["mode"].(string)
		switch Mode(modeStr) {
		case ModeAway, ModeAwayLite, ModeHome:
			if err := modestate.SetOperatingMode(modeStr); err != nil {
				return nil, fmt.Errorf("persisting operating mode: %w", err)
			}
			m.mu.Lock()
			old := m.mode
			m.mode = Mode(modeStr)
			m.mu.Unlock()
			m.logger.Infof("mode changed: %s → %s", old, modeStr)
			return map[string]interface{}{"mode": modeStr}, nil
		default:
			return nil, fmt.Errorf("unknown mode %q; supported: away, away_lite, home", modeStr)
		}
	case "get_mode":
		return map[string]interface{}{"mode": string(m.Mode())}, nil
	case "set_detection_mode":
		modeStr, _ := cmd["mode"].(string)
		switch modeStr {
		case modestate.DetectionModeLearning, modestate.DetectionModeTrained:
			if err := modestate.SetDetectionMode(modeStr); err != nil {
				return nil, fmt.Errorf("persisting detection mode: %w", err)
			}
			m.logger.Infof("detection mode changed to %s", modeStr)
			return map[string]interface{}{"detection_mode": modeStr}, nil
		default:
			return nil, fmt.Errorf("unknown detection mode %q; supported: %s, %s",
				modeStr, modestate.DetectionModeLearning, modestate.DetectionModeTrained)
		}
	case "get_detection_mode":
		detectionMode, err := modestate.GetDetectionMode()
		if err != nil {
			return nil, fmt.Errorf("reading detection mode: %w", err)
		}
		return map[string]interface{}{"detection_mode": detectionMode}, nil
	default:
		return nil, fmt.Errorf("unknown command %q; supported: set_mode, get_mode, set_detection_mode, get_detection_mode", command)
	}
}

func (m *modeController) Close(ctx context.Context) error {
	return nil
}
