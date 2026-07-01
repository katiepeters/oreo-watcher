package activitymonitor

import (
	"context"
	"fmt"
	"image"
	"math"
	"sync"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/vision"
)

// Model is the model triple for the activity-monitor sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "activity-monitor")

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newActivityMonitor,
	})
}

// ActivityCapture is the interface used by movement-monitor and sleep-monitor to
// consume completed sessions from the activity-monitor without coupling to its type.
type ActivityCapture interface {
	PopMovementSession() *CompletedSession
	PopSleepSession() *CompletedSession
}

// CompletedSession holds the start and end time of a single movement or sleep session.
type CompletedSession struct {
	Start time.Time
	End   time.Time
}

// Reading returns the session as a sensor reading map.
func (s *CompletedSession) Reading() map[string]interface{} {
	return map[string]interface{}{
		"start_time": s.Start.Format(time.RFC3339Nano),
		"end_time":   s.End.Format(time.RFC3339Nano),
		"duration_s": math.Round(s.End.Sub(s.Start).Seconds()*10) / 10,
	}
}

const (
	defaultMinBedOverlap            = 0.2
	defaultMaxOreoToBedSizeRatio    = 0.5
	defaultSleepMotionThreshold     = 8.0
	defaultBoundingBoxPadding       = 0.2
	defaultMinStillSeconds          = 30.0
	defaultReferenceRefreshInterval = 300
	defaultNoDetectionGraceSeconds  = 10.0

	// movementGapDur is how long Oreo must be back on his bed before a movement
	// session is considered ended (absorbs brief missed detections).
	movementGapDur = 5 * time.Second

	// pollInterval is how often the activity monitor samples the vision services.
	pollInterval = 2 * time.Second
)

// Config holds the component configuration.
type Config struct {
	OreoVisionService        string  `json:"oreo_vision_service"`
	SceneVisionService       string  `json:"scene_vision_service"`
	Camera1                  string  `json:"camera_1"`
	Camera2                  string  `json:"camera_2"`
	MinBedOverlap            float64 `json:"min_bed_overlap"`
	MaxOreoToBedSizeRatio    float64 `json:"max_oreo_to_bed_size_ratio"`
	SleepMotionThreshold     float64 `json:"sleep_motion_threshold"`
	BoundingBoxPadding       float64 `json:"bounding_box_padding"`
	MinStillSeconds          float64 `json:"min_still_seconds"`
	ReferenceRefreshInterval int     `json:"reference_refresh_interval"`
	NoDetectionGraceSeconds  float64 `json:"no_detection_grace_seconds"`
}

// Validate ensures required fields are present and returns dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.OreoVisionService == "" {
		return nil, nil, fmt.Errorf("oreo_vision_service is required")
	}
	if c.SceneVisionService == "" {
		return nil, nil, fmt.Errorf("scene_vision_service is required")
	}
	if c.Camera1 == "" {
		return nil, nil, fmt.Errorf("camera_1 is required")
	}
	if c.Camera2 == "" {
		return nil, nil, fmt.Errorf("camera_2 is required")
	}
	return []string{c.OreoVisionService, c.SceneVisionService, c.Camera1, c.Camera2}, nil, nil
}

type activityMonitor struct {
	resource.Named
	resource.AlwaysRebuild

	oreoVisSvc  vision.Service
	sceneVisSvc vision.Service
	cam1        camera.Camera
	cam2        camera.Camera
	cam1Name    string
	cam2Name    string
	logger      logging.Logger

	minBedOverlap            float64
	maxOreoToBedSizeRatio    float64
	sleepMotionThreshold     float64
	boundingBoxPadding       float64
	minStillDur              time.Duration
	referenceRefreshInterval int
	noDetectionGraceDur      time.Duration

	mu sync.Mutex

	// Per-camera scene cache — furniture doesn't move, refreshed only when empty.
	cachedBedBox1   *image.Rectangle
	cachedTableBox1 *image.Rectangle
	cachedBedBox2   *image.Rectangle
	cachedTableBox2 *image.Rectangle

	// Per-camera reference frames anchored to when Oreo first became still.
	// All subsequent frames are compared to this image (not the previous frame)
	// so that gradual positional drift is caught. Cleared on any movement or off-bed.
	referenceImg1    image.Image
	stillFrameCount1 int
	referenceImg2    image.Image
	stillFrameCount2 int

	// Live state (written by tick, read by DoCommand).
	onBed   bool
	isStill bool

	// When Oreo was first not detected by either camera; used for the no-detection grace period.
	oreoNotDetectedSince *time.Time

	// Movement session tracking.
	movementStart         *time.Time // non-nil while a movement session is active
	onBedConsecutiveSince *time.Time // when Oreo was first seen back on bed after moving

	// Sleep session tracking.
	stillSince *time.Time // when Oreo first became still on bed
	sleepStart *time.Time // non-nil while a sleep session is active (backdated to stillSince)

	// Completed session queues, consumed by movement-monitor and sleep-monitor.
	pendingMovement []*CompletedSession
	pendingSleep    []*CompletedSession

	closed    chan struct{}
	closeOnce sync.Once
}

func newActivityMonitor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	oreoVisDep, ok := deps[vision.Named(cfg.OreoVisionService)]
	if !ok {
		return nil, fmt.Errorf("oreo_vision_service %q not found in dependencies", cfg.OreoVisionService)
	}
	oreoVisSvc, ok := oreoVisDep.(vision.Service)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not a vision service", cfg.OreoVisionService)
	}

	sceneVisDep, ok := deps[vision.Named(cfg.SceneVisionService)]
	if !ok {
		return nil, fmt.Errorf("scene_vision_service %q not found in dependencies", cfg.SceneVisionService)
	}
	sceneVisSvc, ok := sceneVisDep.(vision.Service)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not a vision service", cfg.SceneVisionService)
	}

	cam1Dep, ok := deps[camera.Named(cfg.Camera1)]
	if !ok {
		return nil, fmt.Errorf("camera_1 %q not found in dependencies", cfg.Camera1)
	}
	cam1, ok := cam1Dep.(camera.Camera)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not a camera", cfg.Camera1)
	}

	cam2Dep, ok := deps[camera.Named(cfg.Camera2)]
	if !ok {
		return nil, fmt.Errorf("camera_2 %q not found in dependencies", cfg.Camera2)
	}
	cam2, ok := cam2Dep.(camera.Camera)
	if !ok {
		return nil, fmt.Errorf("dependency %q is not a camera", cfg.Camera2)
	}

	minBedOverlap := cfg.MinBedOverlap
	if minBedOverlap <= 0 {
		minBedOverlap = defaultMinBedOverlap
	}
	maxRatio := cfg.MaxOreoToBedSizeRatio
	if maxRatio <= 0 {
		maxRatio = defaultMaxOreoToBedSizeRatio
	}
	motionThresh := cfg.SleepMotionThreshold
	if motionThresh <= 0 {
		motionThresh = defaultSleepMotionThreshold
	}
	bbPad := cfg.BoundingBoxPadding
	if bbPad < 0 {
		bbPad = defaultBoundingBoxPadding
	}
	stillSec := cfg.MinStillSeconds
	if stillSec <= 0 {
		stillSec = defaultMinStillSeconds
	}
	refreshInterval := cfg.ReferenceRefreshInterval
	if refreshInterval <= 0 {
		refreshInterval = defaultReferenceRefreshInterval
	}
	graceSec := cfg.NoDetectionGraceSeconds
	if graceSec <= 0 {
		graceSec = defaultNoDetectionGraceSeconds
	}

	a := &activityMonitor{
		Named:                    conf.ResourceName().AsNamed(),
		oreoVisSvc:               oreoVisSvc,
		sceneVisSvc:              sceneVisSvc,
		cam1:                     cam1,
		cam2:                     cam2,
		cam1Name:                 cfg.Camera1,
		cam2Name:                 cfg.Camera2,
		logger:                   logger,
		minBedOverlap:            minBedOverlap,
		maxOreoToBedSizeRatio:    maxRatio,
		sleepMotionThreshold:     motionThresh,
		boundingBoxPadding:       bbPad,
		minStillDur:              time.Duration(float64(time.Second) * stillSec),
		referenceRefreshInterval: refreshInterval,
		noDetectionGraceDur:      time.Duration(float64(time.Second) * graceSec),
		closed:                   make(chan struct{}),
	}

	go a.runLoop()
	return a, nil
}

func (a *activityMonitor) runLoop() {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.closed:
			return
		case <-ticker.C:
			a.tick(context.Background())
		}
	}
}

func (a *activityMonitor) tick(ctx context.Context) {
	now := time.Now()

	// Check which scene caches need refreshing (brief lock, no I/O).
	a.mu.Lock()
	needsScene1 := a.cachedBedBox1 == nil || a.cachedTableBox1 == nil
	needsScene2 := a.cachedBedBox2 == nil || a.cachedTableBox2 == nil
	a.mu.Unlock()

	// Detect Oreo from both cameras using the shared vision service (slow — outside lock).
	oreoDetections1, err1 := a.oreoVisSvc.DetectionsFromCamera(ctx, a.cam1Name, nil)
	oreoDetections2, err2 := a.oreoVisSvc.DetectionsFromCamera(ctx, a.cam2Name, nil)
	if err1 != nil {
		a.logger.Warnw("oreo detection failed on cam1", "error", err1)
	}
	if err2 != nil {
		a.logger.Warnw("oreo detection failed on cam2", "error", err2)
	}
	if err1 != nil && err2 != nil {
		return
	}

	var oreoBox1, oreoBox2 *image.Rectangle
	for _, d := range oreoDetections1 {
		if d.Label() == "oreo" {
			b := *d.BoundingBox()
			oreoBox1 = &b
			break
		}
	}
	for _, d := range oreoDetections2 {
		if d.Label() == "oreo" {
			b := *d.BoundingBox()
			oreoBox2 = &b
			break
		}
	}

	// Refresh scene caches if needed (slow — outside lock).
	var newBedBox1, newTableBox1, newBedBox2, newTableBox2 *image.Rectangle
	if needsScene1 {
		sceneDetections, sceneErr := a.sceneVisSvc.DetectionsFromCamera(ctx, a.cam1Name, nil)
		if sceneErr != nil {
			a.logger.Warnw("scene detection failed on cam1", "error", sceneErr)
		} else {
			for _, d := range sceneDetections {
				b := *d.BoundingBox()
				switch d.Label() {
				case "oreos-bed":
					bc := b
					newBedBox1 = &bc
				case "coffee-table-top":
					bc := b
					newTableBox1 = &bc
				}
			}
		}
	}
	if needsScene2 {
		sceneDetections, sceneErr := a.sceneVisSvc.DetectionsFromCamera(ctx, a.cam2Name, nil)
		if sceneErr != nil {
			a.logger.Warnw("scene detection failed on cam2", "error", sceneErr)
		} else {
			for _, d := range sceneDetections {
				b := *d.BoundingBox()
				switch d.Label() {
				case "oreos-bed":
					bc := b
					newBedBox2 = &bc
				case "coffee-table-top":
					bc := b
					newTableBox2 = &bc
				}
			}
		}
	}

	// Merge scene cache updates and snapshot current values (brief lock).
	a.mu.Lock()
	if newBedBox1 != nil {
		a.cachedBedBox1 = newBedBox1
	}
	if newTableBox1 != nil {
		a.cachedTableBox1 = newTableBox1
	}
	if newBedBox2 != nil {
		a.cachedBedBox2 = newBedBox2
	}
	if newTableBox2 != nil {
		a.cachedTableBox2 = newTableBox2
	}
	bedBox1 := a.cachedBedBox1
	tableBox1 := a.cachedTableBox1
	bedBox2 := a.cachedBedBox2
	tableBox2 := a.cachedTableBox2
	a.mu.Unlock()

	// Grace period: if neither camera sees Oreo, handle no-detection logic.
	if oreoBox1 == nil && oreoBox2 == nil {
		a.mu.Lock()
		if a.oreoNotDetectedSince == nil {
			t := now
			a.oreoNotDetectedSince = &t
		}
		if time.Since(*a.oreoNotDetectedSince) < a.noDetectionGraceDur {
			// Within grace period — preserve all session state.
			a.mu.Unlock()
			return
		}
		// Past grace period — end any active sessions but do not start new ones.
		a.logger.Infow("oreo undetected past grace period — ending active sessions")
		if a.sleepStart != nil {
			dur := now.Sub(*a.sleepStart)
			a.pendingSleep = append(a.pendingSleep, &CompletedSession{Start: *a.sleepStart, End: now})
			a.logger.Infof("sleep session ended (oreo undetected): duration=%.1fs", dur.Seconds())
			a.sleepStart = nil
		}
		if a.movementStart != nil {
			dur := now.Sub(*a.movementStart)
			a.pendingMovement = append(a.pendingMovement, &CompletedSession{Start: *a.movementStart, End: now})
			a.logger.Infof("movement session ended (oreo undetected): duration=%.1fs", dur.Seconds())
			a.movementStart = nil
		}
		a.stillSince = nil
		a.referenceImg1 = nil
		a.stillFrameCount1 = 0
		a.referenceImg2 = nil
		a.stillFrameCount2 = 0
		a.mu.Unlock()
		return
	}

	// At least one camera sees Oreo — reset the no-detection timer.
	a.mu.Lock()
	a.oreoNotDetectedSince = nil
	a.mu.Unlock()

	// Compute per-camera on-bed status.
	cam1Detected := oreoBox1 != nil
	cam2Detected := oreoBox2 != nil
	cam1OnBed := cam1Detected && bedBox1 != nil && tableBox1 != nil && a.checkOnBed(oreoBox1, bedBox1, tableBox1)
	cam2OnBed := cam2Detected && bedBox2 != nil && tableBox2 != nil && a.checkOnBed(oreoBox2, bedBox2, tableBox2)

	// If both cameras detect Oreo, both must agree he's on bed.
	// If only one detects him, that camera decides.
	var onBed bool
	switch {
	case cam1Detected && cam2Detected:
		onBed = cam1OnBed && cam2OnBed
	case cam1Detected:
		onBed = cam1OnBed
	default:
		onBed = cam2OnBed
	}

	// Fetch images for pixel diff only from cameras where Oreo is on bed.
	var img1, img2 image.Image
	if cam1OnBed {
		frames, _, framesErr := a.cam1.Images(ctx, nil, nil)
		if framesErr != nil {
			a.logger.Warnw("failed to get image from cam1", "error", framesErr)
		} else if len(frames) > 0 {
			img1, _ = frames[0].Image(ctx)
		}
	}
	if cam2OnBed {
		frames, _, framesErr := a.cam2.Images(ctx, nil, nil)
		if framesErr != nil {
			a.logger.Warnw("failed to get image from cam2", "error", framesErr)
		} else if len(frames) > 0 {
			img2, _ = frames[0].Image(ctx)
		}
	}

	// Compute per-camera stillness and advance state machine under lock.
	a.mu.Lock()
	defer a.mu.Unlock()

	cam1IsStill := a.updateCameraStillness(img1, oreoBox1, cam1OnBed, &a.referenceImg1, &a.stillFrameCount1, "cam1")
	cam2IsStill := a.updateCameraStillness(img2, oreoBox2, cam2OnBed, &a.referenceImg2, &a.stillFrameCount2, "cam2")

	// Either camera detecting motion ends stillness.
	isStill := cam1IsStill && cam2IsStill

	a.onBed = onBed
	a.isStill = isStill
	a.advanceState(now, onBed, isStill)
}

// updateCameraStillness updates per-camera reference frame state and returns whether this camera
// considers Oreo still. A camera not seeing Oreo on bed returns true (neutral — it has no signal).
// Must be called with a.mu held.
func (a *activityMonitor) updateCameraStillness(img image.Image, oreoBox *image.Rectangle, onBed bool, referenceImg *image.Image, stillFrameCount *int, label string) bool {
	if !onBed {
		*referenceImg = nil
		*stillFrameCount = 0
		return true
	}
	if img != nil {
		if *referenceImg == nil {
			// First frame while on bed — anchor the reference, treat as still.
			*referenceImg = img
			*stillFrameCount = 1
			return true
		}
		region := paddedBox(oreoBox, a.boundingBoxPadding, img.Bounds())
		diff := meanAbsPixelDiff(img, *referenceImg, region)
		isStill := diff < a.sleepMotionThreshold
		a.logger.Debugf("%s pixel diff=%.2f threshold=%.2f isStill=%v", label, diff, a.sleepMotionThreshold, isStill)
		if isStill {
			*stillFrameCount++
			// Periodically re-anchor to handle gradual lighting changes.
			if *stillFrameCount >= a.referenceRefreshInterval {
				*referenceImg = img
				*stillFrameCount = 0
				a.logger.Debugf("%s reference frame refreshed after %d still frames", label, a.referenceRefreshInterval)
			}
		} else {
			*referenceImg = nil
			*stillFrameCount = 0
		}
		return isStill
	} else if *referenceImg != nil {
		return true // image fetch failed — preserve stillness while reference exists
	}
	return false
}

// checkOnBed returns true when all geometric conditions confirm Oreo is on his bed.
// Reads only immutable config fields — safe to call without mu.
func (a *activityMonitor) checkOnBed(oreo, bed, table *image.Rectangle) bool {
	// 1. Oreo's box must overlap the bed box by at least minBedOverlap fraction.
	intersection := oreo.Intersect(*bed)
	if intersection.Empty() {
		return false
	}
	oreoArea := float64(oreo.Dx() * oreo.Dy())
	bedArea := float64(bed.Dx() * bed.Dy())
	if oreoArea == 0 || bedArea == 0 {
		return false
	}
	overlapFrac := float64(intersection.Dx()*intersection.Dy()) / oreoArea
	if overlapFrac < a.minBedOverlap {
		return false
	}

	// 2. Oreo's box must not be close in size to the bed box.
	// If the ratio is too high, Oreo is probably standing in front of the bed.
	if oreoArea/bedArea > a.maxOreoToBedSizeRatio {
		return false
	}

	// 3. Oreo's center must be below the bottom edge of the coffee table top.
	// Y increases downward in image coordinates; the table-top bounding box
	// captures only the visible surface, so below Max.Y means under the table.
	oreoCenterY := (oreo.Min.Y + oreo.Max.Y) / 2
	if oreoCenterY <= table.Max.Y {
		return false
	}

	return true
}

// advanceState updates session state based on the current onBed/isStill readings.
// Must be called with a.mu held.
func (a *activityMonitor) advanceState(now time.Time, onBed, isStill bool) {
	if !onBed {
		// Reset on-bed and still streaks.
		a.onBedConsecutiveSince = nil
		a.stillSince = nil

		// End any active sleep session.
		if a.sleepStart != nil {
			dur := now.Sub(*a.sleepStart)
			a.pendingSleep = append(a.pendingSleep, &CompletedSession{Start: *a.sleepStart, End: now})
			a.logger.Infof("sleep session ended: duration=%.1fs", dur.Seconds())
			a.sleepStart = nil
		}

		// Start a movement session if not already in one.
		if a.movementStart == nil {
			t := now
			a.movementStart = &t
			a.logger.Infof("movement session started")
		}
		return
	}

	// Oreo is on bed.
	if a.onBedConsecutiveSince == nil {
		t := now
		a.onBedConsecutiveSince = &t
	}

	// End the movement session once Oreo has been back on bed long enough.
	if a.movementStart != nil && time.Since(*a.onBedConsecutiveSince) >= movementGapDur {
		dur := now.Sub(*a.movementStart)
		a.pendingMovement = append(a.pendingMovement, &CompletedSession{Start: *a.movementStart, End: now})
		a.logger.Infof("movement session ended: duration=%.1fs", dur.Seconds())
		a.movementStart = nil
	}

	if isStill {
		if a.stillSince == nil {
			t := now
			a.stillSince = &t
		}
		// Start a sleep session once Oreo has been still long enough.
		// Backdate the start to when stillness first began.
		if a.sleepStart == nil && time.Since(*a.stillSince) >= a.minStillDur {
			t := *a.stillSince
			a.sleepStart = &t
			a.logger.Infof("sleep session started (still for %.0fs)", time.Since(t).Seconds())
		}
	} else {
		// On bed but moving — end any active sleep session.
		if a.sleepStart != nil {
			dur := now.Sub(*a.sleepStart)
			a.pendingSleep = append(a.pendingSleep, &CompletedSession{Start: *a.sleepStart, End: now})
			a.logger.Infof("sleep session ended (movement on bed): duration=%.1fs", dur.Seconds())
			a.sleepStart = nil
		}
		a.stillSince = nil
	}
}

// PopMovementSession returns and removes the next completed movement session, or nil.
func (a *activityMonitor) PopMovementSession() *CompletedSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingMovement) == 0 {
		return nil
	}
	s := a.pendingMovement[0]
	a.pendingMovement = a.pendingMovement[1:]
	return s
}

// PopSleepSession returns and removes the next completed sleep session, or nil.
func (a *activityMonitor) PopSleepSession() *CompletedSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pendingSleep) == 0 {
		return nil
	}
	s := a.pendingSleep[0]
	a.pendingSleep = a.pendingSleep[1:]
	return s
}

// Readings never captures data — the activity-monitor drives the background loop
// and exposes sessions to movement-monitor and sleep-monitor via ActivityCapture.
func (a *activityMonitor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	return nil, data.ErrNoCaptureToStore
}

// DoCommand supports a "status" command for live debugging via the Viam control panel.
func (a *activityMonitor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	switch command {
	case "status":
		a.mu.Lock()
		defer a.mu.Unlock()
		return map[string]interface{}{
			"on_bed":              a.onBed,
			"is_still":            a.isStill,
			"in_movement_session": a.movementStart != nil,
			"in_sleep_session":    a.sleepStart != nil,
			"pending_movement":    len(a.pendingMovement),
			"pending_sleep":       len(a.pendingSleep),
			"scene_cache_ready_1": a.cachedBedBox1 != nil && a.cachedTableBox1 != nil,
			"scene_cache_ready_2": a.cachedBedBox2 != nil && a.cachedTableBox2 != nil,
		}, nil
	default:
		return nil, fmt.Errorf("unknown command %q; supported: status", command)
	}
}

// Close stops the background poll loop.
func (a *activityMonitor) Close(ctx context.Context) error {
	a.closeOnce.Do(func() { close(a.closed) })
	return nil
}

// paddedBox expands box by paddingFrac of its width/height on each side,
// clamped to imgBounds so it never goes out of frame.
func paddedBox(box *image.Rectangle, paddingFrac float64, imgBounds image.Rectangle) image.Rectangle {
	padX := int(float64(box.Dx()) * paddingFrac)
	padY := int(float64(box.Dy()) * paddingFrac)
	return image.Rectangle{
		Min: image.Point{
			X: max(imgBounds.Min.X, box.Min.X-padX),
			Y: max(imgBounds.Min.Y, box.Min.Y-padY),
		},
		Max: image.Point{
			X: min(imgBounds.Max.X, box.Max.X+padX),
			Y: min(imgBounds.Max.Y, box.Max.Y+padY),
		},
	}
}

// meanAbsPixelDiff computes the mean absolute per-channel pixel difference between
// two images within region. Returns a value in the 0–255 range.
func meanAbsPixelDiff(cur, prev image.Image, region image.Rectangle) float64 {
	var total float64
	count := 0
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			cr, cg, cb, _ := cur.At(x, y).RGBA()
			pr, pg, pb, _ := prev.At(x, y).RGBA()
			// RGBA() returns 0–65535; shift to 0–255.
			dr := absDiff(cr>>8, pr>>8)
			dg := absDiff(cg>>8, pg>>8)
			db := absDiff(cb>>8, pb>>8)
			total += float64(dr+dg+db) / 3
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
