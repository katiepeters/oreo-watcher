package sleepmonitor

import (
	"context"
	"fmt"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	activitymonitor "github.com/katie-viam/oreo-watcher/activitymonitor"
)

// Model is the model triple for the sleep-monitor sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "sleep-monitor")

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newSleepMonitor,
	})
}

// Config holds the component configuration.
type Config struct {
	ActivityMonitor string `json:"activity_monitor"`
}

// Validate ensures required fields are present and returns dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.ActivityMonitor == "" {
		return nil, nil, fmt.Errorf("activity_monitor is required")
	}
	return []string{c.ActivityMonitor}, nil, nil
}

type sleepMonitor struct {
	resource.Named
	resource.AlwaysRebuild
	activity activitymonitor.ActivityCapture
	logger   logging.Logger
}

func newSleepMonitor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	dep, ok := deps[sensor.Named(cfg.ActivityMonitor)]
	if !ok {
		return nil, fmt.Errorf("activity_monitor %q not found in dependencies", cfg.ActivityMonitor)
	}
	capture, ok := dep.(activitymonitor.ActivityCapture)
	if !ok {
		return nil, fmt.Errorf("dependency %q does not implement ActivityCapture; ensure it is an activity-monitor component", cfg.ActivityMonitor)
	}
	return &sleepMonitor{
		Named:    conf.ResourceName().AsNamed(),
		activity: capture,
		logger:   logger,
	}, nil
}

// Readings returns the next completed sleep session, or ErrNoCaptureToStore if none is ready.
func (m *sleepMonitor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	s := m.activity.PopSleepSession()
	if s == nil {
		return nil, data.ErrNoCaptureToStore
	}
	return s.Reading(), nil
}

func (m *sleepMonitor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, fmt.Errorf("no commands supported")
}

func (m *sleepMonitor) Close(ctx context.Context) error {
	return nil
}
