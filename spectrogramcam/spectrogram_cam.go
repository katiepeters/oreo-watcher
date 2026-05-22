package spectrogramcam

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"math"
	"time"

	"github.com/fogleman/gg"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"gonum.org/v1/gonum/dsp/fourier"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/spatialmath"
	rutils "go.viam.com/rdk/utils"

	filteredmic "github.com/katie-viam/oreo-watcher/filteredmic"
)

// Model is the model triple for the spectrogram-cam component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "spectrogram-cam")

func init() {
	resource.RegisterComponent(camera.API, Model, resource.Registration[camera.Camera, *Config]{
		Constructor: newSpectrogramCam,
	})
}

// Config holds the component configuration.
type Config struct {
	FilteredMic string `json:"filtered_mic"`
}

// Validate ensures the config is valid and returns required dependencies.
func (c *Config) Validate(path string) ([]string, []string, error) {
	if c.FilteredMic == "" {
		return nil, nil, fmt.Errorf("filtered_mic is required")
	}
	return []string{c.FilteredMic}, nil, nil
}

type spectrogramCam struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	cache  filteredmic.AudioCapture
	name   resource.Name
	logger logging.Logger
}

func newSpectrogramCam(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (camera.Camera, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	dep, ok := deps[audioin.Named(cfg.FilteredMic)]
	if !ok {
		return nil, fmt.Errorf("filtered_mic %q not found in dependencies", cfg.FilteredMic)
	}
	cache, ok := dep.(filteredmic.AudioCapture)
	if !ok {
		return nil, fmt.Errorf("dependency %q does not implement AudioCapture; make sure it is a filtered-mic component", cfg.FilteredMic)
	}

	name := conf.ResourceName()
	cache.RegisterConsumer(name)
	return &spectrogramCam{
		Named:  name.AsNamed(),
		cache:  cache,
		name:   name,
		logger: logger,
	}, nil
}

func (s *spectrogramCam) Images(ctx context.Context, _ []string, _ map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	capture := s.cache.PopCapture(s.name)
	if capture == nil {
		return nil, resource.ResponseMetadata{}, data.ErrNoCaptureToStore
	}

	sampleRate := 44100
	if capture.Chunks[0].AudioInfo != nil && capture.Chunks[0].AudioInfo.SampleRateHz > 0 {
		sampleRate = int(capture.Chunks[0].AudioInfo.SampleRateHz)
	}

	samples := chunksToMono(capture.Chunks)
	if len(samples) < fftSize {
		return nil, resource.ResponseMetadata{}, data.ErrNoCaptureToStore
	}

	startNs := capture.Chunks[0].StartTimestampNanoseconds
	endNs := capture.Chunks[len(capture.Chunks)-1].EndTimestampNanoseconds

	spec := logMelSpectrogram(samples, sampleRate)

	pngBytes, err := renderSpectrogram(spec, capture.CapturedAt, startNs, endNs, capture.DB)
	if err != nil {
		return nil, resource.ResponseMetadata{}, fmt.Errorf("rendering spectrogram: %w", err)
	}

	img, err := camera.NamedImageFromBytes(pngBytes, s.Name().Name, rutils.MimeTypePNG, data.Annotations{})
	if err != nil {
		return nil, resource.ResponseMetadata{}, err
	}

	return []camera.NamedImage{img}, resource.ResponseMetadata{CapturedAt: time.Unix(0, endNs)}, nil
}

func (s *spectrogramCam) NextPointCloud(ctx context.Context, extra map[string]interface{}) (pointcloud.PointCloud, error) {
	return nil, fmt.Errorf("NextPointCloud not supported by spectrogram-cam")
}

func (s *spectrogramCam) Properties(ctx context.Context) (camera.Properties, error) {
	return camera.Properties{
		SupportsPCD: false,
		MimeTypes:   []string{rutils.MimeTypePNG},
	}, nil
}

func (s *spectrogramCam) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return []spatialmath.Geometry{}, nil
}

// STFT / mel parameters.
const (
	fftSize = 1024
	hopSize = fftSize / 4
	nMels   = 80
	melFMin = 50.0
	melFMax = 8000.0
)

// Output image dimensions.
const (
	specImgWidth = 1024
	specImgH     = 512 // spectrogram area
	specCaptionH = 48
	specTotalH   = specImgH + specCaptionH
)

// chunksToMono decodes chunks to normalized mono float64 samples, averaging channels.
func chunksToMono(chunks []*audioin.AudioChunk) []float64 {
	var samples []float64
	for _, chunk := range chunks {
		if chunk == nil || len(chunk.AudioData) == 0 || chunk.AudioInfo == nil {
			continue
		}
		nCh := int(chunk.AudioInfo.NumChannels)
		if nCh <= 0 {
			nCh = 1
		}
		switch chunk.AudioInfo.Codec {
		case rutils.CodecPCM32Float:
			stride := 4 * nCh
			for i := 0; i+stride <= len(chunk.AudioData); i += stride {
				var sum float64
				for ch := 0; ch < nCh; ch++ {
					bits := binary.LittleEndian.Uint32(chunk.AudioData[i+ch*4:])
					sum += float64(math.Float32frombits(bits))
				}
				samples = append(samples, sum/float64(nCh))
			}
		default: // PCM16
			stride := 2 * nCh
			for i := 0; i+stride <= len(chunk.AudioData); i += stride {
				var sum float64
				for ch := 0; ch < nCh; ch++ {
					s := int16(binary.LittleEndian.Uint16(chunk.AudioData[i+ch*2:]))
					sum += float64(s) / 32768.0
				}
				samples = append(samples, sum/float64(nCh))
			}
		}
	}
	return samples
}

// logMelSpectrogram computes a log-mel spectrogram, returning [nFrames][nMels] in dB.
func logMelSpectrogram(samples []float64, sampleRate int) [][]float64 {
	// Hann window
	win := make([]float64, fftSize)
	for i := range win {
		win[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(fftSize-1)))
	}

	fMax := math.Min(melFMax, float64(sampleRate)/2.0)
	filters := melFilterbank(nMels, fftSize, sampleRate, melFMin, fMax)

	fft := fourier.NewFFT(fftSize)
	coeff := make([]complex128, fftSize/2+1)
	frame := make([]float64, fftSize)

	var frames [][]float64
	for start := 0; start+fftSize <= len(samples); start += hopSize {
		for i := 0; i < fftSize; i++ {
			frame[i] = samples[start+i] * win[i]
		}
		coeff = fft.Coefficients(coeff, frame)

		mel := make([]float64, nMels)
		for m, filter := range filters {
			var energy float64
			for k, w := range filter {
				if w == 0 {
					continue
				}
				r, im := real(coeff[k]), imag(coeff[k])
				energy += w * (r*r + im*im)
			}
			mel[m] = 10 * math.Log10(math.Max(energy, 1e-10))
		}
		frames = append(frames, mel)
	}
	return frames
}

// melFilterbank builds [nMels][fftSize/2+1] triangular filter weights.
func melFilterbank(nMels, fftSize, sampleRate int, fMin, fMax float64) [][]float64 {
	nBins := fftSize/2 + 1
	melMin := hzToMel(fMin)
	melMax := hzToMel(fMax)

	// nMels+2 equally spaced points in mel space
	melPts := make([]float64, nMels+2)
	for i := range melPts {
		melPts[i] = melMin + float64(i)*(melMax-melMin)/float64(nMels+1)
	}

	// Convert to FFT bin indices
	bins := make([]int, nMels+2)
	for i, mel := range melPts {
		b := int(math.Round(melToHz(mel) * float64(fftSize) / float64(sampleRate)))
		bins[i] = clampInt(b, 0, nBins-1)
	}

	filters := make([][]float64, nMels)
	for m := 0; m < nMels; m++ {
		filters[m] = make([]float64, nBins)
		left, center, right := bins[m], bins[m+1], bins[m+2]

		if center > left {
			for k := left; k <= center && k < nBins; k++ {
				filters[m][k] = float64(k-left) / float64(center-left)
			}
		} else if center < nBins {
			filters[m][center] = 1.0
		}
		if right > center {
			for k := center; k <= right && k < nBins; k++ {
				filters[m][k] = float64(right-k) / float64(right-center)
			}
		}
	}
	return filters
}

func hzToMel(hz float64) float64 { return 2595 * math.Log10(1+hz/700) }
func melToHz(mel float64) float64 { return 700 * (math.Pow(10, mel/2595) - 1) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// renderSpectrogram draws the spectrogram as a PNG with a caption bar.
func renderSpectrogram(frames [][]float64, capturedAt time.Time, startNs, endNs int64, db float64) ([]byte, error) {
	if len(frames) == 0 {
		return nil, fmt.Errorf("no spectrogram frames")
	}

	// Per-clip normalization: find min/max dB across all frames.
	minDB, maxDB := math.Inf(1), math.Inf(-1)
	for _, frame := range frames {
		for _, v := range frame {
			if v < minDB {
				minDB = v
			}
			if v > maxDB {
				maxDB = v
			}
		}
	}
	// Floor at -80 so silence is consistently black.
	minDB = math.Max(minDB, -80)
	dbRange := maxDB - minDB
	if dbRange < 1 {
		dbRange = 1
	}

	img := image.NewNRGBA(image.Rect(0, 0, specImgWidth, specTotalH))

	// Fill caption area background.
	captionColor := color.NRGBA{R: 30, G: 30, B: 30, A: 255}
	for y := specImgH; y < specTotalH; y++ {
		for x := 0; x < specImgWidth; x++ {
			img.SetNRGBA(x, y, captionColor)
		}
	}

	nFrames := len(frames)
	nBins := len(frames[0])

	for x := 0; x < specImgWidth; x++ {
		fi := x * nFrames / specImgWidth
		if fi >= nFrames {
			fi = nFrames - 1
		}
		frame := frames[fi]

		for y := 0; y < specImgH; y++ {
			// y=0 is top of image = high frequency
			mi := (specImgH-1-y) * nBins / specImgH
			if mi >= nBins {
				mi = nBins - 1
			}
			t := (frame[mi] - minDB) / dbRange
			if t < 0 {
				t = 0
			} else if t > 1 {
				t = 1
			}
			r, g, b := magmaColor(t)
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(r * 255),
				G: uint8(g * 255),
				B: uint8(b * 255),
				A: 255,
			})
		}
	}

	// Overlay caption text using gg.
	dc := gg.NewContextForImage(img)
	captured := capturedAt.Local().Format("2006-01-02 15:04:05.000")
	startTime := time.Unix(0, startNs).Local().Format("15:04:05.000")
	endTime := time.Unix(0, endNs).Local().Format("15:04:05.000")
	caption := fmt.Sprintf("%s  |  %s – %s    %.1f dBFS", captured, startTime, endTime, db)

	f, err := opentype.Parse(goregular.TTF)
	if err == nil {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 20, DPI: 72})
		if err == nil {
			dc.SetFontFace(face)
		}
	}
	dc.SetRGB(0.85, 0.85, 0.85)
	dc.DrawStringAnchored(caption, float64(specImgWidth)/2, float64(specImgH)+float64(specCaptionH)/2, 0.5, 0.5)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderChunks renders a spectrogram PNG directly from AudioChunks.
// Exported so other components (e.g. bark-monitor) can classify the same image.
func RenderChunks(chunks []*audioin.AudioChunk, sampleRate int, capturedAt time.Time, startNs, endNs int64, db float64) ([]byte, error) {
	samples := chunksToMono(chunks)
	if len(samples) < fftSize {
		return nil, fmt.Errorf("not enough samples for spectrogram (%d < %d)", len(samples), fftSize)
	}
	return RenderSamples(samples, sampleRate, capturedAt, startNs, endNs, db)
}

// RenderSamples computes a log-mel spectrogram from raw mono float64 samples
// and returns a PNG using the same parameters as the spectrogram-cam component.
func RenderSamples(samples []float64, sampleRate int, capturedAt time.Time, startNs, endNs int64, db float64) ([]byte, error) {
	frames := logMelSpectrogram(samples, sampleRate)
	if len(frames) == 0 {
		return nil, fmt.Errorf("no spectrogram frames produced")
	}
	return renderSpectrogram(frames, capturedAt, startNs, endNs, db)
}

// magmaColor maps t in [0,1] to an RGB triple using a magma-like palette.
func magmaColor(t float64) (r, g, b float64) {
	stops := [5][3]float64{
		{0.001, 0.000, 0.014}, // 0.00 — nearly black
		{0.230, 0.005, 0.278}, // 0.25 — dark purple
		{0.717, 0.107, 0.387}, // 0.50 — deep red
		{0.980, 0.531, 0.133}, // 0.75 — orange
		{0.988, 0.998, 0.644}, // 1.00 — pale yellow
	}
	idx := t * 4.0
	i := int(idx)
	if i >= 4 {
		return stops[4][0], stops[4][1], stops[4][2]
	}
	f := idx - float64(i)
	r = stops[i][0] + f*(stops[i+1][0]-stops[i][0])
	g = stops[i][1] + f*(stops[i+1][1]-stops[i][1])
	b = stops[i][2] + f*(stops[i+1][2]-stops[i][2])
	return
}
