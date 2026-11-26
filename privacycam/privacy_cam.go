// Package privacycam provides a webcam component that only opens the camera
// device while the persisted operating mode allows cameras (away mode).
//
// It replaces rdk:builtin:webcam on machines that must not produce camera
// frames outside away mode. Because frames come only from this component,
// nothing downstream can get them while the mode denies cameras: not the
// live stream in the Viam app, not video-service recording, not vision
// services, and not data-manager capture.
package privacycam

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/camera/videosource"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"

	"github.com/katie-viam/oreo-watcher/modestate"
)

// Model is the model triple for the privacy-cam camera component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "privacy-cam")

// ErrCameraOff is returned by Images while the operating mode denies cameras.
var ErrCameraOff = errors.New("camera is off: cameras are only enabled in away mode")

// modePollInterval bounds how long the device can stay open after the mode
// leaves away.
const modePollInterval = time.Second

func init() {
	resource.RegisterComponent(camera.API, Model, resource.Registration[camera.Camera, *Config]{
		Constructor: newPrivacyCam,
	})
}

// Config takes the same attributes as rdk:builtin:webcam, so an existing
// webcam component can be switched over by changing only its model.
type Config struct {
	videosource.WebcamConfig
}

// Validate checks the embedded webcam config.
func (c *Config) Validate(path string) ([]string, []string, error) {
	return c.WebcamConfig.Validate(path)
}

type opener func(ctx context.Context) (camera.Camera, error)

type privacyCam struct {
	resource.Named
	resource.AlwaysRebuild

	logger  logging.Logger
	open    opener
	allowed func() bool

	mu      sync.Mutex
	dev     camera.Camera // nil while the device is closed
	lastErr string        // last open error logged, to avoid repeating it every poll

	cancel context.CancelFunc
	done   chan struct{}
}

func newPrivacyCam(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (camera.Camera, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	webcamConf := cfg.WebcamConfig
	open := func(ctx context.Context) (camera.Camera, error) {
		return videosource.NewWebcam(ctx, deps, resource.Config{
			Name:                conf.Name,
			API:                 camera.API,
			Model:               videosource.ModelWebcam,
			ConvertedAttributes: &webcamConf,
		}, logger)
	}
	return newWithDeps(conf.ResourceName().AsNamed(), logger, open, modestate.CameraCaptureAllowed), nil
}

func newWithDeps(named resource.Named, logger logging.Logger, open opener, allowed func() bool) *privacyCam {
	ctx, cancel := context.WithCancel(context.Background())
	c := &privacyCam{
		Named:   named,
		logger:  logger,
		open:    open,
		allowed: allowed,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	c.sync(ctx)
	go c.watch(ctx)
	return c
}

func (c *privacyCam) watch(ctx context.Context) {
	defer close(c.done)
	t := time.NewTicker(modePollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sync(ctx)
		}
	}
}

// sync opens or closes the device to match the current operating mode.
func (c *privacyCam) sync(ctx context.Context) {
	allowed := c.allowed()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !allowed {
		c.closeDeviceLocked(ctx)
		return
	}
	if c.dev != nil {
		return
	}
	dev, err := c.open(ctx)
	if err != nil {
		if msg := err.Error(); msg != c.lastErr {
			c.logger.Warnw("cameras allowed but device failed to open; will retry", "error", err)
			c.lastErr = msg
		}
		return
	}
	c.lastErr = ""
	c.dev = dev
	c.logger.Info("camera device opened: operating mode allows cameras")
}

func (c *privacyCam) closeDeviceLocked(ctx context.Context) {
	if c.dev == nil {
		return
	}
	if err := c.dev.Close(ctx); err != nil {
		c.logger.Warnw("closing camera device", "error", err)
	}
	c.dev = nil
	c.logger.Info("camera device closed: operating mode does not allow cameras")
}

// device returns the open device, re-checking the mode first so a frame is
// never served in the window between a mode change and the next poll.
func (c *privacyCam) device(ctx context.Context) (camera.Camera, error) {
	if !c.allowed() {
		c.mu.Lock()
		c.closeDeviceLocked(ctx)
		c.mu.Unlock()
		return nil, ErrCameraOff
	}
	c.mu.Lock()
	dev := c.dev
	c.mu.Unlock()
	if dev == nil {
		c.sync(ctx)
		c.mu.Lock()
		dev = c.dev
		c.mu.Unlock()
	}
	if dev == nil {
		return nil, fmt.Errorf("camera device not available (last error: %s)", c.lastErrSnapshot())
	}
	return dev, nil
}

func (c *privacyCam) lastErrSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

func (c *privacyCam) Images(ctx context.Context, filterSourceNames []string, extra map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	dev, err := c.device(ctx)
	if err != nil {
		return nil, resource.ResponseMetadata{}, err
	}
	return dev.Images(ctx, filterSourceNames, extra)
}

func (c *privacyCam) NextPointCloud(ctx context.Context, extra map[string]interface{}) (pointcloud.PointCloud, error) {
	dev, err := c.device(ctx)
	if err != nil {
		return nil, err
	}
	return dev.NextPointCloud(ctx, extra)
}

// Properties never opens the device; with it closed, it reports a camera
// with no intrinsics rather than an error so dependents can still build.
func (c *privacyCam) Properties(ctx context.Context) (camera.Properties, error) {
	c.mu.Lock()
	dev := c.dev
	c.mu.Unlock()
	if dev == nil {
		return camera.Properties{}, nil
	}
	return dev.Properties(ctx)
}

func (c *privacyCam) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return nil, nil
}

func (c *privacyCam) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	c.mu.Lock()
	open := c.dev != nil
	c.mu.Unlock()
	return map[string]interface{}{"device_open": open}, nil
}

func (c *privacyCam) Close(ctx context.Context) error {
	c.cancel()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeDeviceLocked(ctx)
	return nil
}
