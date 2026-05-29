package filteredmic

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sync"
	"syscall"
	"time"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	rutils "go.viam.com/rdk/utils"
)

// CapturedAudio holds the chunks from a completed audio capture.
type CapturedAudio struct {
	Chunks     []*audioin.AudioChunk
	DB         float64   // dBFS level detected on the first chunk
	CapturedAt time.Time // wall-clock time when the audio stream finished
}

// AudioCapture is implemented by components that cache their last capture.
// Each consumer registers a named slot; new captures populate every slot.
// PopCapture returns the pending capture for that slot and clears it, so the
// underlying data is eligible for GC once every registered consumer has popped.
type AudioCapture interface {
	RegisterConsumer(name resource.Name)
	PopCapture(name resource.Name) *CapturedAudio
}

// Model is the model triple for the filtered-mic component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "filtered-mic")

func init() {
	resource.RegisterComponent(audioin.API, Model, resource.Registration[audioin.AudioIn, *Config]{
		Constructor: newFilteredMic,
	})
}

// Config holds the component configuration.
type Config struct {
	UnderlyingMic        string  `json:"underlying_mic"`
	MinDB                float64 `json:"min_db"`
	MaxConsecutiveErrors int     `json:"max_consecutive_errors"` // restart viam-server after this many consecutive mic errors (default 30)
}

// Validate ensures the config is valid and returns required dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.UnderlyingMic == "" {
		return nil, nil, fmt.Errorf("underlying_mic is required")
	}
	if c.MinDB > 0 {
		return nil, nil, fmt.Errorf("min_db must be <= 0 (dBFS scale: 0 is maximum loudness, typical values are -40 to -10)")
	}
	return []string{c.UnderlyingMic}, nil, nil
}

type filteredMic struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	underlyingMic        audioin.AudioIn
	minDB                float64
	maxConsecutiveErrors int
	logger               logging.Logger

	mu               sync.Mutex
	slots            map[resource.Name]*CapturedAudio
	consecutiveErrors int
}

func newFilteredMic(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (audioin.AudioIn, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	dep, ok := deps[audioin.Named(cfg.UnderlyingMic)]
	if !ok {
		return nil, fmt.Errorf("underlying mic %q not found in dependencies", cfg.UnderlyingMic)
	}
	mic, ok := dep.(audioin.AudioIn)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not an audio_in component", cfg.UnderlyingMic)
	}

	minDB := cfg.MinDB
	if minDB == 0 {
		minDB = -40
	}
	maxErrs := cfg.MaxConsecutiveErrors
	if maxErrs <= 0 {
		maxErrs = 30
	}

	return &filteredMic{
		Named:                conf.ResourceName().AsNamed(),
		underlyingMic:        mic,
		minDB:                minDB,
		maxConsecutiveErrors: maxErrs,
		logger:               logger,
		slots:                make(map[resource.Name]*CapturedAudio),
	}, nil
}

func (f *filteredMic) GetAudio(
	ctx context.Context,
	codec string,
	durationSeconds float32,
	previousTimestampNs int64,
	extra map[string]interface{},
) (chan *audioin.AudioChunk, error) {
	// Always request audio relative to now rather than passing previousTimestampNs through.
	// If we passed previousTimestampNs from the data manager, it would go stale whenever
	// ErrNoCaptureToStore is returned (the data manager only advances it on successful captures),
	// eventually falling outside the mic's 30-second rolling buffer.
	freshTimestamp := time.Now().UnixNano() - int64(float64(durationSeconds)*float64(time.Second)) - int64(time.Second)
	audioChan, err := f.underlyingMic.GetAudio(ctx, codec, durationSeconds, freshTimestamp, extra)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		f.mu.Lock()
		f.consecutiveErrors++
		n := f.consecutiveErrors
		f.mu.Unlock()
		f.logger.Debugw("underlying mic GetAudio error", "error", err, "consecutive", n)
		if n >= f.maxConsecutiveErrors {
			f.logger.Errorw("mic appears stuck — restarting viam-server", "consecutive_errors", n)
			// Signal viam-server (our parent process) to exit; viam-agent will restart it.
			syscall.Kill(os.Getppid(), syscall.SIGTERM)
		}
		return nil, data.ErrNoCaptureToStore
	}
	f.mu.Lock()
	f.consecutiveErrors = 0
	f.mu.Unlock()

	// Read the first chunk to check the audio level before committing to stream.
	var firstChunk *audioin.AudioChunk
	select {
	case chunk, ok := <-audioChan:
		if !ok {
			return nil, data.ErrNoCaptureToStore
		}
		firstChunk = chunk
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	db := chunkToDb(firstChunk)
	f.logger.Debugf("Audio level: %.1f dB (threshold: %.1f dB, codec: %s, data bytes: %d)", db, f.minDB, firstChunk.AudioInfo.Codec, len(firstChunk.AudioData))

	if db < f.minDB {
		return nil, data.ErrNoCaptureToStore
	}

	f.logger.Infof("Audio level %.1f dB exceeded threshold %.1f dB, capturing", db, f.minDB)

	// Level is above threshold — forward the first chunk and the rest of the stream,
	// collecting a copy of every chunk so the waveform cam can render the exact same audio.
	outChan := make(chan *audioin.AudioChunk, 1)
	outChan <- firstChunk

	go func() {
		collected := []*audioin.AudioChunk{firstChunk}
		defer func() {
			close(outChan)
			capture := &CapturedAudio{Chunks: collected, DB: db, CapturedAt: time.Now()}
			f.mu.Lock()
			for name := range f.slots {
				f.slots[name] = capture
			}
			f.mu.Unlock()
		}()
		for {
			select {
			case chunk, ok := <-audioChan:
				if !ok {
					return
				}
				collected = append(collected, chunk)
				select {
				case outChan <- chunk:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return outChan, nil
}

// RegisterConsumer creates a named slot for a consumer. Safe to call multiple
// times with the same name (e.g. on component rebuild) — the existing slot is kept.
func (f *filteredMic) RegisterConsumer(name resource.Name) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.slots[name]; !exists {
		f.slots[name] = nil
	}
}

// PopCapture returns the pending capture for this consumer and clears the slot.
// Returns nil if no new capture has arrived since the last pop.
func (f *filteredMic) PopCapture(name resource.Name) *CapturedAudio {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.slots[name]
	f.slots[name] = nil
	return c
}

func (f *filteredMic) Properties(ctx context.Context, extra map[string]interface{}) (rutils.Properties, error) {
	return f.underlyingMic.Properties(ctx, extra)
}

// chunkToDb calculates dBFS from raw PCM audio data.
// Supports float32 and int16 PCM; defaults to int16 for unknown codecs.
func chunkToDb(chunk *audioin.AudioChunk) float64 {
	if chunk == nil || len(chunk.AudioData) == 0 || chunk.AudioInfo == nil {
		return -100.0
	}

	var sumSquares float64
	var numSamples int

	switch chunk.AudioInfo.Codec {
	case rutils.CodecPCM32Float:
		numSamples = len(chunk.AudioData) / 4
		for i := 0; i+3 < len(chunk.AudioData); i += 4 {
			bits := binary.LittleEndian.Uint32(chunk.AudioData[i:])
			s := math.Float32frombits(bits)
			sumSquares += float64(s) * float64(s)
		}
	default: // PCM16
		numSamples = len(chunk.AudioData) / 2
		for i := 0; i+1 < len(chunk.AudioData); i += 2 {
			s := int16(binary.LittleEndian.Uint16(chunk.AudioData[i:]))
			normalized := float64(s) / 32768.0
			sumSquares += normalized * normalized
		}
	}

	if numSamples == 0 {
		return -100.0
	}
	rms := math.Sqrt(sumSquares / float64(numSamples))
	if rms == 0 {
		return -100.0
	}
	return 20.0 * math.Log10(rms)
}
