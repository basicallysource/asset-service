package derive

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/video"
)

func imageFile(t *testing.T, width, height int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func videoFile(t *testing.T) string {
	t.Helper()
	if !video.Available() {
		t.Skip("ffmpeg is not installed")
	}

	path := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=15", "-t", "1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test video: %v: %s", err, out)
	}
	return path
}

func TestAnImageBecomesWebP(t *testing.T) {
	ladder, err := Ladder(context.Background(), imageFile(t, 1200, 900), "image/png",
		Options{Image: imaging.Options{Widths: []int{320, 640}}})
	if err != nil {
		t.Fatal(err)
	}

	if len(ladder) != 2 {
		t.Fatalf("got %d renditions, want 2", len(ladder))
	}
	for _, r := range ladder {
		if r.ContentType != imaging.JPEGContentType || r.Extension != imaging.JPEGExtension {
			t.Errorf("%s is %s%s, want %s%s", r.Name, r.ContentType, r.Extension,
				imaging.JPEGContentType, imaging.JPEGExtension)
		}
	}
}

func TestAVideoBecomesEncodesAndAPoster(t *testing.T) {
	ladder, err := Ladder(context.Background(), videoFile(t), "video/mp4",
		Options{Video: video.Options{Widths: []int{640}, Preset: "ultrafast", PosterWidth: 320}})
	if err != nil {
		t.Fatal(err)
	}

	if len(ladder) != 2 {
		t.Fatalf("got %d renditions, want an encode and a poster", len(ladder))
	}
	if ladder[0].ContentType != video.OutputContentType {
		t.Errorf("the encode is %s, want %s", ladder[0].ContentType, video.OutputContentType)
	}
	// A caller tells the poster from the encodes by content type, not by name,
	// which is what the API documents.
	if ladder[1].ContentType != imaging.JPEGContentType {
		t.Errorf("the poster is %s, want %s", ladder[1].ContentType, imaging.JPEGContentType)
	}
}

func TestAnAssetWithNoDerivedFormsProducesNothing(t *testing.T) {
	if Supported("application/pdf") {
		t.Error("a PDF has no derived forms")
	}

	ladder, err := Ladder(context.Background(), imageFile(t, 40, 40), "application/pdf", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if ladder != nil {
		t.Errorf("got %d renditions, want none", len(ladder))
	}
}

func TestBytesThatLieAboutTheirTypeAreNotWorthRetrying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims-to-be-a-png.png")
	if err := os.WriteFile(path, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Ladder(context.Background(), path, "image/png", Options{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

// phonePhoto writes the shape a phone actually hands over: landscape pixels,
// and an EXIF APP1 segment whose orientation tag says to stand them up. The
// tag reader itself is tested in internal/imaging; this is about what the
// measurement path does with what it says.
func phonePhoto(t *testing.T, width, height int) string {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	jpg := buf.Bytes()

	// A TIFF block, big-endian, holding one IFD0 entry: tag 0x0112, a SHORT,
	// value 6 -- a quarter turn clockwise.
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segment := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)

	// In front of everything but the SOI marker, which is where a camera's is.
	withEXIF := append([]byte{}, jpg[:2]...)
	withEXIF = append(withEXIF, segment...)
	withEXIF = append(withEXIF, jpg[2:]...)

	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, withEXIF, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnImageIsMeasuredInTheShapeItIsLookedAtIn(t *testing.T) {
	width, height, err := Dimensions(context.Background(), phonePhoto(t, 1200, 900), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	// Stored 1200x900; the tag says it is a 900x1200 picture, and that is the
	// box a page has to reserve for it.
	if width != 900 || height != 1200 {
		t.Errorf("measured %dx%d, want 900x1200", width, height)
	}

	// An image with nothing to say about orientation measures as it is stored.
	width, height, err = Dimensions(context.Background(), imageFile(t, 1200, 900), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if width != 1200 || height != 900 {
		t.Errorf("measured %dx%d, want 1200x900", width, height)
	}
}
