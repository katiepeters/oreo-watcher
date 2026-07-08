package barkmonitor

import (
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
	"go.viam.com/rdk/components/audioout"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/video"
	"go.viam.com/rdk/services/vision"
	rutils "go.viam.com/rdk/utils"

	filteredmic "github.com/katie-viam/oreo-watcher/filteredmic"
)

//go:embed sounds/leave_it.wav
var leaveItSound []byte

//go:embed sounds/good_job.wav
var goodJobSound []byte

// Model is the model triple for the bark-monitor sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "bark-monitor")

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newBarkMonitor,
	})
}

const recordingDir = "/root/bark-recordings"

type namedVideoSvc struct {
	name string
	svc  video.Service
}

// Config holds the component configuration.
type Config struct {
	FilteredMic        string   `json:"filtered_mic"`
	VisionService      string   `json:"vision_service"`
	ClassifierCamera   string   `json:"classifier_camera"`    // spectrogram-cam-classifier
	GapSeconds         float64  `json:"gap_seconds"`          // gap without bark that ends a session (default 10)
	SecondWarnSeconds  float64  `json:"second_warn_seconds"`  // seconds from session start for 2nd warning (default 30)
	ThirdActionSeconds float64  `json:"third_action_seconds"` // seconds from session start for 3rd action (default 60)
	RecordingMic       string   `json:"recording_mic"`        // optional: raw mic for continuous session recording
	VideoServices      []string `json:"video_services"`       // optional: video service names to clip during sessions
	Speaker            string   `json:"speaker"`              // optional: audio_out component for playing warning sounds
	ClipPadSeconds     float64  `json:"clip_pad_seconds"`     // seconds to include before/after session in clips (default 3)
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
	if c.Speaker != "" {
		required = append(required, c.Speaker)
	}
	return required, c.VideoServices, nil
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
	ClassifyErrors    int    // classification failures during the session
}

type barkMonitor struct {
	resource.Named
	resource.AlwaysRebuild

	audioCache       filteredmic.AudioCapture
	visSvc           vision.Service
	classifierCamera string
	recordingDir     string
	recordingMic     audioin.AudioIn   // raw mic for continuous session recording; nil if not configured
	speaker          audioout.AudioOut // audio_out component for playing sounds; nil if not configured
	videoServices    []namedVideoSvc   // video services to clip during sessions
	clipPad          time.Duration     // extra time to include before/after session in clips
	name             resource.Name
	logger           logging.Logger
	warn1Sound []byte
	warn2Sound []byte

	gapDur    time.Duration
	warnDur   time.Duration
	actionDur time.Duration

	mu sync.Mutex

	// Active session state.
	inSession    bool
	sessionStart time.Time
	lastBarkTime time.Time
	lastWarnTime time.Time
	minDB        float64
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
	endTimer     *time.Timer
	warn2Timer   *time.Timer
	actionTimer  *time.Timer
	goodJobTimer *time.Timer // fires at gapDur after last bark to play the good-job sound

	// Cancel func and token for an in-progress good-job playback; nil/0 when not playing.
	goodJobCancel context.CancelFunc
	goodJobToken  int

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

	var speaker audioout.AudioOut
	if cfg.Speaker != "" {
		spDep, ok := deps[audioout.Named(cfg.Speaker)]
		if !ok {
			return nil, fmt.Errorf("speaker %q not found in dependencies", cfg.Speaker)
		}
		speaker, ok = spDep.(audioout.AudioOut)
		if !ok {
			return nil, fmt.Errorf("dependency %q is not an audio_out component", cfg.Speaker)
		}
	}

	var videoServices []namedVideoSvc
	for _, svcName := range cfg.VideoServices {
		d, ok := deps[video.Named(svcName)]
		if !ok {
			logger.Warnw("video service not available, skipping", "service", svcName)
			continue
		}
		svc, ok := d.(video.Service)
		if !ok {
			logger.Warnw("dependency is not a video service, skipping", "service", svcName)
			continue
		}
		videoServices = append(videoServices, namedVideoSvc{name: svcName, svc: svc})
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
	clipPadSec := cfg.ClipPadSeconds
	if clipPadSec <= 0 {
		clipPadSec = 3
	}

	if cfg.RecordingMic != "" || len(cfg.VideoServices) > 0 {
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
		speaker:          speaker,
		videoServices:    videoServices,
		name:             name,
		logger:           logger,
		warn1Sound:       leaveItSound,
		warn2Sound:       leaveItSound,
		gapDur:           time.Duration(float64(time.Second) * gapSec),
		warnDur:          time.Duration(float64(time.Second) * warnSec),
		actionDur:        time.Duration(float64(time.Second) * actionSec),
		clipPad:          time.Duration(float64(time.Second) * clipPadSec),
		closed:           make(chan struct{}),
	}, nil
}

// Readings classifies the latest spectrogram, updates session state, and returns
// a reading when a session completes.
func (b *barkMonitor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	// Check for a completed session BEFORE calling ClassificationsFromCamera.
	// The data manager context can expire during that slow call; if we checked
	// pending only afterward the reading would be silently discarded.
	b.mu.Lock()
	if b.pending != nil {
		s := b.pending
		b.pending = nil
		b.mu.Unlock()
		return sessionReading(s), nil
	}
	b.mu.Unlock()

	capture := b.audioCache.PopCapture(b.name)

	classifications, classifyErr := b.visSvc.ClassificationsFromCamera(ctx, b.classifierCamera, 1, nil)
	if classifyErr == nil && len(classifications) > 0 {
		top := classifications[0]
		b.logger.Debugf("classification: %s %.3f", top.Label(), top.Score())
		if top.Label() == "bark" {
			db := 0.0
			var barkStart time.Time
			if capture != nil {
				db = capture.DB
				if len(capture.Chunks) > 0 {
					barkStart = chunkStartTime(capture.Chunks[0])
				}
			}
			b.onBarkDetected(db, barkStart)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if classifyErr != nil && b.inSession {
		b.classifyErrors++
	}

	// Check again: a session may have ended while classification was running.
	if b.pending != nil {
		s := b.pending
		b.pending = nil
		return sessionReading(s), nil
	}

	return nil, data.ErrNoCaptureToStore
}

func sessionReading(s *completedSession) map[string]interface{} {
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
	}
}

func (b *barkMonitor) onBarkDetected(db float64, barkStart time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()

	if !b.inSession {
		b.inSession = true
		if !barkStart.IsZero() {
			b.sessionStart = barkStart
		} else {
			b.sessionStart = now
		}
		b.lastBarkTime = now
		b.lastWarnTime = now
		b.barkCount = 1
		b.minDB = db
		b.maxDB = db
		b.warn1Played = true
		b.warn2Played = false
		b.classifyErrors = 0
		b.startRecording()

		b.endTimer = time.AfterFunc(b.gapDur/2, b.onSessionEnd)
		if b.goodJobTimer != nil {
			b.goodJobTimer.Stop()
			b.goodJobTimer = nil
		}
		if b.goodJobCancel != nil {
			b.goodJobCancel()
			b.goodJobCancel = nil
		}
		go func() {
			b.playSound(context.Background(), b.warn1Sound)
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.inSession {
				b.warn2Timer = time.AfterFunc(b.warnDur, b.onSecondWarn)
			}
		}()

		b.logger.Infof("bark session started — warning 1 emitted")
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
	if time.Since(b.lastBarkTime) > b.gapDur/2 {
		// Oreo has been quiet long enough that the session is ending — skip warning 2.
		return
	}
	b.warn2Played = true
	b.lastWarnTime = time.Now()
	b.logger.Infof("bark session warning 2 emitted (%.0fs elapsed)", time.Since(b.sessionStart).Seconds())
	go func() {
		b.playSound(context.Background(), b.warn2Sound)
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.inSession {
			b.actionTimer = time.AfterFunc(b.actionDur, b.onThirdAction)
		}
	}()
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

	endTime := b.lastBarkTime.Add(b.gapDur / 2)
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

	// Stop the continuous recorder after clipPad seconds of post-roll.
	cancel := b.recordCancel
	b.recordCancel = nil
	recordDone := b.recordDone
	b.recordDone = nil
	if cancel != nil {
		time.AfterFunc(b.clipPad, cancel)
	}

	b.classifyErrors = 0
	b.inSession = false
	scheduleGoodJob := b.warn1Played

	if b.warn2Timer != nil {
		b.warn2Timer.Stop()
	}
	if b.actionTimer != nil {
		b.actionTimer.Stop()
	}

	b.logger.Infof("bark session ended: duration=%.1fs barks=%d warn2=%v secs_after_warn=%.1f",
		endTime.Sub(b.sessionStart).Seconds(), b.barkCount, b.warn2Played, session.SecsAfterLastWarn)

	// Set pending while still holding b.mu (acquired at the top of this function).
	b.pending = session
	if scheduleGoodJob {
		if b.goodJobTimer != nil {
			b.goodJobTimer.Stop()
		}
		b.goodJobTimer = time.AfterFunc(b.gapDur/2, b.onGoodJob)
	}
	b.mu.Unlock()

	clipStart := session.Start.Add(-b.clipPad)
	clipEnd := session.End.Add(b.clipPad)
	baseFilename := fmt.Sprintf("bark_%s_%ds", session.Start.Format("2006-01-02_15-04-05"), int(session.End.Sub(session.Start).Seconds()))

	// Save WAV and video clips in the background — slow I/O should not block the reading.
	// Video fetch is delayed by clipPad so that clipEnd is in the past before we request it.
	go func() {
		// Wait for post-roll to elapse so the full clip range is recorded before requesting.
		time.Sleep(b.clipPad)

		// Fetch video clips in parallel with the audio post-roll.
		type videoResult struct {
			path string
			name string
		}
		videoCh := make(chan videoResult, len(b.videoServices))
		for _, vsvc := range b.videoServices {
			vsvc := vsvc
			go func() {
				tmpPath, err := b.saveVideoClip(vsvc, clipStart, clipEnd, baseFilename, "/tmp")
				if err != nil {
					b.logger.Warnw("failed to fetch video clip", "service", vsvc.name, "error", err)
					videoCh <- videoResult{}
					return
				}
				b.logger.Infof("video clip fetched: %s", tmpPath)
				videoCh <- videoResult{path: tmpPath, name: vsvc.name}
			}()
		}

		// Wait for audio post-roll to finish, then write WAV.
		var audio []*audioin.AudioChunk
		if recordDone != nil {
			<-recordDone
			b.mu.Lock()
			audio = b.recordedChunks
			b.recordedChunks = nil
			b.mu.Unlock()
		}

		b.logger.Infof("recorder collected %d chunks", len(audio))

		var tmpWavPath string
		var audioOffsetSec float64
		if len(audio) > 0 {
			audioOffsetSec = chunkAudioOffset(audio[0], clipStart)
			b.logger.Infof("audio offset vs clip start: %.3fs (negative = audio starts before clip)", audioOffsetSec)
			path, err := writeSessionWAV("/tmp", session.Start, session.End, audio)
			if err != nil {
				b.logger.Warnw("failed to write session WAV", "error", err)
			} else {
				tmpWavPath = path
				b.logger.Infof("session WAV written: %s", tmpWavPath)
			}
		}

		// Collect video results and mux in audio.
		for range b.videoServices {
			res := <-videoCh
			if res.path == "" {
				continue
			}
			if tmpWavPath != "" {
				if err := muxAudioIntoVideo(res.path, tmpWavPath, audioOffsetSec); err != nil {
					b.logger.Warnw("failed to mux audio into video", "service", res.name, "error", err)
				} else {
					b.logger.Infof("audio muxed into %s", res.path)
				}
			}
			finalPath := filepath.Join(b.recordingDir, filepath.Base(res.path))
			if err := os.Rename(res.path, finalPath); err != nil {
				b.logger.Warnw("failed to move video to sync dir", "service", res.name, "error", err)
			} else {
				b.logger.Infof("video ready for upload: %s", finalPath)
			}
		}

		// Move WAV to sync dir after all muxing is done.
		if tmpWavPath != "" {
			finalPath := filepath.Join(b.recordingDir, filepath.Base(tmpWavPath))
			if err := os.Rename(tmpWavPath, finalPath); err != nil {
				b.logger.Warnw("failed to move WAV to sync dir", "error", err)
			} else {
				b.logger.Infof("WAV ready for upload: %s", finalPath)
			}
		}
	}()
}

// onGoodJob fires gapDur after the last bark. It plays the good-job sound confirming the dog
// stopped barking for a full gap period. Cancelled (goodJobTimer stopped) if barking resumes.
func (b *barkMonitor) onGoodJob() {
	select {
	case <-b.closed:
		return
	default:
	}
	b.mu.Lock()
	if b.inSession {
		b.mu.Unlock()
		return
	}
	goodJobCtx, goodJobCancel := context.WithCancel(context.Background())
	b.goodJobToken++
	myToken := b.goodJobToken
	b.goodJobCancel = goodJobCancel
	b.mu.Unlock()

	go func() {
		defer func() {
			goodJobCancel()
			b.mu.Lock()
			if b.goodJobToken == myToken {
				b.goodJobCancel = nil
			}
			b.mu.Unlock()
		}()
		b.playSound(goodJobCtx, goodJobSound)
	}()
}

// playSound plays WAV bytes via the configured speaker component. Blocks until playback completes.
// Must NOT be called with b.mu held — callers should launch a goroutine.
func (b *barkMonitor) playSound(ctx context.Context, wav []byte) {
	if len(wav) == 0 {
		b.logger.Warnw("playSound called with empty audio data")
		return
	}
	if b.speaker == nil {
		b.logger.Warnw("playSound: no speaker component configured")
		return
	}
	select {
	case <-b.closed:
		return
	default:
	}
	pcm, info, err := wavToPCM(wav)
	if err != nil {
		b.logger.Warnw("failed to parse WAV for playback", "error", err)
		return
	}
	if err := b.speaker.Play(ctx, pcm, info, nil); err != nil && ctx.Err() == nil {
		b.logger.Warnw("speaker play error", "error", err)
	}
}

// wavToPCM extracts raw PCM samples and audio info from a WAV byte slice.
func wavToPCM(wav []byte) ([]byte, *rutils.AudioInfo, error) {
	if len(wav) < 36 {
		return nil, nil, fmt.Errorf("WAV too short")
	}
	numChannels := uint32(binary.LittleEndian.Uint16(wav[22:]))
	sampleRate := binary.LittleEndian.Uint32(wav[24:])
	pcm, err := wavDataChunk(wav)
	if err != nil {
		return nil, nil, err
	}
	return pcm, &rutils.AudioInfo{
		Codec:        rutils.CodecPCM16,
		SampleRateHz: int32(sampleRate),
		NumChannels:  int32(numChannels),
	}, nil
}

// DoCommand supports manual testing of sounds via the Viam control panel or API.
// Supported commands:
//
//	{"command": "play_leave_it"} — play the warning sound
//	{"command": "play_good_job"} — play the good boy sound
func (b *barkMonitor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "play_leave_it":
		go b.playSound(context.Background(), b.warn1Sound)
		return map[string]interface{}{"ok": true}, nil
	case "play_good_job":
		go b.playSound(context.Background(), goodJobSound)
		return map[string]interface{}{"ok": true}, nil
	default:
		return nil, fmt.Errorf("unknown command %q; supported: play_leave_it, play_good_job", command)
	}
}

// Close stops all timers, cancels any in-progress recording, and prevents further sound playback.
func (b *barkMonitor) Close(ctx context.Context) error {
	b.closeOnce.Do(func() { close(b.closed) })
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range []*time.Timer{b.endTimer, b.warn2Timer, b.actionTimer, b.goodJobTimer} {
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
	go b.runRecorder(ctx, done, b.sessionStart)
}

// runRecorder streams audio from the raw mic until ctx is cancelled, accumulating chunks under mu.
func (b *barkMonitor) runRecorder(ctx context.Context, done chan struct{}, sessionStart time.Time) {
	defer close(done)
	const chunkDur = 2.0
	var lastTimestamp int64
outer:
	for ctx.Err() == nil {
		if lastTimestamp == 0 {
			// Start from clipPad before session start to capture pre-roll audio.
			lastTimestamp = sessionStart.Add(-b.clipPad).UnixNano() - int64(chunkDur*float64(time.Second))
		}
		audioChan, err := b.recordingMic.GetAudio(ctx, "pcm16", chunkDur, lastTimestamp, nil)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.logger.Warnw("recorder GetAudio error", "error", err)
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

// saveVideoClip fetches a video clip for the session time range and writes it as an MP4 in dir.
func (b *barkMonitor) saveVideoClip(vsvc namedVideoSvc, start, end time.Time, baseFilename, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ch, err := vsvc.svc.GetVideo(ctx, start, end, "", "mp4", nil)
	if err != nil {
		return "", fmt.Errorf("GetVideo: %w", err)
	}

	path := filepath.Join(dir, baseFilename+"_"+vsvc.name+".mp4")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return path, nil
			}
			if chunk != nil && len(chunk.Data) > 0 {
				if _, err := f.Write(chunk.Data); err != nil {
					return "", err
				}
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// chunkStartTime returns the wall-clock time of the first sample in the chunk,
// derived from EndTimestampNanoseconds and the chunk's PCM frame count.
// Returns zero time if the chunk lacks the necessary metadata.
func chunkStartTime(c *audioin.AudioChunk) time.Time {
	if c == nil || c.AudioInfo == nil || c.AudioInfo.SampleRateHz == 0 || len(c.AudioData) == 0 {
		return time.Time{}
	}
	ch := int64(c.AudioInfo.NumChannels)
	if ch == 0 {
		ch = 1
	}
	var bytesPerSample int64 = 2 // pcm16 default
	if c.AudioInfo.Codec == rutils.CodecPCM32Float {
		bytesPerSample = 4
	}
	numFrames := int64(len(c.AudioData)) / (ch * bytesPerSample)
	if numFrames == 0 {
		return time.Time{}
	}
	durationNs := numFrames * int64(time.Second) / int64(c.AudioInfo.SampleRateHz)
	return time.Unix(0, c.EndTimestampNanoseconds-durationNs)
}

// chunkAudioOffset returns how many seconds the chunk's first sample is offset
// from referenceTime. Negative = audio starts before reference; positive = after.
func chunkAudioOffset(c *audioin.AudioChunk, referenceTime time.Time) float64 {
	t := chunkStartTime(c)
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()-referenceTime.UnixNano()) / float64(time.Second)
}

// muxAudioIntoVideo runs ffmpeg to mix audioPath into videoPath in place.
// audioOffsetSec is the offset of the audio's first sample relative to the
// video's requested start: negative = audio starts before video (trim audio),
// positive = audio starts after video (delay audio).
func muxAudioIntoVideo(videoPath, audioPath string, audioOffsetSec float64) error {
	tmp := videoPath + ".tmp.mp4"
	var args []string
	args = append(args, "-y")

	const threshold = 0.05 // ignore sub-50ms offsets
	switch {
	case audioOffsetSec < -threshold:
		// Audio starts before video: skip the leading audio so both align at session start.
		args = append(args,
			"-ss", fmt.Sprintf("%.3f", -audioOffsetSec),
			"-i", audioPath,
			"-i", videoPath,
			"-c:v", "copy", "-c:a", "aac",
			"-map", "1:v:0", "-map", "0:a:0", "-shortest", tmp)
	case audioOffsetSec > threshold:
		// Audio starts after video: delay the audio stream by the gap.
		args = append(args,
			"-i", videoPath,
			"-itsoffset", fmt.Sprintf("%.3f", audioOffsetSec),
			"-i", audioPath,
			"-c:v", "copy", "-c:a", "aac",
			"-map", "0:v:0", "-map", "1:a:0", "-shortest", tmp)
	default:
		args = append(args,
			"-i", videoPath, "-i", audioPath,
			"-c:v", "copy", "-c:a", "aac",
			"-map", "0:v:0", "-map", "1:a:0", "-shortest", tmp)
	}

	cmd := exec.Command("ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%w\n%s", err, out)
	}
	return os.Rename(tmp, videoPath)
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
