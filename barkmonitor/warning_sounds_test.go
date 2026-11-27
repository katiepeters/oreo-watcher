package barkmonitor

import (
	"testing"
	"time"

	"go.viam.com/rdk/logging"
)

func TestWarningClipSegments(t *testing.T) {
	for _, clip := range warningClips {
		if clip.mp3 == nil {
			continue // short recordings play whole; nothing to segment
		}
		t.Run(clip.name, func(t *testing.T) {
			for range 5 {
				pcm, info, offset, err := clip.segment(maxWarningClipDur)
				if err != nil {
					t.Fatalf("segment: %v", err)
				}
				if info.NumChannels != 1 || info.SampleRateHz <= 0 {
					t.Fatalf("unexpected format: %+v", info)
				}
				dur := pcmDuration(pcm, info)
				if dur > maxWarningClipDur {
					t.Fatalf("segment is %v, want <= %v", dur, maxWarningClipDur)
				}
				if dur < maxWarningClipDur-100*time.Millisecond {
					t.Fatalf("segment is %v, want ~%v (all clips are longer than the cap)", dur, maxWarningClipDur)
				}
				if offset < 0 {
					t.Fatalf("negative offset %v", offset)
				}
			}
		})
	}
}

func TestWarningClipByName(t *testing.T) {
	for _, name := range warningClipNames() {
		if _, ok := warningClipByName(name); !ok {
			t.Errorf("clip %q not found by name", name)
		}
	}
	if _, ok := warningClipByName("nope"); ok {
		t.Error("unexpected clip for unknown name")
	}
}

// Every rotation clip must be playable as itself on a machine with no TTS
// engine, so each session's warning_sound label names what was really heard.
func TestEveryRotationClipPlaysAsItselfWithoutSpeech(t *testing.T) {
	previous := piperDir
	piperDir = t.TempDir()
	t.Cleanup(func() { piperDir = previous })

	b := &barkMonitor{logger: logging.NewTestLogger(t)}
	for _, clip := range warningClips {
		w := b.prepareWarning([]string{"oreo"}, clip)
		if w.label != clip.name {
			t.Errorf("clip %q played as %q", clip.name, w.label)
		}
		if pcmDuration(w.pcm, w.info) < 300*time.Millisecond {
			t.Errorf("clip %q produced no usable audio", clip.name)
		}
	}
}
