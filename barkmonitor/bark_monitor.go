package barkmonitor

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/vision"
	rutils "go.viam.com/rdk/utils"

	filteredmic "github.com/katie-viam/oreo-watcher/filteredmic"
)

//go:embed sounds/warning.wav
var warningSoundRaw []byte

//go:embed sounds/good_boy.wav
var goodBoySound []byte

// Model is the model triple for the bark-monitor sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "bark-monitor")

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newBarkMonitor,
	})
}

const recordingDir = "/root/bark-recordings"

// Config holds the component configuration.
type Config struct {
	FilteredMic        string  `json:"filtered_mic"`
	VisionService      string  `json:"vision_service"`
	ClassifierCamera   string  `json:"classifier_camera"`    // spectrogram-cam-classifier
	GapSeconds         float64 `json:"gap_seconds"`          // gap without bark that ends a session (default 10)
	SecondWarnSeconds  float64 `json:"second_warn_seconds"`  // seconds from session start for 2nd warning (default 30)
	ThirdActionSeconds float64 `json:"third_action_seconds"` // seconds from session start for 3rd action (default 60)
	RecordingMic       string  `json:"recording_mic"`        // optional: raw mic for continuous session recording
}

// Validate ensures the config is valid and returns required dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.FilteredMic == "" {
		return nil, nil, fmt.Errorf("filtered_mic is required")
	}
	if c.VisionService == "" {
		return nil, nil, fmt.Errorf("vision_service is required")
	}
	if c.ClassifierCamera == "" {
		return nil, nil, fmt.Errorf("classifier_camera is required")
	}
	required := []string{c.FilteredMic, c.VisionService}
	if c.RecordingMic != "" {
		required = append(required, c.RecordingMic)
	}
	return required, nil, nil
}

type completedSession struct {
	Start             time.Time
	End               time.Time
	BarkCount         int
	MinDB             float64
	MaxDB             float64
	Warn1Fired        bool
	Warn2Fired        bool
	SecsAfterLastWarn float64 // seconds from last warning to session end; small = warning was effective
	ClassifyErrors    int     // classification failures during the session
	RecordingPath     string  // path to WAV file; empty if recording is disabled or failed
}

type barkMonitor struct {
	resource.Named
	resource.AlwaysRebuild

	audioCache       filteredmic.AudioCapture
	visSvc           vision.Service
	classifierCamera string
	recordingDir     string
	recordingMic     audioin.AudioIn // raw mic for continuous session recording; nil if not configured
	name             resource.Name
	logger           logging.Logger
	warningSound     []byte // pre-processed pulsed warning, ready to pipe to aplay

	gapDur    time.Duration
	warnDur   time.Duration
	actionDur time.Duration

	mu sync.Mutex

	// Active session state.
	inSession      bool
	sessionStart   time.Time
	lastBarkTime   time.Time
	lastWarnTime   time.Time
	minDB          float64
	maxDB          float64
	barkCount      int
	warn1Played    bool
	warn2Played    bool
	classifyErrors int

	// Continuous recording state (nil when not in a session).
	recordCancel   context.CancelFunc
	recordDone     chan struct{}
	recordedChunks []*audioin.AudioChunk

	// Timers (all stopped/replaced under mu).
	endTimer    *time.Timer
	warn2Timer  *time.Timer
	actionTimer *time.Timer

	// Completed session waiting to be consumed by Readings().
	pending   *completedSession
	closed    chan struct{}
	closeOnce sync.Once
}

func newBarkMonitor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	dep, ok := deps[audioin.Named(cfg.FilteredMic)]
	if !ok {
		return nil, fmt.Errorf("filtered_mic %q not found in dependencies", cfg.FilteredMic)
	}
	cache, ok := dep.(filteredmic.AudioCapture)
	if !ok {
		return nil, fmt.Errorf("dependency %q does not implement AudioCapture; make sure it is a filtered-mic component", cfg.FilteredMic)
	}

	depRaw, ok := deps[vision.Named(cfg.VisionService)]
	if !ok {
		return nil, fmt.Errorf("vision service %q not found in dependencies", cfg.VisionService)
	}
	visSvc, ok := depRaw.(vision.Service)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not a vision service", cfg.VisionService)
	}

	var recordingMic audioin.AudioIn
	if cfg.RecordingMic != "" {
		micDep, ok := deps[audioin.Named(cfg.RecordingMic)]
		if !ok {
			return nil, fmt.Errorf("recording_mic %q not found in dependencies", cfg.RecordingMic)
		}
		recordingMic, ok = micDep.(audioin.AudioIn)
		if !ok {
			return nil, fmt.Errorf("dependency %q is not an audio_in component", cfg.RecordingMic)
		}
	}

	gapSec := cfg.GapSeconds
	if gapSec <= 0 {
		gapSec = 10
	}
	warnSec := cfg.SecondWarnSeconds
	if warnSec <= 0 {
		warnSec = 30
	}
	actionSec := cfg.ThirdActionSeconds
	if actionSec <= 0 {
		actionSec = 60
	}

	pulsed, err := pulseWAV(warningSoundRaw, 3*time.Second, 400*time.Millisecond, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("processing warning sound: %w", err)
	}

	if cfg.RecordingMic != "" {
		if err := os.MkdirAll(recordingDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating recording dir %q: %w", recordingDir, err)
		}
	}

	name := conf.ResourceName()
	cache.RegisterConsumer(name)

	return &barkMonitor{
		Named:            name.AsNamed(),
		audioCache:       cache,
		visSvc:           visSvc,
		classifierCamera: cfg.ClassifierCamera,
		recordingDir:     recordingDir,
		recordingMic:     recordingMic,
		name:             name,
		logger:           logger,
		warningSound:     pulsed,
		gapDur:           time.Duration(float64(time.Second) * gapSec),
		warnDur:          time.Duration(float64(time.Second) * warnSec),
		actionDur:        time.Duration(float64(time.Second) * actionSec),
		closed:           make(chan struct{}),
	}, nil
}

// Readings classifies the latest spectrogram, updates session state, and returns
// a reading when a session completes.
func (b *barkMonitor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	capture := b.audioCache.PopCapture(b.name)

	classifications, classifyErr := b.visSvc.ClassificationsFromCamera(ctx, b.classifierCamera, 1, nil)
	if classifyErr == nil && len(classifications) > 0 {
		top := classifications[0]
		b.logger.Debugf("classification: %s %.3f", top.Label(), top.Score())
		if top.Label() == "bark" {
			db := 0.0
			if capture != nil {
				db = capture.DB
			}
			b.onBarkDetected(db)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if classifyErr != nil && b.inSession {
		b.classifyErrors++
	}

	if b.pending != nil {
		s := b.pending
		b.pending = nil
		return map[string]interface{}{
			"start_time":           s.Start.Format(time.RFC3339Nano),
			"end_time":             s.End.Format(time.RFC3339Nano),
			"duration_s":           math.Round(s.End.Sub(s.Start).Seconds()*10) / 10,
			"bark_count":           s.BarkCount,
			"min_db":               math.Round(s.MinDB*10) / 10,
			"max_db":               math.Round(s.MaxDB*10) / 10,
			"warn1_fired":          s.Warn1Fired,
			"warn2_fired":          s.Warn2Fired,
			"secs_after_last_warn": math.Round(s.SecsAfterLastWarn*10) / 10,
			"classify_errors":      s.ClassifyErrors,
			"recording_path":       s.RecordingPath,
		}, nil
	}

	return nil, data.ErrNoCaptureToStore
}

func (b *barkMonitor) onBarkDetected(db float64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	if !b.inSession {
		b.inSession = true
		b.sessionStart = now
		b.lastBarkTime = now
		b.lastWarnTime = now
		b.barkCount = 1
		b.minDB = db
		b.maxDB = db
		b.warn1Played = true
		b.warn2Played = false
		b.classifyErrors = 0
		b.startRecording()

		b.playSound(b.warningSound)
		b.warn2Timer = time.AfterFunc(b.warnDur, b.onSecondWarn)
		b.actionTimer = time.AfterFunc(b.actionDur, b.onThirdAction)
		b.endTimer = time.AfterFunc(b.gapDur, b.onSessionEnd)

		b.logger.Infof("bark session started")
		return
	}

	b.lastBarkTime = now
	b.barkCount++
	if db < b.minDB {
		b.minDB = db
	}
	if db > b.maxDB {
		b.maxDB = db
	}

	if b.endTimer != nil {
		b.endTimer.Stop()
	}
	b.endTimer = time.AfterFunc(b.gapDur, b.onSessionEnd)
}

func (b *barkMonitor) onSecondWarn() {
	select {
	case <-b.closed:
		return
	default:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.inSession {
		return
	}
	b.warn2Played = true
	b.lastWarnTime = time.Now()
	b.logger.Infof("bark session second warning (%.0fs elapsed)", time.Since(b.sessionStart).Seconds())
	b.playSound(b.warningSound)
}

func (b *barkMonitor) onThirdAction() {
	select {
	case <-b.closed:
		return
	default:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.inSession {
		return
	}
	b.logger.Infof("bark session third-action threshold reached (%.0fs elapsed) — not yet implemented", time.Since(b.sessionStart).Seconds())
	// TODO: implement third action
}

func (b *barkMonitor) onSessionEnd() {
	select {
	case <-b.closed:
		return
	default:
	}
	b.mu.Lock()

	if !b.inSession {
		b.mu.Unlock()
		return
	}
	if time.Since(b.lastBarkTime) < b.gapDur/2 {
		b.mu.Unlock()
		return
	}

	endTime := time.Now()
	session := &completedSession{
		Start:             b.sessionStart,
		End:               endTime,
		BarkCount:         b.barkCount,
		MinDB:             b.minDB,
		MaxDB:             b.maxDB,
		Warn1Fired:        b.warn1Played,
		Warn2Fired:        b.warn2Played,
		SecsAfterLastWarn: endTime.Sub(b.lastWarnTime).Seconds(),
		ClassifyErrors:    b.classifyErrors,
	}

	// Stop the continuous recorder.
	if b.recordCancel != nil {
		b.recordCancel()
		b.recordCancel = nil
	}
	recordDone := b.recordDone
	b.recordDone = nil

	b.classifyErrors = 0
	b.inSession = false
	playGoodBoy := b.warn1Played

	if b.warn2Timer != nil {
		b.warn2Timer.Stop()
	}
	if b.actionTimer != nil {
		b.actionTimer.Stop()
	}

	b.logger.Infof("bark session ended: duration=%.1fs barks=%d warn2=%v secs_after_warn=%.1f",
		endTime.Sub(b.sessionStart).Seconds(), b.barkCount, b.warn2Played, session.SecsAfterLastWarn)

	b.mu.Unlock()

	// Wait for recorder goroutine to finish, then collect audio — outside the lock.
	var audio []*audioin.AudioChunk
	if recordDone != nil {
		<-recordDone
		b.mu.Lock()
		audio = b.recordedChunks
		b.recordedChunks = nil
		b.mu.Unlock()
	}

	// Write WAV outside the lock — file I/O can be slow.
	if b.recordingDir != "" && len(audio) > 0 {
		path, err := writeSessionWAV(b.recordingDir, session.Start, session.End, audio)
		if err != nil {
			b.logger.Warnw("failed to write session recording", "error", err)
		} else {
			session.RecordingPath = path
			b.logger.Infof("session recording saved: %s", path)
		}
	}

	b.mu.Lock()
	b.pending = session
	if playGoodBoy {
		b.playSound(goodBoySound)
	}
	b.mu.Unlock()
}

// playSound pipes WAV bytes to aplay's stdin in a goroutine.
func (b *barkMonitor) playSound(wav []byte) {
	if len(wav) == 0 {
		return
	}
	closed := b.closed
	go func() {
		select {
		case <-closed:
			return
		default:
		}
		cmd := exec.Command("aplay", "-q", "-")
		cmd.Stdin = bytes.NewReader(wav)
		if err := cmd.Run(); err != nil {
			b.logger.Debugw("aplay error", "error", err)
		}
	}()
}

// Close stops all timers, cancels any in-progress recording, and prevents further sound playback.
func (b *barkMonitor) Close(ctx context.Context) error {
	b.closeOnce.Do(func() { close(b.closed) })
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range []*time.Timer{b.endTimer, b.warn2Timer, b.actionTimer} {
		if t != nil {
			t.Stop()
		}
	}
	if b.recordCancel != nil {
		b.recordCancel()
		b.recordCancel = nil
	}
	return nil
}

// startRecording begins a continuous GetAudio loop on the raw mic for the duration of the session.
// Must be called with b.mu held.
func (b *barkMonitor) startRecording() {
	if b.recordingMic == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.recordCancel = cancel
	b.recordDone = done
	b.recordedChunks = nil
	go b.runRecorder(ctx, done)
}

// runRecorder streams audio from the raw mic until ctx is cancelled, accumulating chunks under mu.
func (b *barkMonitor) runRecorder(ctx context.Context, done chan struct{}) {
	defer close(done)
	const chunkDur = 2.0
	var lastTimestamp int64
outer:
	for ctx.Err() == nil {
		if lastTimestamp == 0 {
			lastTimestamp = time.Now().UnixNano() - int64(chunkDur*float64(time.Second))
		}
		audioChan, err := b.recordingMic.GetAudio(ctx, "", chunkDur, lastTimestamp, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.logger.Debugw("recorder GetAudio error", "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for {
			select {
			case chunk, ok := <-audioChan:
				if !ok {
					continue outer
				}
				if chunk != nil {
					lastTimestamp = chunk.EndTimestampNanoseconds
					b.mu.Lock()
					b.recordedChunks = append(b.recordedChunks, chunk)
					b.mu.Unlock()
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

// writeSessionWAV assembles AudioChunks into a WAV file in dir and returns the path.
func writeSessionWAV(dir string, start, end time.Time, chunks []*audioin.AudioChunk) (string, error) {
	// Find first chunk with valid audio info.
	var info *rutils.AudioInfo
	for _, c := range chunks {
		if c != nil && c.AudioInfo != nil {
			info = c.AudioInfo
			break
		}
	}
	if info == nil {
		return "", fmt.Errorf("no AudioInfo found in chunks")
	}

	sampleRate := int(info.SampleRateHz)
	if sampleRate == 0 {
		sampleRate = 44100
	}
	numChannels := int(info.NumChannels)
	if numChannels == 0 {
		numChannels = 1
	}

	var audioFormat, bitsPerSample int
	switch info.Codec {
	case rutils.CodecPCM32Float:
		audioFormat = 3
		bitsPerSample = 32
	default: // PCM16
		audioFormat = 1
		bitsPerSample = 16
	}

	blockAlign := numChannels * bitsPerSample / 8
	byteRate := sampleRate * blockAlign

	// Concatenate all PCM data.
	var totalDataSize int
	for _, c := range chunks {
		if c != nil {
			totalDataSize += len(c.AudioData)
		}
	}

	dur := int(end.Sub(start).Seconds())
	filename := fmt.Sprintf("bark_%s_%ds.wav", start.Format("2006-01-02_15-04-05"), dur)
	path := filepath.Join(dir, filename)

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Write RIFF/WAVE header.
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+totalDataSize))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], uint16(audioFormat))
	binary.LittleEndian.PutUint16(hdr[22:], uint16(numChannels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:], uint16(bitsPerSample))
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(totalDataSize))

	if _, err := f.Write(hdr); err != nil {
		return "", err
	}
	for _, c := range chunks {
		if c != nil && len(c.AudioData) > 0 {
			if _, err := f.Write(c.AudioData); err != nil {
				return "", err
			}
		}
	}

	return path, nil
}

// pulseWAV trims a WAV to totalDur and applies a pulsing gate: audio alternates
// between onDur of sound (looping the source if shorter) and offDur of silence.
func pulseWAV(src []byte, totalDur, onDur, offDur time.Duration) ([]byte, error) {
	if len(src) < 12 || string(src[0:4]) != "RIFF" || string(src[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a valid WAV file")
	}

	sampleRate := int(binary.LittleEndian.Uint32(src[24:]))
	numChannels := int(binary.LittleEndian.Uint16(src[22:]))
	bitsPerSample := int(binary.LittleEndian.Uint16(src[34:]))
	blockAlign := numChannels * (bitsPerSample / 8)

	pcm, err := wavDataChunk(src)
	if err != nil {
		return nil, err
	}
	srcFrames := len(pcm) / blockAlign
	if srcFrames == 0 {
		return nil, fmt.Errorf("WAV has no audio data")
	}

	totalFrames := int(totalDur.Seconds() * float64(sampleRate))
	onFrames := int(onDur.Seconds() * float64(sampleRate))
	offFrames := int(offDur.Seconds() * float64(sampleRate))

	out := make([]byte, totalFrames*blockAlign) // zero-filled = silence

	dst := 0
	for dst < totalFrames {
		for i := 0; i < onFrames && dst < totalFrames; i++ {
			srcOff := (dst % srcFrames) * blockAlign
			dstOff := dst * blockAlign
			copy(out[dstOff:dstOff+blockAlign], pcm[srcOff:srcOff+blockAlign])
			dst++
		}
		dst += offFrames
	}

	dataSize := len(out)
	result := make([]byte, 44+dataSize)
	copy(result[0:], "RIFF")
	binary.LittleEndian.PutUint32(result[4:], uint32(36+dataSize))
	copy(result[8:], "WAVE")
	copy(result[12:], "fmt ")
	binary.LittleEndian.PutUint32(result[16:], 16)
	binary.LittleEndian.PutUint16(result[20:], 1) // PCM
	binary.LittleEndian.PutUint16(result[22:], uint16(numChannels))
	binary.LittleEndian.PutUint32(result[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(result[28:], uint32(sampleRate*numChannels*bitsPerSample/8))
	binary.LittleEndian.PutUint16(result[32:], uint16(blockAlign))
	binary.LittleEndian.PutUint16(result[34:], uint16(bitsPerSample))
	copy(result[36:], "data")
	binary.LittleEndian.PutUint32(result[40:], uint32(dataSize))
	copy(result[44:], out)
	return result, nil
}

// wavDataChunk scans a WAV file for the "data" chunk and returns its PCM bytes.
func wavDataChunk(wav []byte) ([]byte, error) {
	pos := 12
	for pos+8 <= len(wav) {
		id := string(wav[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(wav[pos+4:]))
		pos += 8
		if id == "data" {
			end := pos + size
			if end > len(wav) {
				end = len(wav)
			}
			return wav[pos:end], nil
		}
		pos += size
		if size%2 != 0 {
			pos++
		}
	}
	return nil, fmt.Errorf("no data chunk found in WAV")
}
