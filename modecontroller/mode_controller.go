package modecontroller

import (
	"context"
	"fmt"
	"sync"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
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

// ModeController is the interface that activity-monitor and bark-monitor use to
// read the current operating mode without importing the concrete type.
type ModeController interface {
	Mode() Mode
}

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newModeController,
	})
}

// Config holds the component configuration. No required fields — the component
// is self-contained and starts in home mode by default.
type Config struct{}

func (c *Config) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

type modeController struct {
	resource.Named
	resource.AlwaysRebuild
	mu     sync.RWMutex
	mode   Mode
	logger logging.Logger
}

func newModeController(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	return &modeController{
		Named:  conf.ResourceName().AsNamed(),
		mode:   ModeHome,
		logger: logger,
	}, nil
}

// Mode returns the current operating mode. Safe to call concurrently.
func (m *modeController) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// Readings returns the current mode as a sensor reading.
func (m *modeController) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	return nil, data.ErrNoCaptureToStore
}

// DoCommand supports set_mode and get_mode commands.
//
//	{"command": "set_mode", "mode": "away"|"away_lite"|"home"}
//	{"command": "get_mode"}
func (m *modeController) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "set_mode":
		modeStr, _ := cmd["mode"].(string)
		switch Mode(modeStr) {
		case ModeAway, ModeAwayLite, ModeHome:
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
	default:
		return nil, fmt.Errorf("unknown command %q; supported: set_mode, get_mode", command)
	}
}

func (m *modeController) Close(ctx context.Context) error {
	return nil
}
