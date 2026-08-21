// Package model turns one uploaded 3D model into the forms a page and a
// pipeline actually want: a rendered picture of it, and the slicer's own
// answer to what printing it costs.
//
// The render is pure Go -- triangles in, shaded pixels out -- so it works
// anywhere this binary runs. Slicing is not: there is no Go slicer worth
// having, so it drives OrcaSlicer's CLI the same way video drives ffmpeg, and
// the numbers reported are the slicer's own, not an estimate. Grams depend on
// one boolean anybody may disagree about -- supports -- so the ladder carries
// both answers, sliced with supports off and on, and a consumer picks the one
// it means rather than asking for a parameter.
//
// Unlike video, Supported does not ask whether the tool is installed. The
// machine that stores an upload is deliberately not the machine that derives
// from it, so "may this be queued" must not depend on what is on THIS box.
// Where OrcaSlicer is absent, Ladder fails and the job stays visible in the
// queue for a worker that has it.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"os/exec"
	"runtime"
)

// ErrUnsupported means the bytes are not a model this package can read.
var ErrUnsupported = errors.New("model: unsupported model")

// RenderName is the picture; SliceName and SliceSupportName are the slicer's
// numbers with supports off and on. A client tells them apart by name and by
// content type: the render is the only image.
const (
	RenderName       = "render"
	SliceName        = "slice"
	SliceSupportName = "slice-support"
)

// RenderSize is the square canvas the render is drawn on. One size: this is a
// picture of a part, not a photograph with a ladder of its own.
const RenderSize = 1024

func init() {
	// STL is not in Go's built-in table, and an .stl upload usually arrives as
	// application/octet-stream. Registering it here means the upload path's
	// extension fallback resolves the real type wherever this package is
	// linked in.
	_ = mime.AddExtensionType(".stl", "model/stl")
}

// Rendition is one derived form: the render, or a slice report.
type Rendition struct {
	Name        string
	Width       int
	Height      int
	ContentType string
	Extension   string
	Bytes       []byte
}

// Options says where OrcaSlicer is. The zero value can render but not slice.
type Options struct {
	// Bin is the OrcaSlicer executable; Profiles is its bundled BBL profile
	// directory, the one holding machine/, process/ and filament/.
	Bin      string
	Profiles string
}

// OptionsFromEnv reads ORCA_BIN and ORCA_PROFILES -- the same two variables
// every other driver of this slicer uses -- falling back to the standard
// install location on a Mac. On Linux the AppImage extracts to no standard
// place, so the environment has to say.
func OptionsFromEnv() Options {
	o := Options{Bin: os.Getenv("ORCA_BIN"), Profiles: os.Getenv("ORCA_PROFILES")}
	if o.Bin == "" && runtime.GOOS == "darwin" {
		o.Bin = "/Applications/OrcaSlicer.app/Contents/MacOS/OrcaSlicer"
	}
	if o.Profiles == "" && runtime.GOOS == "darwin" {
		o.Profiles = "/Applications/OrcaSlicer.app/Contents/Resources/profiles/BBL"
	}
	return o
}

// Available reports whether these options can actually slice.
func Available(o Options) bool {
	if o.Bin == "" || o.Profiles == "" {
		return false
	}
	if _, err := exec.LookPath(o.Bin); err != nil {
		return false
	}
	info, err := os.Stat(o.Profiles)
	return err == nil && info.IsDir()
}

// Supported reports whether an asset of this content type has derived forms.
// Deliberately a question about the type alone; see the package comment.
func Supported(contentType string) bool {
	switch contentType {
	case "model/stl", "application/sla", "application/vnd.ms-pki.stl",
		"model/x.stl-binary", "model/x.stl-ascii":
		return true
	default:
		return false
	}
}

// Ladder reads the model at path and returns its derived forms: the render
// first, then the two slice reports. Without a slicer it returns an error
// rather than a partial ladder, so the job stays queued for a worker that has
// one instead of completing with the numbers missing.
func Ladder(ctx context.Context, path string, opts Options) ([]Rendition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("model: read %s: %w", path, err)
	}
	m, err := parseSTL(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}

	pngBytes, err := renderPNG(m, RenderSize)
	if err != nil {
		return nil, fmt.Errorf("model: render: %w", err)
	}
	out := []Rendition{{
		Name:        RenderName,
		Width:       RenderSize,
		Height:      RenderSize,
		ContentType: "image/png",
		Extension:   ".png",
		Bytes:       pngBytes,
	}}

	if !Available(opts) {
		return nil, fmt.Errorf("model: OrcaSlicer is not available on this machine (set ORCA_BIN and ORCA_PROFILES)")
	}

	work, err := os.MkdirTemp("", "model-slice-*")
	if err != nil {
		return nil, fmt.Errorf("model: workdir: %w", err)
	}
	defer os.RemoveAll(work)

	profiles, err := buildProfiles(opts.Profiles, work)
	if err != nil {
		return nil, fmt.Errorf("model: profiles: %w", err)
	}
	prepared, err := prepareMesh(m, work)
	if err != nil {
		return nil, fmt.Errorf("model: prepare mesh: %w", err)
	}

	// A variant that fails to slice is omitted rather than failing the job:
	// the slicer makes floating regions fatal with supports off, so some
	// geometry only has a support-on answer, and that answer is still worth
	// having. Both failing means the model itself cannot be sliced.
	var sliceErrs []error
	for _, support := range []bool{false, true} {
		metrics, err := slice(ctx, opts, profiles, prepared, support)
		if err != nil {
			sliceErrs = append(sliceErrs, fmt.Errorf("support=%v: %w", support, err))
			continue
		}
		report, err := json.MarshalIndent(metrics, "", " ")
		if err != nil {
			return nil, fmt.Errorf("model: report: %w", err)
		}
		name := SliceName
		if support {
			name = SliceSupportName
		}
		out = append(out, Rendition{
			Name:        name,
			ContentType: "application/json",
			Extension:   ".json",
			Bytes:       report,
		})
	}
	if len(sliceErrs) == 2 {
		return nil, fmt.Errorf("model: slice: %w; %v", sliceErrs[0], sliceErrs[1])
	}
	return out, nil
}
