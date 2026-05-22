package waveformcam

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/fogleman/gg"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"

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

// Model is the model triple for the waveform-cam component.
var Model = resource.NewModel("katie-viam", "oreo-watcher", "waveform-cam")

func init() {
	resource.RegisterComponent(camera.API, Model, resource.Registration[camera.Camera, *Config]{
		Constructor: newWaveformCam,
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

type waveformCam struct {
	resource.Named
	resource.AlwaysRebuild
	resource.TriviallyCloseable

	cache  filteredmic.AudioCapture
	name   resource.Name
	logger logging.Logger
}

func newWaveformCam(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (camera.Camera, error) {
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
	return &waveformCam{
		Named:  name.AsNamed(),
		cache:  cache,
		name:   name,
		logger: logger,
	}, nil
}

func (w *waveformCam) Images(ctx context.Context, _ []string, _ map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	capture := w.cache.PopCapture(w.name)
	if capture == nil {
		return nil, resource.ResponseMetadata{}, data.ErrNoCaptureToStore
	}

	samples := chunksToSamples(capture.Chunks)
	if len(samples) == 0 {
		return nil, resource.ResponseMetadata{}, data.ErrNoCaptureToStore
	}

	startNs := capture.Chunks[0].StartTimestampNanoseconds
	endNs := capture.Chunks[len(capture.Chunks)-1].EndTimestampNanoseconds

	pngBytes, err := renderWaveform(samples, capture.CapturedAt, startNs, endNs, capture.DB)
	if err != nil {
		return nil, resource.ResponseMetadata{}, fmt.Errorf("rendering waveform: %w", err)
	}

	img, err := camera.NamedImageFromBytes(pngBytes, w.Name().Name, rutils.MimeTypePNG, data.Annotations{})
	if err != nil {
		return nil, resource.ResponseMetadata{}, err
	}

	return []camera.NamedImage{img}, resource.ResponseMetadata{CapturedAt: time.Unix(0, endNs)}, nil
}

func (w *waveformCam) NextPointCloud(ctx context.Context, extra map[string]interface{}) (pointcloud.PointCloud, error) {
	return nil, fmt.Errorf("NextPointCloud not supported by waveform-cam")
}

func (w *waveformCam) Properties(ctx context.Context) (camera.Properties, error) {
	return camera.Properties{
		SupportsPCD: false,
		MimeTypes:   []string{rutils.MimeTypePNG},
	}, nil
}

func (w *waveformCam) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return []spatialmath.Geometry{}, nil
}

// chunksToSamples decodes all audio chunks into normalized float64 samples in [-1, 1].
func chunksToSamples(chunks []*audioin.AudioChunk) []float64 {
	var samples []float64
	for _, chunk := range chunks {
		if chunk == nil || len(chunk.AudioData) == 0 || chunk.AudioInfo == nil {
			continue
		}
		switch chunk.AudioInfo.Codec {
		case rutils.CodecPCM32Float:
			for i := 0; i+3 < len(chunk.AudioData); i += 4 {
				bits := binary.LittleEndian.Uint32(chunk.AudioData[i:])
				s := float64(math.Float32frombits(bits))
				samples = append(samples, clamp(s, -1.0, 1.0))
			}
		default: // PCM16
			for i := 0; i+1 < len(chunk.AudioData); i += 2 {
				s := int16(binary.LittleEndian.Uint16(chunk.AudioData[i:]))
				samples = append(samples, float64(s)/32768.0)
			}
		}
	}
	return samples
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

const (
	imgWidth  = 900
	imgHeight = 220
	captionH  = 30
	waveformH = imgHeight - captionH
	lineWidth = 1.5
)

// renderWaveform draws a waveform image with a timestamp caption and dB level.
func renderWaveform(samples []float64, capturedAt time.Time, startNs, endNs int64, db float64) ([]byte, error) {
	dc := gg.NewContext(imgWidth, imgHeight)

	// Background
	dc.SetRGB(0.05, 0.05, 0.05)
	dc.Clear()

	// Center line
	midY := float64(waveformH) / 2.0
	dc.SetRGB(0.2, 0.2, 0.2)
	dc.SetLineWidth(0.5)
	dc.DrawLine(0, midY, float64(imgWidth), midY)
	dc.Stroke()

	// Waveform: for each x pixel draw a vertical line spanning the min–max sample range.
	// Use fixed padding so peaks never touch the image edge.
	const pad = 12.0
	amplitude := midY - pad

	dc.SetRGB(0.2, 0.9, 0.4)
	dc.SetLineWidth(lineWidth)
	n := len(samples)
	for x := 0; x < imgWidth; x++ {
		lo := x * n / imgWidth
		hi := (x+1)*n/imgWidth - 1
		if hi < lo {
			hi = lo
		}
		minS, maxS := samples[lo], samples[lo]
		for i := lo + 1; i <= hi && i < n; i++ {
			if samples[i] < minS {
				minS = samples[i]
			}
			if samples[i] > maxS {
				maxS = samples[i]
			}
		}
		y1 := midY - maxS*amplitude
		y2 := midY - minS*amplitude
		if math.Abs(y2-y1) < 1 {
			y2 = y1 + 1
		}
		dc.DrawLine(float64(x), y1, float64(x), y2)
		dc.Stroke()
	}

	// Caption bar
	dc.SetRGB(0.12, 0.12, 0.12)
	dc.DrawRectangle(0, float64(waveformH), float64(imgWidth), captionH)
	dc.Fill()

	captured := capturedAt.Local().Format("2006-01-02 15:04:05.000")
	startTime := time.Unix(0, startNs).Local().Format("15:04:05.000")
	endTime := time.Unix(0, endNs).Local().Format("15:04:05.000")
	caption := fmt.Sprintf("%s  |  %s – %s    %.1f dBFS", captured, startTime, endTime, db)

	f, err := opentype.Parse(goregular.TTF)
	if err == nil {
		face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 18, DPI: 72})
		if err == nil {
			dc.SetFontFace(face)
		}
	}
	dc.SetRGB(0.85, 0.85, 0.85)
	dc.DrawStringAnchored(caption, float64(imgWidth)/2, float64(waveformH)+float64(captionH)/2, 0.5, 0.5)

	var buf bytes.Buffer
	if err := dc.EncodePNG(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
