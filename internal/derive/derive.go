// Package derive is the one place that answers "what smaller forms does this
// asset have, and how are they made".
//
// It exists so that the two callers who need that answer -- the upload path,
// deciding whether to queue work, and the worker, doing it -- cannot disagree.
// Adding a kind of asset means adding a backend here and nothing else.
//
// It takes a path rather than bytes: video cannot be transcoded in memory, and
// a file is also the right shape for an image large enough to be worth not
// holding twice.
package derive

import (
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"

	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/video"
	_ "golang.org/x/image/webp"
)

// ErrUnsupported means the bytes are not what their content type claims, so
// there is nothing to come back for. Anything else is worth a retry.
var ErrUnsupported = errors.New("derive: cannot read this asset")

// Rendition is one derived form of an asset.
type Rendition struct {
	// Name is how the ladder refers to this rung: "w640" for a size, "poster"
	// for a video's still.
	Name        string
	Width       int
	Height      int
	ContentType string
	Extension   string
	Bytes       []byte
}

// Options carries each backend's settings. The zero value means defaults.
type Options struct {
	Image imaging.Options
	Video video.Options
}

// Supported reports whether an asset of this content type has derived forms.
func Supported(contentType string) bool {
	return imaging.Supported(contentType) || video.Supported(contentType)
}

// Ladder produces every derived form of the asset at path, smallest first. An
// asset that is already small enough produces nothing, which is not an error.
func Ladder(ctx context.Context, path, contentType string, opts Options) ([]Rendition, error) {
	switch {
	case imaging.Supported(contentType):
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("derive: read %s: %w", path, err)
		}
		ladder, err := imaging.Ladder(src, opts.Image)
		if err != nil {
			if errors.Is(err, imaging.ErrUnsupported) {
				return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
			}
			return nil, err
		}
		out := make([]Rendition, 0, len(ladder))
		for _, r := range ladder {
			out = append(out, Rendition(r))
		}
		return out, nil

	case video.Supported(contentType):
		ladder, err := video.Ladder(ctx, path, opts.Video)
		if err != nil {
			if errors.Is(err, video.ErrUnsupported) {
				return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
			}
			return nil, err
		}
		out := make([]Rendition, 0, len(ladder))
		for _, r := range ladder {
			out = append(out, Rendition(r))
		}
		return out, nil

	default:
		return nil, nil
	}
}

// Dimensions measures the pixel size of the asset at path. It reads only what
// it needs to: an image's header, or one probe of a video's first stream.
//
// A kind of asset with no dimensions -- a PDF, an STL, a zip -- measures zero
// by zero and no error. Not being able to measure something is not a reason to
// refuse to store it.
func Dimensions(ctx context.Context, path, contentType string) (int, int, error) {
	switch {
	case imaging.Supported(contentType):
		file, err := os.Open(path)
		if err != nil {
			return 0, 0, fmt.Errorf("derive: open %s: %w", path, err)
		}
		defer file.Close()

		config, _, err := image.DecodeConfig(file)
		if err != nil {
			return 0, 0, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return config.Width, config.Height, nil

	case video.Supported(contentType):
		return video.Dimensions(ctx, path)

	default:
		return 0, 0, nil
	}
}
