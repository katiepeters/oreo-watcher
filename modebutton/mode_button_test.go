package modebutton

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"

	"github.com/katie-viam/oreo-watcher/bthome"
)

const remoteAddr = "AA:BB:CC:DD:EE:FF"

// fakeModeCtrl mimics mode-controller's set_mode/get_mode commands.
type fakeModeCtrl struct {
	mu     sync.Mutex
	mode   string
	setErr error
}

func (f *fakeModeCtrl) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd["command"] {
	case "set_mode":
		if f.setErr != nil {
			return nil, f.setErr
		}
		f.mode = cmd["mode"].(string)
	case "get_mode":
	default:
		return nil, errors.New("unexpected command")
	}
	return map[string]interface{}{"mode": f.mode}, nil
}

func (f *fakeModeCtrl) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mode
}

// fakeSpeaker records what bark-monitor's say command was asked to speak.
type fakeSpeaker struct {
	spoken chan string
	err    error
}

func (f *fakeSpeaker) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if cmd["command"] != "say" {
		return nil, errors.New("unexpected command")
	}
	if f.err != nil {
		return nil, f.err
	}
	f.spoken <- cmd["text"].(string)
	return map[string]interface{}{"ok": true}, nil
}

type harness struct {
	mb      *modeButton
	ctrl    *fakeModeCtrl
	speaker *fakeSpeaker
	adverts chan advert
}

func newHarness(t *testing.T, address string) *harness {
	t.Helper()
	h := &harness{
		ctrl:    &fakeModeCtrl{mode: "home"},
		speaker: &fakeSpeaker{spoken: make(chan string, 16)},
		adverts: make(chan advert),
	}
	h.mb = newWithSource(sensor.Named("mode-button"), address, h.ctrl, h.speaker, logging.NewTestLogger(t))
	h.mb.start(func(ctx context.Context, onAdvert func(advert), onListening func()) error {
		onListening()
		for {
			select {
			case <-ctx.Done():
				return nil
			case a := <-h.adverts:
				onAdvert(a)
			}
		}
	})
	t.Cleanup(func() { h.mb.Close(context.Background()) })
	return h
}

// payload builds the advert a four-button remote sends when the given button
// (1-4, or 0 for none) reports event.
func payload(packetID byte, button int, event bthome.ButtonEvent) []byte {
	data := []byte{0x44, 0x00, packetID, 0x01, 87}
	for b := 1; b <= 4; b++ {
		value := bthome.ButtonNone
		if b == button {
			value = event
		}
		data = append(data, 0x3A, byte(value))
	}
	return data
}

func (h *harness) send(address string, data []byte) {
	h.adverts <- advert{address: address, data: data}
}

func (h *harness) expectSpoken(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-h.speaker.spoken:
		if got != want {
			t.Fatalf("spoke %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %q to be spoken", want)
	}
}

// expectSilence relies on presses being handled in order: once a later press
// has been announced, anything an earlier advert triggered would already have
// been announced ahead of it.
func (h *harness) expectSilence(t *testing.T, nextPacketID byte) {
	t.Helper()
	h.send(remoteAddr, payload(nextPacketID, 4, bthome.ButtonPress))
	h.expectSpoken(t, h.ctrl.current()+" mode")
}

func TestButtonsSelectAndAnnounceModes(t *testing.T) {
	h := newHarness(t, remoteAddr)
	for i, tc := range []struct {
		button   int
		event    bthome.ButtonEvent
		wantMode string
		wantSay  string
	}{
		{3, bthome.ButtonPress, "away", "away mode"},
		{2, bthome.ButtonPress, "away_lite", "away lite mode"},
		{4, bthome.ButtonPress, "away_lite", "away lite mode"},
		{1, bthome.ButtonDoublePress, "home", "home mode"},
		{4, bthome.ButtonTriplePress, "home", "home mode"},
	} {
		h.send(remoteAddr, payload(byte(i), tc.button, tc.event))
		h.expectSpoken(t, tc.wantSay)
		if got := h.ctrl.current(); got != tc.wantMode {
			t.Fatalf("button %d: mode = %q, want %q", tc.button, got, tc.wantMode)
		}
	}
}

func TestAdvertsThatMustNotChangeTheMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(h *harness)
	}{
		{"rebroadcast of an already-handled press", func(h *harness) {
			h.send(remoteAddr, payload(1, 1, bthome.ButtonPress))
			h.expectSpoken(t, "home mode")
			h.send(remoteAddr, payload(1, 1, bthome.ButtonPress))
			h.ctrl.mu.Lock()
			h.ctrl.mode = "away"
			h.ctrl.mu.Unlock()
		}},
		{"another remote", func(h *harness) {
			h.send("11:22:33:44:55:66", payload(1, 1, bthome.ButtonPress))
		}},
		{"value BlueZ cached before listening began", func(h *harness) {
			h.adverts <- advert{address: remoteAddr, data: payload(1, 1, bthome.ButtonPress), cached: true}
		}},
		{"long press", func(h *harness) {
			h.send(remoteAddr, payload(1, 1, bthome.ButtonLongPress))
		}},
		{"hold", func(h *harness) {
			h.send(remoteAddr, payload(1, 1, bthome.ButtonEvent(0x80)))
		}},
		{"no button pressed", func(h *harness) {
			h.send(remoteAddr, payload(1, 0, bthome.ButtonNone))
		}},
		{"two buttons at once", func(h *harness) {
			data := payload(1, 1, bthome.ButtonPress)
			data[len(data)-1] = byte(bthome.ButtonPress)
			h.send(remoteAddr, data)
		}},
		{"encrypted advert", func(h *harness) {
			h.send(remoteAddr, []byte{0x45, 0x01, 0x02, 0x03})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, remoteAddr)
			h.ctrl.mode = "away"
			tc.send(h)
			h.expectSilence(t, 200)
			if got := h.ctrl.current(); got != "away" {
				t.Fatalf("mode = %q, want it left at away", got)
			}
		})
	}
}

func TestCachedAdvertDoesNotSwallowTheNextPress(t *testing.T) {
	h := newHarness(t, remoteAddr)
	h.adverts <- advert{address: remoteAddr, data: payload(9, 3, bthome.ButtonPress), cached: true}
	h.send(remoteAddr, payload(10, 3, bthome.ButtonPress))
	h.expectSpoken(t, "away mode")
}

func TestModeIsNotAnnouncedWhenSettingItFails(t *testing.T) {
	h := newHarness(t, remoteAddr)
	h.ctrl.setErr = errors.New("disk full")
	h.send(remoteAddr, payload(1, 3, bthome.ButtonPress))
	h.expectSilence(t, 2)
	if got := h.ctrl.current(); got != "home" {
		t.Fatalf("mode = %q, want home", got)
	}
}

func TestWithoutMACAddressPressesAreReportedNotActedOn(t *testing.T) {
	h := newHarness(t, "")
	h.send(remoteAddr, payload(1, 3, bthome.ButtonPress))
	h.send("11:22:33:44:55:66", payload(1, 0, bthome.ButtonNone))
	// Adverts are handled one at a time, so this send returning means the two
	// above have been fully processed.
	h.send(remoteAddr, payload(2, 0, bthome.ButtonNone))

	readings, err := h.mb.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []interface{}{"11:22:33:44:55:66", remoteAddr}
	if got := readings["discovered_remotes"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("discovered_remotes = %v, want %v", got, want)
	}
	select {
	case got := <-h.speaker.spoken:
		t.Fatalf("spoke %q with no mac_address configured", got)
	case <-time.After(100 * time.Millisecond):
	}
	if got := h.ctrl.current(); got != "home" {
		t.Fatalf("mode = %q, want home", got)
	}
}

func TestReadingsAfterPress(t *testing.T) {
	h := newHarness(t, remoteAddr)
	h.send(remoteAddr, payload(1, 2, bthome.ButtonPress))
	h.expectSpoken(t, "away lite mode")
	readings, err := h.mb.Readings(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if readings["battery_percent"] != 87 || readings["last_button"] != 2 || readings["mac_address"] != remoteAddr {
		t.Fatalf("unexpected readings: %v", readings)
	}
}

func TestPressCommand(t *testing.T) {
	h := newHarness(t, remoteAddr)
	resp, err := h.mb.DoCommand(context.Background(), map[string]interface{}{"command": "press", "button": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if resp["mode"] != "away" {
		t.Fatalf("resp = %v, want mode away", resp)
	}
	h.expectSpoken(t, "away mode")

	h.speaker.err = errors.New("no speaker")
	if _, err := h.mb.DoCommand(context.Background(), map[string]interface{}{"command": "press", "button": float64(1)}); err == nil {
		t.Fatal("want an error when the announcement fails")
	}
	if _, err := h.mb.DoCommand(context.Background(), map[string]interface{}{"command": "press", "button": float64(5)}); err == nil {
		t.Fatal("want an error for a button the remote doesn't have")
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{MACAddress: "aa-bb-cc-dd-ee-ff", ModeController: "mc", BarkMonitor: "bm"}, false},
		{"address not yet known", Config{ModeController: "mc", BarkMonitor: "bm"}, false},
		{"malformed address", Config{MACAddress: "nope", ModeController: "mc", BarkMonitor: "bm"}, true},
		{"missing mode_controller", Config{BarkMonitor: "bm"}, true},
		{"missing bark_monitor", Config{ModeController: "mc"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, _, err := tc.cfg.Validate("")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && !reflect.DeepEqual(deps, []string{"mc", "bm"}) {
				t.Fatalf("deps = %v", deps)
			}
		})
	}
}

func TestAdvertFromBlueZProperties(t *testing.T) {
	const adapter = dbus.ObjectPath("/org/bluez/hci0")
	data := payload(1, 1, bthome.ButtonPress)
	props := map[string]dbus.Variant{
		"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{bthome.ServiceUUID: dbus.MakeVariant(data)}),
	}

	got, ok := advertFrom(adapter, adapter+"/dev_aa_BB_CC_DD_EE_FF", props)
	if !ok || got.address != remoteAddr || !reflect.DeepEqual(got.data, data) {
		t.Fatalf("got %+v, ok=%v", got, ok)
	}

	for name, tc := range map[string]struct {
		path  dbus.ObjectPath
		props map[string]dbus.Variant
	}{
		"device on another adapter":   {"/org/bluez/hci1/dev_AA_BB_CC_DD_EE_FF", props},
		"GATT child of the device":    {adapter + "/dev_AA_BB_CC_DD_EE_FF/service0001", props},
		"change without service data": {adapter + "/dev_AA_BB_CC_DD_EE_FF", map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-60))}},
		"service data that isn't BTHome": {adapter + "/dev_AA_BB_CC_DD_EE_FF", map[string]dbus.Variant{
			"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{"0000feaa-0000-1000-8000-00805f9b34fb": dbus.MakeVariant(data)}),
		}},
	} {
		if _, ok := advertFrom(adapter, tc.path, tc.props); ok {
			t.Errorf("%s: want no advert", name)
		}
	}
}

func TestBluetoothErrorClearsOnceListeningResumes(t *testing.T) {
	failed := make(chan struct{})
	resume := make(chan struct{})
	listening := make(chan struct{})
	attempts := 0
	mb := newWithSource(sensor.Named("mode-button"), remoteAddr, &fakeModeCtrl{}, &fakeSpeaker{}, logging.NewTestLogger(t))
	mb.retryDelay = time.Millisecond
	mb.start(func(ctx context.Context, _ func(advert), onListening func()) error {
		attempts++
		if attempts == 1 {
			defer close(failed)
			return errors.New("adapter powered off")
		}
		<-resume
		onListening()
		close(listening)
		<-ctx.Done()
		return nil
	})
	t.Cleanup(func() { mb.Close(context.Background()) })

	<-failed
	waitForReading(t, mb, "bluetooth_error", "adapter powered off")
	close(resume)
	<-listening
	waitForReading(t, mb, "bluetooth_error", nil)
}

func waitForReading(t *testing.T, mb *modeButton, key string, want interface{}) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		readings, err := mb.Readings(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if readings[key] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %v, want %v", key, readings[key], want)
		}
		time.Sleep(time.Millisecond)
	}
}
