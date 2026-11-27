package privacycam

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/testutils/inject"
)

// fakeDevice counts opens and closes and tracks whether a device is open.
type fakeDevice struct {
	mu      sync.Mutex
	opens   int
	closes  int
	open    bool
	openErr error
}

func (f *fakeDevice) opener() opener {
	return func(context.Context) (camera.Camera, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.openErr != nil {
			return nil, f.openErr
		}
		f.opens++
		f.open = true
		cam := inject.NewCamera("dev")
		cam.ImagesFunc = func(context.Context, []string, map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.open {
				return nil, resource.ResponseMetadata{}, errors.New("frame read from a closed device")
			}
			return []camera.NamedImage{{}}, resource.ResponseMetadata{}, nil
		}
		cam.CloseFunc = func(context.Context) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.closes++
			f.open = false
			return nil
		}
		return cam, nil
	}
}

func (f *fakeDevice) isOpen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

// withDeadline returns a context like the ones gRPC callers (the Viam app,
// the CLI, vision services) send, which privacy-cam never holds.
func withDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newTestCam(t *testing.T, dev *fakeDevice, allowed *atomic.Bool) *privacyCam {
	t.Helper()
	c := newWithDeps(resource.NewName(camera.API, "cam").AsNamed(), logging.NewTestLogger(t), dev.opener(), allowed.Load)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestNoDeviceOrFramesOutsideAway(t *testing.T) {
	var allowed atomic.Bool
	dev := &fakeDevice{}
	c := newTestCam(t, dev, &allowed)

	if dev.isOpen() {
		t.Fatal("device opened at construction while cameras are denied")
	}
	if _, _, err := c.Images(withDeadline(t), nil, nil); !errors.Is(err, ErrCameraOff) {
		t.Fatalf("Images outside away: err = %v, want ErrCameraOff", err)
	}
	if _, err := c.NextPointCloud(withDeadline(t), nil); !errors.Is(err, ErrCameraOff) {
		t.Fatalf("NextPointCloud outside away: err = %v, want ErrCameraOff", err)
	}
	if dev.opens != 0 {
		t.Fatalf("device opened %d times while cameras were denied", dev.opens)
	}
}

func TestFramesInAway(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	dev := &fakeDevice{}
	c := newTestCam(t, dev, &allowed)

	if !dev.isOpen() {
		t.Fatal("device not opened at construction in away mode")
	}
	imgs, _, err := c.Images(context.Background(), nil, nil)
	if err != nil || len(imgs) != 1 {
		t.Fatalf("Images in away: got %d images, err %v", len(imgs), err)
	}
}

// TestLeavingAwayClosesDevice checks both paths: Images re-checks the mode
// immediately, and the watcher closes the device even with no callers.
func TestLeavingAwayClosesDevice(t *testing.T) {
	t.Run("on next Images call", func(t *testing.T) {
		var allowed atomic.Bool
		allowed.Store(true)
		dev := &fakeDevice{}
		c := newTestCam(t, dev, &allowed)

		allowed.Store(false)
		if _, _, err := c.Images(withDeadline(t), nil, nil); !errors.Is(err, ErrCameraOff) {
			t.Fatalf("Images right after leaving away: err = %v, want ErrCameraOff", err)
		}
		if dev.isOpen() {
			t.Fatal("device still open after Images saw cameras denied")
		}
	})
	t.Run("by the watcher with no callers", func(t *testing.T) {
		var allowed atomic.Bool
		allowed.Store(true)
		dev := &fakeDevice{}
		newTestCam(t, dev, &allowed)

		allowed.Store(false)
		deadline := time.Now().Add(3 * modePollInterval)
		for dev.isOpen() {
			if time.Now().After(deadline) {
				t.Fatalf("device still open %v after leaving away", 3*modePollInterval)
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestReturningToAwayReopens(t *testing.T) {
	var allowed atomic.Bool
	dev := &fakeDevice{}
	c := newTestCam(t, dev, &allowed)

	allowed.Store(true)
	if _, _, err := c.Images(context.Background(), nil, nil); err != nil {
		t.Fatalf("Images after returning to away: %v", err)
	}
	if dev.opens != 1 {
		t.Fatalf("opens = %d, want 1", dev.opens)
	}
}

func TestOpenFailureIsAnErrorNotAFrame(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	dev := &fakeDevice{openErr: errors.New("no such device")}
	c := newTestCam(t, dev, &allowed)

	if _, _, err := c.Images(context.Background(), nil, nil); err == nil {
		t.Fatal("Images succeeded with no device")
	}
}

func TestCloseReleasesDevice(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	dev := &fakeDevice{}
	c := newWithDeps(resource.NewName(camera.API, "cam").AsNamed(), logging.NewTestLogger(t), dev.opener(), allowed.Load)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dev.isOpen() {
		t.Fatal("device still open after Close")
	}
}

func TestPropertiesDoesNotOpenDevice(t *testing.T) {
	var allowed atomic.Bool
	dev := &fakeDevice{}
	c := newTestCam(t, dev, &allowed)
	if _, err := c.Properties(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dev.opens != 0 {
		t.Fatal("Properties opened the device while cameras were denied")
	}
}

// TestCallerWithDeadlineFailsFast keeps the Viam app and CLI responsive: a
// caller with a deadline is never held while cameras are off.
func TestCallerWithDeadlineFailsFast(t *testing.T) {
	var allowed atomic.Bool
	c := newTestCam(t, &fakeDevice{}, &allowed)

	start := time.Now()
	if _, _, err := c.Images(withDeadline(t), nil, nil); !errors.Is(err, ErrCameraOff) {
		t.Fatalf("err = %v, want ErrCameraOff", err)
	}
	if waited := time.Since(start); waited > modePollInterval/2 {
		t.Fatalf("caller with a deadline was held for %v", waited)
	}
}

// TestCallerWithoutDeadlineWaitsForAway covers video-store's frame poller:
// it is held while cameras are off, so it does not retry and log every
// second, and gets a frame once the mode allows cameras.
func TestCallerWithoutDeadlineWaitsForAway(t *testing.T) {
	var allowed atomic.Bool
	dev := &fakeDevice{}
	c := newTestCam(t, dev, &allowed)

	type result struct {
		n   int
		err error
	}
	got := make(chan result, 1)
	go func() {
		imgs, _, err := c.Images(context.Background(), nil, nil)
		got <- result{len(imgs), err}
	}()

	select {
	case r := <-got:
		t.Fatalf("caller without a deadline returned while cameras were off: %+v", r)
	case <-time.After(2 * modePollInterval):
	}
	if dev.isOpen() {
		t.Fatal("device opened while the caller was waiting and cameras were off")
	}

	allowed.Store(true)
	select {
	case r := <-got:
		if r.err != nil || r.n != 1 {
			t.Fatalf("after entering away: %d images, err %v", r.n, r.err)
		}
	case <-time.After(3 * modePollInterval):
		t.Fatal("waiting caller not released after entering away")
	}
}

func TestWaitingCallerReleased(t *testing.T) {
	t.Run("by offWait", func(t *testing.T) {
		var allowed atomic.Bool
		c := newTestCam(t, &fakeDevice{}, &allowed)
		c.offWait = 50 * time.Millisecond
		if _, _, err := c.Images(context.Background(), nil, nil); !errors.Is(err, ErrCameraOff) {
			t.Fatalf("err = %v, want ErrCameraOff after offWait", err)
		}
	})
	t.Run("by caller cancel", func(t *testing.T) {
		var allowed atomic.Bool
		c := newTestCam(t, &fakeDevice{}, &allowed)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		if _, _, err := c.Images(ctx, nil, nil); !errors.Is(err, ErrCameraOff) {
			t.Fatalf("err = %v, want ErrCameraOff after cancel", err)
		}
	})
	t.Run("by Close", func(t *testing.T) {
		var allowed atomic.Bool
		c := newWithDeps(resource.NewName(camera.API, "cam").AsNamed(), logging.NewTestLogger(t), (&fakeDevice{}).opener(), allowed.Load)
		time.AfterFunc(50*time.Millisecond, func() { _ = c.Close(context.Background()) })
		done := make(chan error, 1)
		go func() { _, _, err := c.Images(context.Background(), nil, nil); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, ErrCameraOff) {
				t.Fatalf("err = %v, want ErrCameraOff after Close", err)
			}
		case <-time.After(3 * modePollInterval):
			t.Fatal("Close did not release a waiting caller")
		}
	})
}
