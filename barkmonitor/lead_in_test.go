package barkmonitor

import (
	"bytes"
	"testing"
	"time"

	rutils "go.viam.com/rdk/utils"
)

func TestWithLeadInPrependsSilenceOfTheRequestedLength(t *testing.T) {
	info := &rutils.AudioInfo{Codec: rutils.CodecPCM16, SampleRateHz: 22050, NumChannels: 2}
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	got := withLeadIn(pcm, info, 300*time.Millisecond)

	if d := pcmDuration(got, info) - pcmDuration(pcm, info); d != 300*time.Millisecond {
		t.Errorf("lead-in lasts %v, want 300ms", d)
	}
	silence := got[:len(got)-len(pcm)]
	if len(silence)%4 != 0 {
		t.Errorf("lead-in is %d bytes, not a whole number of stereo frames", len(silence))
	}
	if !bytes.Equal(silence, make([]byte, len(silence))) {
		t.Error("lead-in is not silent")
	}
	if !bytes.Equal(got[len(silence):], pcm) {
		t.Error("original audio was altered")
	}
}
