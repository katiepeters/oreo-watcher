// Package modebutton lets a Shelly BLU RC Button 4 remote select the operating
// mode. It listens for the remote's BTHome Bluetooth broadcasts, so the remote
// needs no pairing and the machine needs no Zigbee radio.
package modebutton

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"github.com/katie-viam/oreo-watcher/bthome"
	"github.com/katie-viam/oreo-watcher/modecontroller"
)

// Model is the model triple for the mode-button sensor component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "mode-button")

const (
	defaultAdapter = "hci0"
	// announceButton speaks the current mode instead of changing it.
	announceButton = 4
	buttonCount    = 4
	scanRetryDelay = 10 * time.Second
	// repeatWindow is how long an identical advert without a packet ID is
	// treated as a rebroadcast of the same press rather than a new one.
	repeatWindow = 3 * time.Second
)

// buttonModes maps a remote button number to the mode it selects.
var buttonModes = map[int]modecontroller.Mode{
	1: modecontroller.ModeHome,
	2: modecontroller.ModeAwayLite,
	3: modecontroller.ModeAway,
}

func init() {
	resource.RegisterComponent(sensor.API, Model, resource.Registration[sensor.Sensor, *Config]{
		Constructor: newModeButton,
	})
}

// Config holds the component configuration.
type Config struct {
	// MACAddress is the remote's Bluetooth address. Until it is set, presses
	// are ignored and every Shelly-style button heard is reported in Readings
	// and the logs so the address can be copied from there.
	MACAddress string `json:"mac_address"`
	// ModeController is the mode-controller sensor that buttons 1-3 drive.
	ModeController string `json:"mode_controller"`
	// BarkMonitor is the bark-monitor sensor whose speaker announces the mode.
	BarkMonitor string `json:"bark_monitor"`
	// Adapter is the BlueZ adapter to listen on. Defaults to "hci0".
	Adapter string `json:"adapter"`
}

// Validate ensures the config is valid and returns required dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.ModeController == "" {
		return nil, nil, fmt.Errorf("mode_controller is required")
	}
	if c.BarkMonitor == "" {
		return nil, nil, fmt.Errorf("bark_monitor is required")
	}
	if c.MACAddress != "" {
		if _, err := normalizeMAC(c.MACAddress); err != nil {
			return nil, nil, err
		}
	}
	return []string{c.ModeController, c.BarkMonitor}, nil, nil
}

func normalizeMAC(s string) (string, error) {
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return "", fmt.Errorf("mac_address %q is not a Bluetooth address like AA:BB:CC:DD:EE:FF", s)
	}
	return strings.ToUpper(hw.String()), nil
}

// commander is the slice of a resource this component drives.
type commander interface {
	DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error)
}

type modeButton struct {
	resource.Named
	resource.AlwaysRebuild
	logger     logging.Logger
	address    string // empty until mac_address is configured
	modeCtrl   commander
	speaker    commander
	presses    chan int
	cancel     context.CancelFunc
	background sync.WaitGroup

	mu           sync.Mutex
	lastData     []byte
	lastHeard    time.Time
	battery      *uint8
	lastButton   int
	lastPressAt  time.Time
	scanErr      error
	discovered   map[string]struct{}
	warnedCipher bool
}

func newModeButton(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}
	modeCtrl, ok := deps[sensor.Named(cfg.ModeController)]
	if !ok {
		return nil, fmt.Errorf("mode_controller %q not found in dependencies", cfg.ModeController)
	}
	speaker, ok := deps[sensor.Named(cfg.BarkMonitor)]
	if !ok {
		return nil, fmt.Errorf("bark_monitor %q not found in dependencies", cfg.BarkMonitor)
	}
	var address string
	if cfg.MACAddress != "" {
		if address, err = normalizeMAC(cfg.MACAddress); err != nil {
			return nil, err
		}
	} else {
		logger.Warn("mac_address is not set: button presses are ignored. Press a button on the remote, then copy its address from this component's logs or readings into mac_address.")
	}
	adapter := cfg.Adapter
	if adapter == "" {
		adapter = defaultAdapter
	}

	mb := newWithSource(conf.ResourceName(), address, modeCtrl, speaker, logger)
	mb.start(listenBlueZ(adapter))
	return mb, nil
}

func newWithSource(name resource.Name, address string, modeCtrl, speaker commander, logger logging.Logger) *modeButton {
	return &modeButton{
		Named:      name.AsNamed(),
		logger:     logger,
		address:    address,
		modeCtrl:   modeCtrl,
		speaker:    speaker,
		presses:    make(chan int, buttonCount),
		discovered: map[string]struct{}{},
	}
}

// start launches the listener and the worker that acts on presses. Presses are
// handled off the listener goroutine because announcing blocks until the
// speaker finishes.
func (mb *modeButton) start(source advertSource) {
	ctx, cancel := context.WithCancel(context.Background())
	mb.cancel = cancel

	mb.background.Add(2)
	go func() {
		defer mb.background.Done()
		for {
			err := source(ctx, mb.handleAdvert)
			if ctx.Err() != nil {
				return
			}
			mb.mu.Lock()
			mb.scanErr = err
			mb.mu.Unlock()
			mb.logger.Errorw("bluetooth listening stopped, will retry", "error", err, "retry_in", scanRetryDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(scanRetryDelay):
			}
		}
	}()
	go func() {
		defer mb.background.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case button := <-mb.presses:
				if _, err := mb.press(ctx, button); err != nil {
					mb.logger.Errorw("button press failed", "button", button, "error", err)
				}
			}
		}
	}()
}

// handleAdvert turns one BTHome advert into at most one queued button press.
func (mb *modeButton) handleAdvert(a advert) {
	packet, err := bthome.Parse(a.data)

	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.scanErr = nil

	if mb.address == "" {
		if _, known := mb.discovered[a.address]; !known && len(packet.Buttons) > 0 {
			mb.discovered[a.address] = struct{}{}
			mb.logger.Infow("heard a button remote; set mac_address to this address to use it", "address", a.address)
		}
		return
	}
	if a.address != mb.address {
		return
	}
	if err != nil && len(packet.Buttons) == 0 {
		if errors.Is(err, bthome.ErrEncrypted) && !mb.warnedCipher {
			mb.warnedCipher = true
			mb.logger.Error("the remote has encryption enabled, which is not supported; factory-reset the remote to turn encryption off")
		}
		return
	}

	now := time.Now()
	repeat := bytes.Equal(a.data, mb.lastData) && (packet.HasPacketID || now.Sub(mb.lastHeard) < repeatWindow)
	mb.lastData, mb.lastHeard = a.data, now
	if packet.HasBattery {
		battery := packet.BatteryPercent
		mb.battery = &battery
	}
	if repeat || a.cached {
		return
	}

	var pressed []int
	for i, event := range packet.Buttons {
		switch event {
		case bthome.ButtonPress, bthome.ButtonDoublePress, bthome.ButtonTriplePress:
			pressed = append(pressed, i+1)
		}
	}
	if len(pressed) != 1 {
		if len(pressed) > 1 {
			mb.logger.Infow("ignoring simultaneous button presses", "buttons", pressed)
		}
		return
	}
	mb.lastButton, mb.lastPressAt = pressed[0], now
	select {
	case mb.presses <- pressed[0]:
	default:
		mb.logger.Warnw("dropping button press: earlier presses are still being handled", "button", pressed[0])
	}
}

// press performs a button's action and returns the mode that was announced.
// The mode is only spoken once the mode controller has accepted it.
func (mb *modeButton) press(ctx context.Context, button int) (string, error) {
	var mode string
	if target, ok := buttonModes[button]; ok {
		mode = string(target)
		if _, err := mb.modeCtrl.DoCommand(ctx, map[string]interface{}{"command": "set_mode", "mode": mode}); err != nil {
			return "", fmt.Errorf("setting mode %q: %w", mode, err)
		}
	} else if button == announceButton {
		resp, err := mb.modeCtrl.DoCommand(ctx, map[string]interface{}{"command": "get_mode"})
		if err != nil {
			return "", fmt.Errorf("reading mode: %w", err)
		}
		if mode, ok = resp["mode"].(string); !ok || mode == "" {
			return "", fmt.Errorf("mode controller returned no mode: %v", resp)
		}
	} else {
		return "", fmt.Errorf("unknown button %d; the remote has buttons 1-%d", button, buttonCount)
	}

	text := modecontroller.Mode(mode).SpokenName()
	if _, err := mb.speaker.DoCommand(ctx, map[string]interface{}{"command": "say", "text": text}); err != nil {
		return mode, fmt.Errorf("mode is %q but announcing it failed: %w", mode, err)
	}
	return mode, nil
}

// Readings reports the remote's state.
func (mb *modeButton) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	readings := map[string]interface{}{"mac_address": mb.address}
	if mb.battery != nil {
		readings["battery_percent"] = int(*mb.battery)
	}
	if !mb.lastPressAt.IsZero() {
		readings["last_button"] = mb.lastButton
		readings["last_press_at"] = mb.lastPressAt.Format(time.RFC3339)
	}
	if mb.scanErr != nil {
		readings["bluetooth_error"] = mb.scanErr.Error()
	}
	if mb.address == "" {
		discovered := make([]interface{}, 0, len(mb.discovered))
		for address := range mb.discovered {
			discovered = append(discovered, address)
		}
		sort.Slice(discovered, func(i, j int) bool { return discovered[i].(string) < discovered[j].(string) })
		readings["discovered_remotes"] = discovered
	}
	return readings, nil
}

// DoCommand simulates a press, for testing without the remote in hand:
//
//	{"command": "press", "button": 1|2|3|4}
func (mb *modeButton) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	command, _ := cmd["command"].(string)
	if command != "press" {
		return nil, fmt.Errorf("unknown command %q; supported: press", command)
	}
	button, ok := cmd["button"].(float64)
	if !ok {
		return nil, fmt.Errorf("press requires a numeric \"button\" (1-%d)", buttonCount)
	}
	mode, err := mb.press(ctx, int(button))
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"mode": mode}, nil
}

func (mb *modeButton) Close(ctx context.Context) error {
	if mb.cancel != nil {
		mb.cancel()
	}
	mb.background.Wait()
	return nil
}
