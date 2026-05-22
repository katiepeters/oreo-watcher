// spectrogen slices a WAV or MP3 file into fixed-duration chunks, writes one
// log-mel spectrogram PNG per chunk, and optionally uploads them to Viam.
//
// Usage:
//
//	spectrogen [flags] input.(wav|mp3)
//
// Flags:
//
//	-duration float     chunk length in seconds (default 1)
//	-out string         output directory for PNGs (default ".")
//	-prefix string      filename prefix (default: input filename stem)
//	-tags string        comma-separated tags to apply (e.g. "not-barking,traffic")
//	-upload             upload PNGs to Viam after generating them
//	-api-key string     Viam API key (or set VIAM_API_KEY)
//	-api-key-id string  Viam API key ID (or set VIAM_API_KEY_ID)
//	-part-id string     robot part ID to associate uploads with
//	-component string   component name to tag uploads with (default "spectrogram-cam-fake")
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	mp3dec "github.com/hajimehoshi/go-mp3"
	rdkapp "go.viam.com/rdk/app"
	"go.viam.com/rdk/logging"

	spectrogramcam "github.com/katie-viam/oreo-watcher/spectrogramcam"
)

func main() {
	duration := flag.Float64("duration", 1.0, "chunk duration in seconds")
	outDir := flag.String("out", ".", "output directory for PNGs")
	prefix := flag.String("prefix", "", "output filename prefix (default: input file stem)")
	tagsCSV := flag.String("tags", "", "comma-separated tags to apply to uploads")
	upload := flag.Bool("upload", false, "upload generated PNGs to Viam")
	apiKey := flag.String("api-key", "", "Viam API key (or set VIAM_API_KEY)")
	apiKeyID := flag.String("api-key-id", "", "Viam API key ID (or set VIAM_API_KEY_ID)")
	partID := flag.String("part-id", "", "robot part ID to associate uploads with")
	component := flag.String("component", "spectrogram-cam-fake", "component name for upload metadata")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: spectrogen [flags] input.(wav|mp3)")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Resolve API key from flags or environment.
	key := *apiKey
	if key == "" {
		key = os.Getenv("VIAM_API_KEY")
	}
	keyID := *apiKeyID
	if keyID == "" {
		keyID = os.Getenv("VIAM_API_KEY_ID")
	}
	if *upload && (key == "" || keyID == "" || *partID == "") {
		log.Fatal("-upload requires -api-key (or VIAM_API_KEY), -api-key-id (or VIAM_API_KEY_ID), and -part-id")
	}

	var tags []string
	if *tagsCSV != "" {
		for _, t := range strings.Split(*tagsCSV, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}

	inputPath := flag.Arg(0)
	f, err := os.Open(inputPath)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Fatal(err)
	}
	fileTime := info.ModTime()

	var samples []float64
	var sampleRate int

	switch strings.ToLower(filepath.Ext(inputPath)) {
	case ".mp3":
		samples, sampleRate, err = decodeMp3(f)
		if err != nil {
			log.Fatalf("decoding MP3: %v", err)
		}
	default:
		wav, werr := parseWAV(f)
		if werr != nil {
			log.Fatalf("parsing WAV: %v", werr)
		}
		sampleRate = wav.SampleRate
		samples, err = wavToMono(wav)
		if err != nil {
			log.Fatalf("decoding samples: %v", err)
		}
	}

	stem := *prefix
	if stem == "" {
		stem = strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	chunkLen := int(math.Round(float64(sampleRate) * *duration))
	if chunkLen < 1 {
		log.Fatal("chunk duration too short")
	}
	chunkDurNs := int64(*duration * float64(time.Second))

	numChunks := len(samples) / chunkLen
	if numChunks == 0 {
		log.Fatalf("audio is shorter than one %.1fs chunk", *duration)
	}

	// Set up Viam client if uploading.
	var dataClient *rdkapp.DataClient
	if *upload {
		logger := logging.NewLogger("spectrogen")
		vc, err := rdkapp.CreateViamClientWithAPIKey(context.Background(), rdkapp.Options{}, key, keyID, logger)
		if err != nil {
			log.Fatalf("connecting to Viam: %v", err)
		}
		defer vc.Close()
		dataClient = vc.DataClient()
	}

	for i := 0; i < numChunks; i++ {
		chunk := samples[i*chunkLen : (i+1)*chunkLen]

		startNs := fileTime.UnixNano() + int64(i)*chunkDurNs
		endNs := startNs + chunkDurNs
		capturedAt := time.Unix(0, endNs)
		db := rmsDB(chunk)

		png, err := spectrogramcam.RenderSamples(chunk, sampleRate, capturedAt, startNs, endNs, db)
		if err != nil {
			log.Printf("chunk %04d: render failed: %v", i, err)
			continue
		}

		outPath := filepath.Join(*outDir, fmt.Sprintf("%s_%04d.png", stem, i))
		if err := os.WriteFile(outPath, png, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s  (%.1f dBFS)", outPath, db)

		if dataClient != nil {
			start := time.Unix(0, startNs)
			end := time.Unix(0, endNs)
			opts := &rdkapp.BinaryDataCaptureUploadOptions{
				Tags:             tags,
				DataRequestTimes: &[2]time.Time{start, end},
			}
			id, err := dataClient.BinaryDataCaptureUpload(
				context.Background(),
				png,
				*partID,
				"camera",
				*component,
				"GetImages",
				".png",
				opts,
			)
			if err != nil {
				log.Printf("  upload failed: %v", err)
			} else {
				fmt.Printf("  → uploaded %s", id)
			}
		}
		fmt.Println()
	}

	fmt.Printf("done: %d chunks from %d total samples\n", numChunks, len(samples))
}

// decodeMp3 decodes an MP3 file to normalized mono float64 samples.
// go-mp3 always outputs stereo PCM16 little-endian at the source sample rate.
func decodeMp3(r io.Reader) ([]float64, int, error) {
	d, err := mp3dec.NewDecoder(r)
	if err != nil {
		return nil, 0, err
	}
	raw, err := io.ReadAll(d)
	if err != nil {
		return nil, 0, err
	}
	// go-mp3 outputs stereo (2 channels) PCM16 LE regardless of source channels.
	const nCh = 2
	stride := 2 * nCh
	samples := make([]float64, 0, len(raw)/stride)
	for i := 0; i+stride <= len(raw); i += stride {
		var sum float64
		for ch := 0; ch < nCh; ch++ {
			s := int16(binary.LittleEndian.Uint16(raw[i+ch*2:]))
			sum += float64(s) / 32768.0
		}
		samples = append(samples, sum/float64(nCh))
	}
	return samples, d.SampleRate(), nil
}

// rmsDB returns the dBFS level of a mono float64 sample slice.
func rmsDB(samples []float64) float64 {
	if len(samples) == 0 {
		return -100
	}
	var sum float64
	for _, s := range samples {
		sum += s * s
	}
	rms := math.Sqrt(sum / float64(len(samples)))
	if rms == 0 {
		return -100
	}
	return 20 * math.Log10(rms)
}

// wavFile holds the decoded header fields and raw PCM data from a WAV file.
type wavFile struct {
	AudioFormat   int // 1 = PCM, 3 = IEEE float
	NumChannels   int
	SampleRate    int
	BitsPerSample int
	Data          []byte
}

// parseWAV reads and parses a RIFF/WAVE file.
func parseWAV(r io.Reader) (*wavFile, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(raw) < 12 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a valid RIFF/WAVE file")
	}

	var w wavFile
	pos := 12
	for pos+8 <= len(raw) {
		id := string(raw[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(raw[pos+4:]))
		pos += 8
		end := pos + size

		switch id {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("fmt chunk too small (%d bytes)", size)
			}
			w.AudioFormat = int(binary.LittleEndian.Uint16(raw[pos:]))
			w.NumChannels = int(binary.LittleEndian.Uint16(raw[pos+2:]))
			w.SampleRate = int(binary.LittleEndian.Uint32(raw[pos+4:]))
			w.BitsPerSample = int(binary.LittleEndian.Uint16(raw[pos+14:]))
		case "data":
			if end > len(raw) {
				end = len(raw)
			}
			w.Data = raw[pos:end]
		}

		pos = end
		if size%2 != 0 {
			pos++ // WAV chunks are word-aligned
		}
	}

	if w.SampleRate == 0 {
		return nil, fmt.Errorf("no fmt chunk found")
	}
	if len(w.Data) == 0 {
		return nil, fmt.Errorf("no data chunk found")
	}
	return &w, nil
}

// wavToMono decodes WAV PCM data to normalised mono float64 samples in [-1, 1].
// Supports PCM 16-bit, PCM 24-bit, PCM 32-bit int, and IEEE float 32-bit.
func wavToMono(w *wavFile) ([]float64, error) {
	nCh := w.NumChannels
	if nCh < 1 {
		return nil, fmt.Errorf("invalid channel count: %d", nCh)
	}

	var samples []float64

	switch {
	case w.AudioFormat == 1 && w.BitsPerSample == 16:
		stride := 2 * nCh
		for i := 0; i+stride <= len(w.Data); i += stride {
			var sum float64
			for ch := 0; ch < nCh; ch++ {
				s := int16(binary.LittleEndian.Uint16(w.Data[i+ch*2:]))
				sum += float64(s) / 32768.0
			}
			samples = append(samples, sum/float64(nCh))
		}

	case w.AudioFormat == 1 && w.BitsPerSample == 24:
		stride := 3 * nCh
		for i := 0; i+stride <= len(w.Data); i += stride {
			var sum float64
			for ch := 0; ch < nCh; ch++ {
				b := w.Data[i+ch*3:]
				val := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
				sum += float64(val) / 8388608.0
			}
			samples = append(samples, sum/float64(nCh))
		}

	case w.AudioFormat == 1 && w.BitsPerSample == 32:
		stride := 4 * nCh
		for i := 0; i+stride <= len(w.Data); i += stride {
			var sum float64
			for ch := 0; ch < nCh; ch++ {
				s := int32(binary.LittleEndian.Uint32(w.Data[i+ch*4:]))
				sum += float64(s) / 2147483648.0
			}
			samples = append(samples, sum/float64(nCh))
		}

	case w.AudioFormat == 3 && w.BitsPerSample == 32:
		stride := 4 * nCh
		for i := 0; i+stride <= len(w.Data); i += stride {
			var sum float64
			for ch := 0; ch < nCh; ch++ {
				bits := binary.LittleEndian.Uint32(w.Data[i+ch*4:])
				s := float64(math.Float32frombits(bits))
				if s > 1 {
					s = 1
				} else if s < -1 {
					s = -1
				}
				sum += s
			}
			samples = append(samples, sum/float64(nCh))
		}

	default:
		return nil, fmt.Errorf("unsupported WAV format: audioFormat=%d bitsPerSample=%d", w.AudioFormat, w.BitsPerSample)
	}

	return samples, nil
}
