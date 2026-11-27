package barkmonitor

import (
	"encoding/binary"
	"math"
	"time"

	rutils "go.viam.com/rdk/utils"
)

// chimeNotes is the short rising two-note chime played ahead of a spoken
// announcement. The speaker takes a moment to produce sound after being idle
// and swallows whatever comes first; the chime takes that loss instead of the
// first word, and tells the listener something is about to be said.
var chimeNotes = []struct {
	hz  float64
	dur time.Duration
}{
	{660, 140 * time.Millisecond},
	{880, 220 * time.Millisecond},
}

const (
	// chimeAmplitude is the chime's peak as a fraction of full scale.
	chimeAmplitude = 0.2
	// chimeGap is the pause between the chime and the speech.
	chimeGap = 150 * time.Millisecond
)

// announcement returns speech (16-bit PCM) preceded by the chime, with the
// whole thing scaled to volume (0-1].
func announcement(speech []byte, info *rutils.AudioInfo, volume float64) []byte {
	out := append(chime(info), speech...)
	for i := 0; i+1 < len(out); i += 2 {
		sample := float64(int16(binary.LittleEndian.Uint16(out[i:])))
		binary.LittleEndian.PutUint16(out[i:], uint16(int16(math.Round(sample*volume))))
	}
	return out
}

// chime renders chimeNotes followed by chimeGap in info's format. Each note is
// shaped by a half-sine envelope so it starts and ends at zero without a click.
func chime(info *rutils.AudioInfo) []byte {
	rate, channels := float64(info.SampleRateHz), int(info.NumChannels)
	var out []byte
	for _, note := range chimeNotes {
		frames := int(note.dur.Seconds() * rate)
		for f := 0; f < frames; f++ {
			envelope := math.Sin(math.Pi * float64(f) / float64(frames))
			value := chimeAmplitude * envelope * math.Sin(2*math.Pi*note.hz*float64(f)/rate)
			sample := uint16(int16(math.Round(value * math.MaxInt16)))
			for ch := 0; ch < channels; ch++ {
				out = binary.LittleEndian.AppendUint16(out, sample)
			}
		}
	}
	gapFrames := int(chimeGap.Seconds() * rate)
	return append(out, make([]byte, gapFrames*channels*2)...)
}
