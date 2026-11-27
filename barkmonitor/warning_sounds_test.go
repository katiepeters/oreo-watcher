package barkmonitor

import (
	"testing"
	"time"
)

func TestWarningClipSegments(t *testing.T) {
	for _, clip := range warningClips {
		if clip.tts {
			continue // synthesized at play time; needs espeak, not an MP3 decode
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
