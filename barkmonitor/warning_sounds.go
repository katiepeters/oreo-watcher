package barkmonitor

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand/v2"
	"time"

	"github.com/hajimehoshi/go-mp3"
	rutils "go.viam.com/rdk/utils"
)

//go:embed sounds/warning_tinnitus.mp3
var warningTinnitusMP3 []byte

//go:embed sounds/warning_bugle.mp3
var warningBugleMP3 []byte

//go:embed sounds/warning_whistle.mp3
var warningWhistleMP3 []byte

// warningClip is one sound in the warning rotation. name is what gets
// recorded in the session reading (warning_sound), so it should stay stable
// once data has been collected under it. Exactly one of mp3 or tts is set.
type warningClip struct {
	name string
	mp3  []byte
	// tts marks the spoken "<dog>, bark detected" line (the original
	// warning), synthesized per play since it names the detected dog(s).
	tts bool
}

// warningClips is the rotation a bark session picks its warning sound from.
// Each session picks one at random (see pickWarningClip) and uses it for
// both warnings, so a session's outcome is attributable to a single sound.
var warningClips = []warningClip{
	{name: "tinnitus", mp3: warningTinnitusMP3},
	{name: "bugle", mp3: warningBugleMP3},
	{name: "whistle", mp3: warningWhistleMP3},
	{name: "voice", tts: true},
}

// maxWarningClipDur caps how long any one warning plays. Clips longer than
// this play a random maxWarningClipDur-long section instead of the start.
const maxWarningClipDur = 5 * time.Second

// clipFadeDur is the fade-in/out applied to each played section, so cutting
// into the middle of a waveform doesn't produce an audible click.
const clipFadeDur = 30 * time.Millisecond

// pickWarningClip returns a uniformly random clip from the rotation. Random
// rather than round-robin so module restarts don't bias toward the first clip.
func pickWarningClip() warningClip {
	return warningClips[rand.IntN(len(warningClips))]
}

// warningClipByName returns the named clip from the rotation, if present.
func warningClipByName(name string) (warningClip, bool) {
	for _, c := range warningClips {
		if c.name == name {
			return c, true
		}
	}
	return warningClip{}, false
}

// warningClipNames lists the rotation's clip names, for error messages.
func warningClipNames() []string {
	names := make([]string, len(warningClips))
	for i, c := range warningClips {
		names[i] = c.name
	}
	return names
}

// segment decodes a random section of at most maxDur from the clip and
// returns it as mono PCM16 along with its start offset in seconds. A clip no
// longer than maxDur is returned whole from offset 0.
func (c warningClip) segment(maxDur time.Duration) ([]byte, *rutils.AudioInfo, float64, error) {
	if c.mp3 == nil {
		return nil, nil, 0, fmt.Errorf("clip %s has no MP3 data", c.name)
	}
	dec, err := mp3.NewDecoder(bytes.NewReader(c.mp3))
	if err != nil {
		return nil, nil, 0, fmt.Errorf("decoding %s: %w", c.name, err)
	}
	// go-mp3 always outputs 16-bit little-endian stereo: 4 bytes per frame.
	const bytesPerFrame = 4
	sampleRate := dec.SampleRate()
	totalFrames := dec.Length() / bytesPerFrame
	maxFrames := int64(maxDur.Seconds() * float64(sampleRate))

	var startFrame int64
	wantFrames := totalFrames
	if totalFrames > maxFrames {
		startFrame = rand.Int64N(totalFrames - maxFrames + 1)
		wantFrames = maxFrames
	}
	if _, err := dec.Seek(startFrame*bytesPerFrame, io.SeekStart); err != nil {
		return nil, nil, 0, fmt.Errorf("seeking %s: %w", c.name, err)
	}
	stereo := make([]byte, wantFrames*bytesPerFrame)
	n, err := io.ReadFull(dec, stereo)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, nil, 0, fmt.Errorf("reading %s: %w", c.name, err)
	}
	stereo = stereo[:n-n%bytesPerFrame]

	pcm := downmixToMono(stereo)
	applyFade(pcm, int(clipFadeDur.Seconds()*float64(sampleRate)))

	info := &rutils.AudioInfo{
		Codec:        rutils.CodecPCM16,
		SampleRateHz: int32(sampleRate),
		NumChannels:  1,
	}
	return pcm, info, float64(startFrame) / float64(sampleRate), nil
}

// downmixToMono averages interleaved 16-bit LE stereo frames into mono.
func downmixToMono(stereo []byte) []byte {
	frames := len(stereo) / 4
	mono := make([]byte, frames*2)
	for i := range frames {
		l := int32(int16(binary.LittleEndian.Uint16(stereo[i*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(stereo[i*4+2:])))
		binary.LittleEndian.PutUint16(mono[i*2:], uint16(int16((l+r)/2)))
	}
	return mono
}

// applyFade linearly ramps the first and last fadeSamples of mono PCM16 in
// place. Shrinks the fade if the clip is too short to fit both ramps.
func applyFade(pcm []byte, fadeSamples int) {
	samples := len(pcm) / 2
	if fadeSamples > samples/2 {
		fadeSamples = samples / 2
	}
	for i := range fadeSamples {
		gain := float64(i) / float64(fadeSamples)
		for _, idx := range []int{i, samples - 1 - i} {
			s := float64(int16(binary.LittleEndian.Uint16(pcm[idx*2:])))
			binary.LittleEndian.PutUint16(pcm[idx*2:], uint16(int16(s*gain)))
		}
	}
}
