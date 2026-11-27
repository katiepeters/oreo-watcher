package barkmonitor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"go.viam.com/rdk/logging"
)

// Where first_run.sh installs the Piper engine and its voice.
// TestFirstRunInstallsWhatSpeechLooksFor keeps the script in step with these.
var piperDir = "/opt/pupster/piper"

const piperVoice = "en_US-kristin-medium"

// piperLengthScale stretches Piper's speech; above 1 is slower than the
// voice's natural pace.
const piperLengthScale = "1.1"

// espeakWordsPerMinute is the speaking rate passed to espeak, a little under
// its default of 175 so short phrases are easier to catch.
const espeakWordsPerMinute = "150"

// piperPaths returns the Piper binary and voice model if both are installed.
func piperPaths() (bin, model string, ok bool) {
	bin = filepath.Join(piperDir, "piper", "piper")
	model = filepath.Join(piperDir, piperVoice+".onnx")
	for _, path := range []string{bin, model, model + ".json"} {
		if _, err := os.Stat(path); err != nil {
			return "", "", false
		}
	}
	return bin, model, true
}

// espeakBinary locates espeak. espeak-ng (the maintained fork, and what
// first_run.sh installs) doesn't always provide an "espeak"-named binary, so
// both names are tried.
func espeakBinary() (string, error) {
	for _, name := range []string{"espeak-ng", "espeak"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("neither espeak-ng nor espeak found in PATH")
}

// logTTSEngine reports at startup which engine spoken lines will use, so a
// machine that is missing the intended voice says so in its logs.
func logTTSEngine(logger logging.Logger) {
	if _, _, ok := piperPaths(); ok {
		logger.Infof("spoken lines use the Piper voice %s", piperVoice)
		return
	}
	if _, err := espeakBinary(); err == nil {
		logger.Warnf("the Piper voice %s is not installed in %s, so spoken lines use espeak; the module's first_run.sh installs it", piperVoice, piperDir)
		return
	}
	logger.Errorf("no text-to-speech engine on this machine: spoken lines will fall back to static sounds or fail. The module's first_run.sh installs Piper into %s and espeak-ng.", piperDir)
}

// speechCache holds Piper's output per phrase. Piper takes a second or more on
// a small machine, and the same few phrases are spoken over and over.
var speechCache = struct {
	sync.Mutex
	wavs map[string][]byte
}{wavs: map[string][]byte{}}

const speechCacheLimit = 64

// synthesizeSpeech renders text to WAV bytes: with Piper when it is installed,
// otherwise with espeak.
func synthesizeSpeech(text string) ([]byte, error) {
	bin, model, ok := piperPaths()
	if !ok {
		return synthesizeWithEspeak(text)
	}
	speechCache.Lock()
	cached, hit := speechCache.wavs[text]
	speechCache.Unlock()
	if hit {
		return cached, nil
	}
	wav, piperErr := runTTS(exec.Command(bin, "--model", model, "--length_scale", piperLengthScale, "--output_file"), text)
	if piperErr != nil {
		wav, err := synthesizeWithEspeak(text)
		if err != nil {
			return nil, errors.Join(piperErr, err)
		}
		return wav, nil
	}
	speechCache.Lock()
	if len(speechCache.wavs) < speechCacheLimit {
		speechCache.wavs[text] = wav
	}
	speechCache.Unlock()
	return wav, nil
}

func synthesizeWithEspeak(text string) ([]byte, error) {
	bin, err := espeakBinary()
	if err != nil {
		return nil, err
	}
	return runTTS(exec.Command(bin, "-s", espeakWordsPerMinute, "--stdin", "-w"), text)
}

// runTTS runs a TTS command that reads the text on stdin and takes the output
// WAV path as its final argument, returning the WAV it wrote.
func runTTS(cmd *exec.Cmd, text string) ([]byte, error) {
	tmpFile, err := os.CreateTemp("", "tts-*.wav")
	if err != nil {
		return nil, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	cmd.Args = append(cmd.Args, tmpPath)
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", cmd.Path, err, out)
	}
	wav, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("reading synthesized audio: %w", err)
	}
	return wav, nil
}
