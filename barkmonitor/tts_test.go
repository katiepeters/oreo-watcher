package barkmonitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakePiper points piperDir at a temp dir holding a stand-in piper that
// writes "<args>|<stdin>" to its output file, and resets the speech cache.
func installFakePiper(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "piper"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join("piper", "piper"):  script,
		piperVoice + ".onnx":             "",
		piperVoice + ".onnx.json":        "",
		filepath.Join("piper", "counts"): "",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previous := piperDir
	piperDir = dir
	speechCache.Lock()
	speechCache.wavs = map[string][]byte{}
	speechCache.Unlock()
	t.Cleanup(func() { piperDir = previous })
}

const fakePiper = `#!/bin/sh
echo run >> "$(dirname "$0")/counts"
for last; do :; done
printf '%s|%s' "$*" "$(cat)" > "$last"
`

func TestSynthesizeSpeechUsesPiperWhenInstalled(t *testing.T) {
	installFakePiper(t, fakePiper)

	got, err := synthesizeSpeech("Switched to away mode")
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	if !strings.HasSuffix(out, "|Switched to away mode") {
		t.Errorf("piper was not given the text on stdin: %q", out)
	}
	for _, want := range []string{"--model " + filepath.Join(piperDir, piperVoice+".onnx"), "--length_scale " + piperLengthScale, "--output_file "} {
		if !strings.Contains(out, want) {
			t.Errorf("piper args %q are missing %q", out, want)
		}
	}
}

func TestSynthesizeSpeechRunsPiperOncePerPhrase(t *testing.T) {
	installFakePiper(t, fakePiper)
	for i := 0; i < 3; i++ {
		if _, err := synthesizeSpeech("Currently in home mode"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := synthesizeSpeech("Switched to home mode"); err != nil {
		t.Fatal(err)
	}
	counts, err := os.ReadFile(filepath.Join(piperDir, "piper", "counts"))
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(counts), "run"); runs != 2 {
		t.Errorf("piper ran %d times for 2 distinct phrases, want 2", runs)
	}
}

func TestPiperIsNotUsedUntilItsVoiceIsInstalled(t *testing.T) {
	installFakePiper(t, fakePiper)
	if err := os.Remove(filepath.Join(piperDir, piperVoice+".onnx")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := piperPaths(); ok {
		t.Error("piperPaths reports Piper usable without its voice model")
	}
}

// first_run.sh and this package must agree on where Piper lives and which
// voice it uses, or speech silently stays on espeak.
func TestFirstRunInstallsWhatSpeechLooksFor(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "first_run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`PIPER_DIR="/opt/pupster/piper"`, `PIPER_VOICE="` + piperVoice + `"`} {
		if !strings.Contains(string(script), want) {
			t.Errorf("first_run.sh does not contain %s; update it to match barkmonitor/tts.go", want)
		}
	}
	if piperDir != "/opt/pupster/piper" {
		t.Errorf("piperDir is %q; update this test and first_run.sh together", piperDir)
	}
}
