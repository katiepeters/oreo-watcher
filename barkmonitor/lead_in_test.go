package barkmonitor

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	rutils "go.viam.com/rdk/utils"
)

func TestWithLeadInPrependsAQuietSignalOfTheRequestedLength(t *testing.T) {
	info := &rutils.AudioInfo{Codec: rutils.CodecPCM16, SampleRateHz: 22050, NumChannels: 2}
	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	got := withLeadIn(pcm, info, 300*time.Millisecond)

	if d := pcmDuration(got, info) - pcmDuration(pcm, info); d != 300*time.Millisecond {
		t.Errorf("lead-in lasts %v, want 300ms", d)
	}
	leadIn := got[:len(got)-len(pcm)]
	if len(leadIn)%4 != 0 {
		t.Fatalf("lead-in is %d bytes, not a whole number of stereo frames", len(leadIn))
	}
	for i := 0; i < len(leadIn); i += 2 {
		sample := int16(binary.LittleEndian.Uint16(leadIn[i:]))
		if sample == 0 || sample > leadInAmplitude || sample < -leadInAmplitude {
			t.Fatalf("lead-in sample %d is %d, want non-zero and within ±%d", i/2, sample, leadInAmplitude)
		}
	}
	if !bytes.Equal(got[len(leadIn):], pcm) {
		t.Error("original audio was altered")
	}
}
