// Package learningmode implements a zero-training bark classifier using
// YAMNet. Unlike bark_monitor ("Trained Mode"), it never plays sounds or
// tracks sessions — its only job is to classify each gated audio clip and
// upload it (plus a paired spectrogram) to Viam, tagged for later human
// review in the companion web app. Only one of Learning Mode / Trained Mode
// is ever active at a time, controlled by the shared modestate file.
package learningmode

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.viam.com/rdk/app"
	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/ml"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/mlmodel"
	rutils "go.viam.com/rdk/utils"
	"gorgonia.org/tensor"

	barkmonitor "github.com/katie-viam/oreo-watcher/barkmonitor"
	filteredmic "github.com/katie-viam/oreo-watcher/filteredmic"
	"github.com/katie-viam/oreo-watcher/modestate"
	spectrogramcam "github.com/katie-viam/oreo-watcher/spectrogramcam"
)

// Model is the model triple for the learning-mode sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "learning-mode")

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newLearningMode,
	})
}

//go:embed yamnet_class_map.csv
var yamnetClassMapCSV []byte

//go:embed yamnet.tflite
var yamnetModelBytes []byte

const (
	yamnetInputTensorName  = "waveform_binary"
	yamnetOutputTensorName = "tower0/network/layer32/final_output"
	yamnetTargetRate       = 16000
	yamnetInputLen         = 15600 // fixed input length reported by the deployed model's Metadata()

	// YamnetModelPath is the fixed on-disk location this package writes its
	// embedded .tflite model to on first run. The tflite_cpu mlmodel service
	// instance backing yamnet_service must have its model_path config
	// attribute set to this same path.
	YamnetModelPath = "/root/models/yamnet.tflite"
)

// ensureModelFile writes the embedded YAMNet .tflite model to YamnetModelPath
// if it isn't already there, so a fresh machine doesn't need the model
// manually copied on before this module (and the tflite_cpu service pointed
// at YamnetModelPath) can work.
func ensureModelFile() error {
	if _, err := os.Stat(YamnetModelPath); err == nil {
		return nil // already present
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking for existing model file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(YamnetModelPath), 0o755); err != nil {
		return fmt.Errorf("creating model directory: %w", err)
	}
	if err := os.WriteFile(YamnetModelPath, yamnetModelBytes, 0o644); err != nil {
		return fmt.Errorf("writing embedded model file: %w", err)
	}
	return nil
}

// defaultTargetLabels is the default set of AudioSet labels treated as "a bark
// happened." Growling and Whimper (dog) are deliberately excluded for now.
var defaultTargetLabels = []string{"Dog", "Bark", "Yip", "Howl", "Bow-wow"}

// Config holds the component configuration.
type Config struct {
	FilteredMic          string   `json:"filtered_mic"`
	YamnetService        string   `json:"yamnet_service"`
	YamnetLabelThreshold float64  `json:"yamnet_label_threshold"`
	YamnetTargetLabels   []string `json:"yamnet_target_labels"`
}

// Validate ensures the config is valid and returns required dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.FilteredMic == "" {
		return nil, nil, fmt.Errorf("filtered_mic is required")
	}
	if c.YamnetService == "" {
		return nil, nil, fmt.Errorf("yamnet_service is required")
	}
	return []string{c.FilteredMic, c.YamnetService}, nil, nil
}

type learningMode struct {
	resource.Named
	resource.AlwaysRebuild

	audioCache filteredmic.AudioCapture
	yamnetSvc  mlmodel.Service
	dataClient *app.DataClient
	partID     string
	name       resource.Name
	logger     logging.Logger

	labelThreshold float64
	targetLabels   []string
	labelIndex     map[string]int // label display_name -> index into the 521-length score tensor
}

func newLearningMode(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	if err := ensureModelFile(); err != nil {
		return nil, fmt.Errorf("ensuring yamnet model file: %w", err)
	}

	dep, ok := deps[audioin.Named(cfg.FilteredMic)]
	if !ok {
		return nil, fmt.Errorf("filtered_mic %q not found in dependencies", cfg.FilteredMic)
	}
	cache, ok := dep.(filteredmic.AudioCapture)
	if !ok {
		return nil, fmt.Errorf("dependency %q does not implement AudioCapture; make sure it is a filtered-mic component", cfg.FilteredMic)
	}

	svcDep, ok := deps[mlmodel.Named(cfg.YamnetService)]
	if !ok {
		return nil, fmt.Errorf("yamnet_service %q not found in dependencies", cfg.YamnetService)
	}
	yamnetSvc, ok := svcDep.(mlmodel.Service)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not an mlmodel service", cfg.YamnetService)
	}

	threshold := cfg.YamnetLabelThreshold
	if threshold <= 0 {
		threshold = 0.5
	}
	targetLabels := cfg.YamnetTargetLabels
	if len(targetLabels) == 0 {
		targetLabels = defaultTargetLabels
	}

	labelIndex, err := parseLabelIndex(yamnetClassMapCSV)
	if err != nil {
		return nil, fmt.Errorf("parsing embedded yamnet class map: %w", err)
	}
	for _, label := range targetLabels {
		if _, ok := labelIndex[label]; !ok {
			return nil, fmt.Errorf("yamnet_target_labels: unknown label %q", label)
		}
	}

	viamClient, err := app.CreateViamClientFromEnvVars(ctx, nil, logger)
	if err != nil {
		return nil, fmt.Errorf("connecting to Viam app API (requires VIAM_API_KEY/VIAM_API_KEY_ID module env vars): %w", err)
	}
	partID := os.Getenv(rutils.MachinePartIDEnvVar)
	if partID == "" {
		return nil, fmt.Errorf("%s env var not set; cannot upload data without a part ID", rutils.MachinePartIDEnvVar)
	}

	name := conf.ResourceName()
	cache.RegisterConsumer(name)

	return &learningMode{
		Named:          name.AsNamed(),
		audioCache:     cache,
		yamnetSvc:      yamnetSvc,
		dataClient:     viamClient.DataClient(),
		partID:         partID,
		name:           name,
		logger:         logger,
		labelThreshold: threshold,
		targetLabels:   targetLabels,
		labelIndex:     labelIndex,
	}, nil
}

// Readings classifies the latest gated audio clip via YAMNet and uploads it
// (plus a paired spectrogram) to Viam, tagged for human review. There is no
// session concept here — each clip is handled independently, and nothing is
// ever played over a speaker.
func (l *learningMode) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	// Only one detection mode is ever active. Trained Mode (bark_monitor)
	// handles classification otherwise.
	mode, err := modestate.GetDetectionMode()
	if err != nil {
		l.logger.Warnw("failed to read detection mode, defaulting to inactive", "error", err)
		return nil, data.ErrNoCaptureToStore
	}
	if mode != modestate.DetectionModeLearning {
		return nil, data.ErrNoCaptureToStore
	}

	capture := l.audioCache.PopCapture(l.name)
	if capture == nil || len(capture.Chunks) == 0 {
		return nil, data.ErrNoCaptureToStore
	}

	sampleRate := 44100
	if capture.Chunks[0].AudioInfo != nil && capture.Chunks[0].AudioInfo.SampleRateHz > 0 {
		sampleRate = int(capture.Chunks[0].AudioInfo.SampleRateHz)
	}

	samples := spectrogramcam.ChunksToMono(capture.Chunks)
	if len(samples) == 0 {
		return nil, data.ErrNoCaptureToStore
	}

	scores, err := l.classify(ctx, samples, sampleRate)
	if err != nil {
		l.logger.Warnw("yamnet classification failed", "error", err)
		return nil, data.ErrNoCaptureToStore
	}
	tag := l.tagFromScores(scores)

	startNs := capture.Chunks[0].StartTimestampNanoseconds
	endNs := capture.Chunks[len(capture.Chunks)-1].EndTimestampNanoseconds
	imgBytes, err := spectrogramcam.RenderChunks(capture.Chunks, sampleRate, capture.CapturedAt, startNs, endNs, capture.DB)
	if err != nil {
		l.logger.Warnw("rendering spectrogram failed", "error", err)
		return nil, data.ErrNoCaptureToStore
	}

	audioBytes, err := barkmonitor.WAVBytesFromChunks(capture.Chunks)
	if err != nil {
		l.logger.Warnw("encoding audio failed", "error", err)
		return nil, data.ErrNoCaptureToStore
	}

	// Both uploads share the exact same capture timestamp, so the paired
	// spectrogram can later be found by an exact-match lookup against the
	// audio clip, without needing a dedicated correlation tag.
	uploadTimes := &[2]time.Time{capture.CapturedAt, capture.CapturedAt}

	if _, err := l.dataClient.BinaryDataCaptureUpload(ctx, audioBytes, l.partID,
		"rdk:component:audio_in", l.name.Name, "GetAudio", "wav",
		&app.BinaryDataCaptureUploadOptions{Tags: []string{tag}, DataRequestTimes: uploadTimes}); err != nil {
		l.logger.Warnw("uploading audio clip failed", "error", err)
		return nil, data.ErrNoCaptureToStore
	}
	if _, err := l.dataClient.BinaryDataCaptureUpload(ctx, imgBytes, l.partID,
		"rdk:component:camera", l.name.Name, "GetImage", "png",
		&app.BinaryDataCaptureUploadOptions{Tags: []string{tag}, DataRequestTimes: uploadTimes}); err != nil {
		l.logger.Warnw("uploading spectrogram failed", "error", err)
		return nil, data.ErrNoCaptureToStore
	}

	return map[string]interface{}{
		"tag":         tag,
		"captured_at": capture.CapturedAt.Format(time.RFC3339Nano),
	}, nil
}

// classify resamples samples (at sampleRate Hz) to YAMNet's fixed 16kHz/15600
// input and returns the raw 521-length score tensor.
func (l *learningMode) classify(ctx context.Context, samples []float64, sampleRate int) ([]float32, error) {
	waveform := resampleTo16kFixedLen(samples, sampleRate)
	inputTensor := tensor.New(tensor.WithShape(yamnetInputLen), tensor.WithBacking(waveform))

	out, err := l.yamnetSvc.Infer(ctx, ml.Tensors{yamnetInputTensorName: inputTensor})
	if err != nil {
		return nil, fmt.Errorf("Infer: %w", err)
	}
	scoresTensor, ok := out[yamnetOutputTensorName]
	if !ok {
		return nil, fmt.Errorf("no output tensor named %q", yamnetOutputTensorName)
	}
	scores, ok := scoresTensor.Data().([]float32)
	if !ok {
		return nil, fmt.Errorf("unexpected output tensor data type %T", scoresTensor.Data())
	}
	return scores, nil
}

// tagFromScores returns "unconfirmed-bark" if any target label's score clears
// the configured threshold, else "unconfirmed-no-bark".
func (l *learningMode) tagFromScores(scores []float32) string {
	for _, label := range l.targetLabels {
		idx := l.labelIndex[label]
		if idx < len(scores) && float64(scores[idx]) >= l.labelThreshold {
			return "unconfirmed-bark"
		}
	}
	return "unconfirmed-no-bark"
}

// resampleTo16kFixedLen resamples mono float64 samples at sampleRate Hz to a
// fixed-length float32 waveform at yamnetTargetRate, via linear interpolation.
// If there aren't enough source samples to fill the target length, the tail
// is held at the last available value.
func resampleTo16kFixedLen(samples []float64, sampleRate int) []float32 {
	out := make([]float32, yamnetInputLen)
	if len(samples) == 0 || sampleRate <= 0 {
		return out
	}
	ratio := float64(sampleRate) / float64(yamnetTargetRate)
	for i := 0; i < yamnetInputLen; i++ {
		srcPos := float64(i) * ratio
		i0 := int(srcPos)
		if i0 >= len(samples)-1 {
			out[i] = float32(samples[len(samples)-1])
			continue
		}
		frac := srcPos - float64(i0)
		v := samples[i0]*(1-frac) + samples[i0+1]*frac
		out[i] = float32(v)
	}
	return out
}

// parseLabelIndex parses the embedded yamnet_class_map.csv (index,mid,display_name)
// into a map from display_name to its index in the 521-length score tensor.
func parseLabelIndex(csvBytes []byte) (map[string]int, error) {
	r := csv.NewReader(bytes.NewReader(csvBytes))
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("class map has no data rows")
	}
	index := make(map[string]int, len(rows)-1)
	for i, row := range rows[1:] { // skip header
		if len(row) < 3 {
			continue
		}
		index[row[2]] = i
	}
	return index, nil
}

// DoCommand supports manual smoke-testing of a single classification.
//
//	{"command": "get_mode"} — returns the currently persisted detection mode
func (l *learningMode) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "get_mode":
		mode, err := modestate.GetDetectionMode()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"detection_mode": mode}, nil
	default:
		return nil, fmt.Errorf("unknown command %q; supported: get_mode", command)
	}
}

// Close is a no-op; there is nothing to tear down.
func (l *learningMode) Close(ctx context.Context) error {
	return nil
}
