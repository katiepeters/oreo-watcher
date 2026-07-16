// Package modestate persists small operating-state values (which detection
// backend is active, which away/home mode is active) to a local file on disk,
// independent of Viam machine config so mode switches survive both restarts
// and machine-config changes (e.g. a Fragment being reapplied).
package modestate

import (
	"encoding/json"
	"os"
	"sync"
)

const statePath = "/root/pupster-mode.json"

// Detection mode values, read by learningmode and bark_monitor to decide
// whether they should be doing anything on a given poll.
const (
	DetectionModeLearning = "learning"
	DetectionModeTrained  = "trained"
)

type state struct {
	OperatingMode string `json:"operating_mode"`
	DetectionMode string `json:"detection_mode"`
}

var mu sync.Mutex

func read() (state, error) {
	mu.Lock()
	defer mu.Unlock()
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var s state
	if err := json.Unmarshal(data, &s); err != nil {
		return state{}, err
	}
	return s, nil
}

func write(s state) error {
	mu.Lock()
	defer mu.Unlock()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, data, 0o644)
}

// GetDetectionMode returns the persisted detection mode, defaulting to
// DetectionModeLearning if none has been set yet.
func GetDetectionMode() (string, error) {
	s, err := read()
	if err != nil {
		return "", err
	}
	if s.DetectionMode == "" {
		return DetectionModeLearning, nil
	}
	return s.DetectionMode, nil
}

// SetDetectionMode persists the detection mode.
func SetDetectionMode(mode string) error {
	s, err := read()
	if err != nil {
		return err
	}
	s.DetectionMode = mode
	return write(s)
}

// GetOperatingMode returns the persisted away/away_lite/home mode, defaulting
// to "home" if none has been set yet.
func GetOperatingMode() (string, error) {
	s, err := read()
	if err != nil {
		return "", err
	}
	if s.OperatingMode == "" {
		return "home", nil
	}
	return s.OperatingMode, nil
}

// SetOperatingMode persists the away/away_lite/home mode.
func SetOperatingMode(mode string) error {
	s, err := read()
	if err != nil {
		return err
	}
	s.OperatingMode = mode
	return write(s)
}
