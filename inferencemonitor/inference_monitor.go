// Package inferencemonitor exposes bark_monitor's per-classification
// inference durations (recorded via inferencetiming) as a pollable sensor,
// so they can be captured/synced like any other Viam data.
package inferencemonitor

import (
	"context"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/katie-viam/oreo-watcher/inferencetiming"
)

// Model is the model triple for the inference-monitor sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "inference-monitor")

// Config holds the component configuration. There is nothing to configure
// today — bark_monitor records samples into the shared inferencetiming
// buffer independent of any resource dependency graph.
type Config struct{}

func (c *Config) Validate(path string) ([]string, []string, error) {
	return nil, nil, nil
}

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newInferenceMonitor,
	})
}

type inferenceMonitor struct {
	resource.Named
	resource.AlwaysRebuild
	logger logging.Logger
}

func newInferenceMonitor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	if _, err := resource.NativeConfig[*Config](conf); err != nil {
		return nil, err
	}
	return &inferenceMonitor{
		Named:  conf.ResourceName().AsNamed(),
		logger: logger,
	}, nil
}

// Readings drains every classification-duration sample recorded since the
// last poll and returns each as its own {started_at, duration_ms} object.
// Draining (rather than reporting only the latest sample) means no
// individual inference call's timing is lost even if this sensor is polled
// less often than bark_monitor actually classifies spectrograms.
func (m *inferenceMonitor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	entries := inferencetiming.Drain()
	if len(entries) == 0 {
		return nil, data.ErrNoCaptureToStore
	}

	// Protobuf Struct conversion (structpb.NewValue) only knows how to encode
	// []interface{} and map[string]interface{} — a concretely-typed slice or
	// struct here fails at the gRPC boundary with "invalid type: ...", not at
	// compile or vet time.
	samples := make([]interface{}, len(entries))
	for i, e := range entries {
		samples[i] = map[string]interface{}{
			"started_at":  e.StartedAt.Format(time.RFC3339Nano),
			"duration_ms": e.DurationMs,
		}
	}

	return map[string]interface{}{
		"count":   len(entries),
		"samples": samples,
	}, nil
}

func (m *inferenceMonitor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, resource.ErrDoUnimplemented
}

func (m *inferenceMonitor) Close(ctx context.Context) error {
	return nil
}
