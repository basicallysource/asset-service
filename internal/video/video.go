// Package video turns one uploaded video into the encodes a page should
// actually download, plus a still to show before it plays.
//
// It is the same bargain as imaging: keep the original exactly as it was
// given, and serve something smaller. A camera's own file is tens of megabytes
// of footage nobody's browser should be asked to download to see a ten-second
// clip inline.
//
// Unlike imaging this cannot be pure Go -- there is no Go H.264 encoder worth
// having -- so it drives ffmpeg, which must be on PATH. Where it is not,
// Supported says no and videos simply have no derived forms, which is what the
// service did before this package existed.
package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/basicallysource/asset-service/internal/imaging"
)

// What an encode produces. One format, for the same reason imaging has one:
// H.264 in MP4 plays everywhere, so the ladder is a list of sizes rather than
// a matrix of sizes and codecs.
const (
	OutputContentType = "video/mp4"
	OutputExtension   = ".mp4"
)

// PosterName is the rendition that is a still image rather than a video. A
// client tells it apart from the encodes by its content type, not its name.
const PosterName = "poster"

// DefaultWidths are the two sizes worth keeping: one for a phone, one for a
// desktop. More rungs would multiply encode time for differences nobody sees
// on a short inline clip.
var DefaultWidths = []int{960, 1920}

// Defaults for an encode. CRF 26 with a medium preset is where H.264 stops
// showing obvious artefacts on real footage without the file getting silly.
const (
	DefaultCRF           = 26
	DefaultPreset        = "medium"
	DefaultPosterWidth   = 1280
	DefaultPosterQuality = 82
)

// posterOffsetSeconds is where the still is taken from. The first frame of a
// clip is very often black or mid-transition; a second in is representative.
const posterOffsetSeconds = 1.0

// ErrUnsupported means the bytes are not a video ffmpeg can read.
var ErrUnsupported = errors.New("video: unsupported video")

// Rendition is one derived form: an encode, or the poster.
type Rendition struct {
	// Name is how the ladder refers to this rung, e.g. "w960" or "poster".
	Name        string
	Width       int
	Height      int
	ContentType string
	Extension   string
	Bytes       []byte
}

// Options controls what a ladder contains.
type Options struct {
	// Widths to produce. A width at or above the source's is skipped.
	Widths []int
	// CRF is the H.264 quality, lower being better and larger.
	CRF int
	// Preset is the libx264 speed/size tradeoff.
	Preset string
	// PosterWidth and PosterQuality describe the still. A zero PosterWidth
	// uses the default; a negative one produces no poster.
	PosterWidth   int
	PosterQuality int
	// WorkDir is where intermediate files go. Empty means the system
	// temporary directory.
	WorkDir string
}

// Available reports whether the tools this package needs are installed. It is
// looked up once: the answer cannot change without restarting the process.
var Available = sync.OnceValue(func() bool {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			return false
		}
	}
	return true
})

// Supported reports whether a content type is worth handing to Ladder.
func Supported(contentType string) bool {
	switch contentType {
	case "video/mp4", "video/quicktime", "video/webm", "video/x-matroska",
		"video/x-m4v", "video/x-msvideo", "video/mpeg":
		return Available()
	default:
		return false
	}
}

// Ladder reads the video at path and returns its derived forms, smallest
// first, with the poster last.
//
// Every configured width no wider than the source is encoded, and a source
// narrower than all of them is encoded once at its own width. Unlike an image,
// re-encoding a video at the size it already is remains the point of the
// exercise: what comes off a camera is tens of megabytes a minute, and the
// same frames at the same size are a tenth of that. What is never done is
// upscaling, which invents detail and charges for it.
func Ladder(ctx context.Context, path string, opts Options) ([]Rendition, error) {
	if !Available() {
		return nil, fmt.Errorf("%w: ffmpeg is not installed", ErrUnsupported)
	}
	if len(opts.Widths) == 0 {
		opts.Widths = DefaultWidths
	}
	if opts.CRF <= 0 {
		opts.CRF = DefaultCRF
	}
	if opts.Preset == "" {
		opts.Preset = DefaultPreset
	}
	if opts.PosterWidth == 0 {
		opts.PosterWidth = DefaultPosterWidth
	}
	if opts.PosterQuality <= 0 {
		opts.PosterQuality = DefaultPosterQuality
	}

	source, err := probe(ctx, path)
	if err != nil {
		return nil, err
	}

	work, err := os.MkdirTemp(opts.WorkDir, "video-")
	if err != nil {
		return nil, fmt.Errorf("video: work directory: %w", err)
	}
	defer os.RemoveAll(work)

	var ladder []Rendition
	for _, width := range widthsFor(source.Width, opts.Widths) {
		rendition, err := encode(ctx, path, filepath.Join(work, fmt.Sprintf("w%d.mp4", width)), width, source, opts)
		if err != nil {
			return nil, err
		}
		ladder = append(ladder, rendition)
	}

	if opts.PosterWidth > 0 {
		poster, err := still(ctx, path, work, source, opts)
		if err != nil {
			return nil, err
		}
		ladder = append(ladder, poster)
	}
	return ladder, nil
}

// Dimensions returns the pixel size of a video's first video stream.
func Dimensions(ctx context.Context, path string) (int, int, error) {
	probed, err := probe(ctx, path)
	if err != nil {
		return 0, 0, err
	}
	return probed.Width, probed.Height, nil
}

// widthsFor picks the widths to encode at, ascending. An even width, because
// H.264 in yuv420p cannot have an odd one.
func widthsFor(sourceWidth int, configured []int) []int {
	var widths []int
	for _, width := range configured {
		if width <= sourceWidth {
			widths = append(widths, width)
		}
	}
	if len(widths) == 0 && sourceWidth > 1 {
		// Narrower than anything configured. Encode it at its own size rather
		// than serve a camera's file: the saving here is the codec and the
		// bitrate, not the number of pixels.
		widths = []int{sourceWidth - sourceWidth%2}
	}
	sort.Ints(widths)
	return widths
}

// source is what probing a file tells us about it.
type source struct {
	Width    int
	Height   int
	Duration float64
}

func probe(ctx context.Context, path string) (source, error) {
	out, err := run(ctx, "ffprobe", "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-show_entries", "format=duration",
		"-of", "json", path)
	if err != nil {
		return source{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}

	var probed struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probed); err != nil {
		return source{}, fmt.Errorf("%w: unreadable probe output: %v", ErrUnsupported, err)
	}
	if len(probed.Streams) == 0 || probed.Streams[0].Width < 1 || probed.Streams[0].Height < 1 {
		return source{}, fmt.Errorf("%w: no video stream", ErrUnsupported)
	}

	duration, _ := strconv.ParseFloat(probed.Format.Duration, 64)
	return source{
		Width:    probed.Streams[0].Width,
		Height:   probed.Streams[0].Height,
		Duration: duration,
	}, nil
}

func encode(ctx context.Context, in, out string, width int, src source, opts Options) (Rendition, error) {
	// -2 rather than -1 on the height: H.264 in yuv420p needs even
	// dimensions, and an odd one is a hard failure rather than a rounding.
	_, err := run(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-i", in,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", fmt.Sprintf("scale=%d:-2:flags=lanczos", width),
		"-c:v", "libx264", "-crf", strconv.Itoa(opts.CRF), "-preset", opts.Preset,
		"-profile:v", "high", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "128k",
		// faststart moves the index to the front so a browser can start
		// playing before it has the whole file.
		"-movflags", "+faststart",
		// One thread. This usually runs beside other services on a small
		// machine, and an encoder will take every core it is offered.
		"-threads", "1",
		out)
	if err != nil {
		return Rendition{}, fmt.Errorf("video: encode w%d: %w", width, err)
	}

	bytes, err := os.ReadFile(out)
	if err != nil {
		return Rendition{}, fmt.Errorf("video: read w%d: %w", width, err)
	}

	height := src.Height * width / src.Width
	if height%2 == 1 {
		height--
	}
	if height < 2 {
		height = 2
	}
	return Rendition{
		Name:        fmt.Sprintf("w%d", width),
		Width:       width,
		Height:      height,
		ContentType: OutputContentType,
		Extension:   OutputExtension,
		Bytes:       bytes,
	}, nil
}

// still extracts one frame and encodes it with the same code that produces
// image ladders, so a poster is a WebP like every other derived image and this
// package needs nothing from ffmpeg's own image encoders.
func still(ctx context.Context, in, work string, src source, opts Options) (Rendition, error) {
	offset := posterOffsetSeconds
	if src.Duration > 0 && src.Duration < posterOffsetSeconds*2 {
		offset = src.Duration / 2
	}

	frame := filepath.Join(work, "poster.png")
	if _, err := run(ctx, "ffmpeg", "-nostdin", "-v", "error", "-y",
		"-ss", strconv.FormatFloat(offset, 'f', 3, 64),
		"-i", in, "-frames:v", "1", "-c:v", "png", "-threads", "1", frame); err != nil {
		return Rendition{}, fmt.Errorf("video: poster frame: %w", err)
	}

	raw, err := os.ReadFile(frame)
	if err != nil {
		return Rendition{}, fmt.Errorf("video: read poster frame: %w", err)
	}

	rendition, err := imaging.Still(raw, PosterName, opts.PosterWidth, opts.PosterQuality)
	if err != nil {
		return Rendition{}, fmt.Errorf("video: encode poster: %w", err)
	}
	return Rendition{
		Name:        rendition.Name,
		Width:       rendition.Width,
		Height:      rendition.Height,
		ContentType: imaging.OutputContentType,
		Extension:   imaging.OutputExtension,
		Bytes:       rendition.Bytes,
	}, nil
}

// run executes a tool and returns its standard output, folding whatever it
// wrote to stderr into the error so a failure says why rather than "exit 1".
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr, stdout []byte
	stderrPipe := &captured{}
	cmd.Stderr = stderrPipe
	out, err := cmd.Output()
	stdout, stderr = out, stderrPipe.buf
	if err != nil {
		message := string(stderr)
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s: %s", name, message)
	}
	return stdout, nil
}

// captured collects a bounded amount of a tool's stderr. Bounded because a
// broken input can make ffmpeg complain once per frame, and an error message
// should fit in a log line rather than in memory.
type captured struct{ buf []byte }

const maxCaptured = 4 << 10

func (c *captured) Write(p []byte) (int, error) {
	if room := maxCaptured - len(c.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}
