package barkmonitor

import (
	"encoding/binary"
	"math"
	"time"

	rutils "go.viam.com/rdk/utils"
)

// chimeNotes is the short rising two-note chime played ahead of a spoken
// announcement. The speaker fades its volume up over the first sound it plays
// after being quiet; the chime takes that fade instead of the first word.
var chimeNotes = []struct {
	hz  float64
	dur time.Duration
}{
	{660, 160 * time.Millisecond},
	{880, 260 * time.Millisecond},
}

const (
	// chimeAmplitude is the chime's peak as a fraction of full scale. It is
	// near speech level on purpose: a quieter chime did not wake the speaker.
	chimeAmplitude = 0.5
	// chimeOvertone is the level of each note's octave overtone relative to
	// its fundamental, which makes the chime less of a pure tone.
	chimeOvertone = 0.3
	// chimeGap is the pause between the chime and the speech, kept short so
	// the speaker has no time to go quiet again.
	chimeGap = 40 * time.Millisecond
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
			phase := 2 * math.Pi * note.hz * float64(f) / rate
			tone := (math.Sin(phase) + chimeOvertone*math.Sin(2*phase)) / (1 + chimeOvertone)
			value := chimeAmplitude * envelope * tone
			sample := uint16(int16(math.Round(value * math.MaxInt16)))
			for ch := 0; ch < channels; ch++ {
				out = binary.LittleEndian.AppendUint16(out, sample)
			}
		}
	}
	gapFrames := int(chimeGap.Seconds() * rate)
	return append(out, make([]byte, gapFrames*channels*2)...)
}
