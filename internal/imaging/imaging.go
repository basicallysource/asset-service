// Package imaging turns one uploaded image into the set of smaller ones a page
// should actually download.
//
// It is pure: bytes in, bytes out, no storage and no database. That is what
// makes the expensive part of this service testable without any of the rest of
// it, and what keeps the worker that calls it down to bookkeeping.
package imaging

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/gen2brain/webp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// WebP is the one output format. Every browser in use supports it, it is
// roughly a third smaller than JPEG at the same quality, and one format means
// the ladder is a list of sizes rather than a matrix of sizes and formats.
const (
	OutputContentType = "image/webp"
	OutputExtension   = ".webp"
)

// ErrUnsupported means the bytes are not an image this package can read.
var ErrUnsupported = errors.New("imaging: unsupported image")

// Rendition is one derived image.
type Rendition struct {
	// Name is how the ladder refers to this rung, e.g. "w640".
	Name   string
	Width  int
	Height int
	Bytes  []byte
}

// Options controls what a ladder contains.
type Options struct {
	// Widths to produce. A width at or above the original's is skipped:
	// upscaling invents detail and costs bytes to do it.
	Widths []int
	// Quality is the WebP quality, 1-100.
	Quality int
}

// DefaultWidths covers a phone through a retina desktop. The largest is well
// under what a camera produces, because the full-size original is always
// available under its own URL for anyone who actually wants it.
var DefaultWidths = []int{320, 640, 1024, 1600, 2048}

// DefaultQuality is where WebP stops being visibly lossy for photographs.
const DefaultQuality = 80

// Supported reports whether a content type is worth handing to Ladder.
func Supported(contentType string) bool {
	switch {
	case contentType == "image/jpeg", contentType == "image/jpg",
		contentType == "image/png", contentType == "image/webp":
		return true
	default:
		// GIF decodes, but only its first frame, which would silently turn an
		// animation into a still. Better to leave it alone.
		return false
	}
}

// Ladder decodes src once and returns a rendition per usable width, smallest
// first. An image smaller than every width produces nothing, which is not an
// error -- it means the original is already the right size.
func Ladder(src []byte, opts Options) ([]Rendition, error) {
	if len(opts.Widths) == 0 {
		opts.Widths = DefaultWidths
	}
	if opts.Quality <= 0 || opts.Quality > 100 {
		opts.Quality = DefaultQuality
	}

	decoded, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	bounds := decoded.Bounds()
	if bounds.Dx() < 1 || bounds.Dy() < 1 {
		return nil, fmt.Errorf("%w: zero-sized image", ErrUnsupported)
	}

	var ladder []Rendition
	for _, width := range opts.Widths {
		if width >= bounds.Dx() {
			continue
		}
		height := bounds.Dy() * width / bounds.Dx()
		if height < 1 {
			height = 1
		}

		// CatmullRom because these are photographs being shrunk a long way,
		// where a cheaper filter shows it.
		resized := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(resized, resized.Bounds(), decoded, bounds, draw.Over, nil)

		var out bytes.Buffer
		if err := webp.Encode(&out, resized, webp.Options{Quality: opts.Quality}); err != nil {
			return nil, fmt.Errorf("imaging: encode w%d: %w", width, err)
		}
		ladder = append(ladder, Rendition{
			Name:   fmt.Sprintf("w%d", width),
			Width:  width,
			Height: height,
			Bytes:  out.Bytes(),
		})
	}
	return ladder, nil
}
