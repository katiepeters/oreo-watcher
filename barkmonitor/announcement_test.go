package barkmonitor

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	rutils "go.viam.com/rdk/utils"
)

func samples(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

func peak(s []int16) float64 {
	var p float64
	for _, v := range s {
		p = math.Max(p, math.Abs(float64(v)))
	}
	return p
}

func TestChimeIsShortQuietAndClickFree(t *testing.T) {
	for _, info := range []*rutils.AudioInfo{
		{Codec: rutils.CodecPCM16, SampleRateHz: 22050, NumChannels: 1},
		{Codec: rutils.CodecPCM16, SampleRateHz: 48000, NumChannels: 2},
	} {
		pcm := chime(info)
		if d := pcmDuration(pcm, info); d < 300*time.Millisecond || d > time.Second {
			t.Errorf("%dHz: chime lasts %v, want between 300ms and 1s", info.SampleRateHz, d)
		}
		s := samples(pcm)
		if p := peak(s); p < 0.05*math.MaxInt16 || p > 0.3*math.MaxInt16 {
			t.Errorf("%dHz: chime peak is %.0f, want audible but well under full scale", info.SampleRateHz, p)
		}
		if first := math.Abs(float64(s[0])); first > 50 {
			t.Errorf("%dHz: chime starts at %v, which would click", info.SampleRateHz, first)
		}
		if tail := peak(s[len(s)-int(info.NumChannels)*100:]); tail != 0 {
			t.Errorf("%dHz: chime does not end in silence before the speech", info.SampleRateHz)
		}
	}
}

func TestAnnouncementKeepsSpeechAfterChimeAndScalesVolume(t *testing.T) {
	info := &rutils.AudioInfo{Codec: rutils.CodecPCM16, SampleRateHz: 22050, NumChannels: 1}
	speech := binary.LittleEndian.AppendUint16(nil, uint16(int16(10000)))
	speech = binary.LittleEndian.AppendUint16(speech, uint16(0x10000-10000))

	full := samples(announcement(speech, info, 1))
	half := samples(announcement(speech, info, 0.5))

	if got := full[len(full)-2:]; got[0] != 10000 || got[1] != -10000 {
		t.Errorf("at volume 1 the speech is %v, want it unchanged after the chime", got)
	}
	if got := half[len(half)-2:]; got[0] != 5000 || got[1] != -5000 {
		t.Errorf("at volume 0.5 the speech is %v, want [5000 -5000]", got)
	}
	if ratio := peak(half[:len(half)-2]) / peak(full[:len(full)-2]); math.Abs(ratio-0.5) > 0.01 {
		t.Errorf("chime scaled by %.3f at volume 0.5, want 0.5", ratio)
	}
}

func TestSayRejectsOutOfRangeVolume(t *testing.T) {
	b := &barkMonitor{}
	for _, volume := range []interface{}{0.0, -1.0, 1.5, "loud"} {
		cmd := map[string]interface{}{"command": "say", "text": "hi", "volume": volume}
		if _, err := b.DoCommand(context.Background(), cmd); err == nil {
			t.Errorf("volume %v: want an error", volume)
		}
	}
}
