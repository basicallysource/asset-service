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
	"image/jpeg"
	"image/png"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// The output formats, and why there are two.
//
// JPEG for photographs, because a picture somebody saves off a page should
// open in whatever they open it with. WebP is roughly a quarter smaller and
// the difference is real, but it is a file that a good number of tools still
// refuse, and a page's images are things people take away.
//
// PNG for anything with transparency, because JPEG has none: flattening a
// render onto white is a visible wrong answer on a page with a dark mode, and
// guessing the background is worse than the bytes.
const (
	JPEGContentType = "image/jpeg"
	JPEGExtension   = ".jpg"
	PNGContentType  = "image/png"
	PNGExtension    = ".png"
)

// ErrUnsupported means the bytes are not an image this package can read.
var ErrUnsupported = errors.New("imaging: unsupported image")

// Rendition is one derived image.
type Rendition struct {
	// Name is how the ladder refers to this rung, e.g. "w640".
	Name        string
	Width       int
	Height      int
	ContentType string
	Extension   string
	Bytes       []byte
}

// Options controls what a ladder contains.
type Options struct {
	// Widths to produce. A width at or above the original's is skipped:
	// upscaling invents detail and costs bytes to do it.
	Widths []int
	// Quality is the JPEG quality, 1-100. It does not apply to renditions kept
	// as PNG, which are lossless.
	Quality int
}

// DefaultWidths covers a phone through a retina desktop. The largest is well
// under what a camera produces, because the full-size original is always
// available under its own URL for anyone who actually wants it.
var DefaultWidths = []int{320, 640, 1024, 1600, 2048}

// DefaultQuality is where JPEG stops being visibly lossy for photographs.
const DefaultQuality = 82

// Supported reports whether a content type is worth handing to Ladder.
func Supported(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/jpg", "image/png", "image/webp":
		return true
	default:
		// GIF decodes, but only its first frame, which would silently turn an
		// animation into a still. Better to leave it alone.
		return false
	}
}

// Publishable reports whether an image of this content type has a copy that
// can be published in place of the bytes that were uploaded -- one this
// package can strip the camera's own notes out of, byte for byte.
func Publishable(contentType string) bool {
	switch contentType {
	case "image/jpeg", "image/jpg", "image/png":
		return true
	default:
		// A WebP or a GIF is left as it was uploaded: nothing that arrives
		// here comes off a camera in one. See Full.
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

	decoded, err := decode(src)
	if err != nil {
		return nil, err
	}
	bounds := decoded.Bounds()

	var ladder []Rendition
	for _, width := range opts.Widths {
		if width >= bounds.Dx() {
			continue
		}
		rendition, err := render(decoded, width, opts.Quality)
		if err != nil {
			return nil, err
		}
		ladder = append(ladder, rendition)
	}
	return ladder, nil
}

// Still encodes one image under a given name, shrinking it to at most width.
// Unlike Ladder it always produces something: it is for a single derived image
// -- a video's poster frame -- where "the original is already the right size"
// means encode it as it is, not skip it.
func Still(src []byte, name string, width, quality int) (Rendition, error) {
	if quality <= 0 || quality > 100 {
		quality = DefaultQuality
	}

	decoded, err := decode(src)
	if err != nil {
		return Rendition{}, err
	}
	if bounds := decoded.Bounds(); width <= 0 || width > bounds.Dx() {
		width = bounds.Dx()
	}

	rendition, err := render(decoded, width, quality)
	if err != nil {
		return Rendition{}, err
	}
	rendition.Name = name
	return rendition, nil
}

func decode(src []byte) (image.Image, error) {
	decoded, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() < 1 || bounds.Dy() < 1 {
		return nil, fmt.Errorf("%w: zero-sized image", ErrUnsupported)
	}
	// A phone stores the sensor's own frame and an EXIF tag saying which way
	// up it was held; the standard library's decoders ignore the tag entirely.
	// Bake it in here, before anything is scaled, so every rung comes out
	// upright and nothing downstream has to know the tag exists.
	return orient(decoded, orientation(src)), nil
}

// render resizes to width and encodes, preserving the aspect ratio and
// choosing the format by whether the pixels need one.
func render(decoded image.Image, width, quality int) (Rendition, error) {
	bounds := decoded.Bounds()
	height := bounds.Dy() * width / bounds.Dx()
	if height < 1 {
		height = 1
	}

	// CatmullRom because these are photographs being shrunk a long way, where
	// a cheaper filter shows it. Src rather than Over: this is a resample into
	// an empty buffer, and compositing over transparent black darkens the edge
	// of anything that has an alpha channel.
	resized := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(resized, resized.Bounds(), decoded, bounds, draw.Src, nil)

	rendition := Rendition{
		Name:        fmt.Sprintf("w%d", width),
		Width:       width,
		Height:      height,
		ContentType: JPEGContentType,
		Extension:   JPEGExtension,
	}

	var out bytes.Buffer
	if transparent(decoded) {
		rendition.ContentType, rendition.Extension = PNGContentType, PNGExtension
		if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, resized); err != nil {
			return Rendition{}, fmt.Errorf("imaging: encode w%d: %w", width, err)
		}
	} else if err := jpeg.Encode(&out, resized, &jpeg.Options{Quality: quality}); err != nil {
		return Rendition{}, fmt.Errorf("imaging: encode w%d: %w", width, err)
	}

	rendition.Bytes = out.Bytes()
	return rendition, nil
}

// transparent reports whether any pixel is not fully opaque.
//
// It asks the pixels rather than the format: a PNG saved with an alpha channel
// it never uses is the common case, and there is no reason to keep that one as
// a PNG. Opaque() answers immediately for image types that cannot have alpha
// at all, so this costs nothing for a JPEG.
func transparent(img image.Image) bool {
	if o, ok := img.(interface{ Opaque() bool }); ok {
		return !o.Opaque()
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return true
			}
		}
	}
	return false
}
