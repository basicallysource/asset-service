package imaging

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// testImage is a plausible photograph: smooth, so the encoder behaves the way
// it would on a real one.
func testImage(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{
				R: uint8(255 * x / max(width, 1)),
				G: uint8(255 * y / max(height, 1)),
				B: 128,
				A: 255,
			})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLadderProducesEveryWidthSmallerThanTheOriginal(t *testing.T) {
	ladder, err := Ladder(testImage(t, 1200, 900), Options{Widths: []int{320, 640, 1024, 1600}})
	if err != nil {
		t.Fatal(err)
	}

	if len(ladder) != 3 {
		t.Fatalf("got %d renditions, want 3 (1600 is wider than the original)", len(ladder))
	}
	for i, want := range []int{320, 640, 1024} {
		got := ladder[i]
		if got.Width != want {
			t.Errorf("rendition %d is %dpx, want %d", i, got.Width, want)
		}
		if got.Name != "w"+itoa(want) {
			t.Errorf("rendition %d is named %q", i, got.Name)
		}
		// 4:3 in, 4:3 out.
		if got.Height != want*3/4 {
			t.Errorf("rendition %d is %dx%d, which is not the original's shape", i, got.Width, got.Height)
		}
	}
}

func TestRenditionsAreRealImagesOfTheStatedSize(t *testing.T) {
	ladder, err := Ladder(testImage(t, 1000, 500), Options{Widths: []int{400}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 1 {
		t.Fatalf("got %d renditions", len(ladder))
	}

	cfg, format, err := image.DecodeConfig(bytes.NewReader(ladder[0].Bytes))
	if err != nil {
		t.Fatalf("rendition does not decode: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("format = %q, want jpeg", format)
	}
	if cfg.Width != 400 || cfg.Height != 200 {
		t.Errorf("decoded %dx%d, want 400x200", cfg.Width, cfg.Height)
	}
}

func TestLadderNeverUpscales(t *testing.T) {
	ladder, err := Ladder(testImage(t, 200, 150), Options{Widths: DefaultWidths})
	if err != nil {
		t.Fatal(err)
	}
	if len(ladder) != 0 {
		t.Errorf("an image smaller than every width produced %d renditions", len(ladder))
	}
}

func TestLadderRejectsWhatIsNotAnImage(t *testing.T) {
	_, err := Ladder([]byte("this is not an image, it is a sentence"), Options{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestSupported(t *testing.T) {
	for _, ct := range []string{"image/jpeg", "image/png", "image/webp"} {
		if !Supported(ct) {
			t.Errorf("Supported(%q) = false", ct)
		}
	}
	// A GIF decodes to its first frame only, which would turn an animation
	// into a still; an STL or a video has no ladder at all.
	for _, ct := range []string{"image/gif", "video/mp4", "model/stl", "application/pdf", ""} {
		if Supported(ct) {
			t.Errorf("Supported(%q) = true", ct)
		}
	}
}

func TestSmallerRenditionsAreSmallerFiles(t *testing.T) {
	ladder, err := Ladder(testImage(t, 1600, 1200), Options{Widths: []int{320, 640, 1024}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(ladder); i++ {
		if len(ladder[i].Bytes) <= len(ladder[i-1].Bytes) {
			t.Errorf("%s (%d bytes) is not larger than %s (%d bytes)",
				ladder[i].Name, len(ladder[i].Bytes), ladder[i-1].Name, len(ladder[i-1].Bytes))
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// transparentImage has a pixel that is not opaque, which is the only thing
// that should make a rendition keep an alpha channel.
func transparentImage(t *testing.T, width, height int) []byte {
	t.Helper()

	// A transparent left half, so that shrinking it cannot blend the
	// transparency away into its opaque neighbours.
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			if x < width/2 {
				continue
			}
			img.Set(x, y, color.RGBA{R: 200, G: 120, B: 60, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAPhotographBecomesJPEG(t *testing.T) {
	ladder, err := Ladder(testImage(t, 1200, 900), Options{Widths: []int{640}})
	if err != nil {
		t.Fatal(err)
	}
	if ladder[0].ContentType != JPEGContentType || ladder[0].Extension != JPEGExtension {
		t.Errorf("got %s%s, want %s%s", ladder[0].ContentType, ladder[0].Extension,
			JPEGContentType, JPEGExtension)
	}
}

func TestTransparencyIsKeptRatherThanFlattened(t *testing.T) {
	ladder, err := Ladder(transparentImage(t, 1200, 900), Options{Widths: []int{640}})
	if err != nil {
		t.Fatal(err)
	}
	if ladder[0].ContentType != PNGContentType || ladder[0].Extension != PNGExtension {
		t.Fatalf("got %s, want %s -- flattening onto a guessed background is a visible wrong answer",
			ladder[0].ContentType, PNGContentType)
	}

	decoded, format, err := image.Decode(bytes.NewReader(ladder[0].Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if format != "png" {
		t.Errorf("decoded as %s", format)
	}
	if _, _, _, alpha := decoded.At(0, 0).RGBA(); alpha != 0 {
		t.Errorf("the transparent corner came back with alpha %d", alpha)
	}
}

func TestAPNGThatNeverUsesItsAlphaIsStillAJPEG(t *testing.T) {
	// The common case: a PNG saved with an alpha channel and nothing in it.
	// There is no reason to pay PNG's size for that.
	ladder, err := Ladder(testImage(t, 1200, 900), Options{Widths: []int{640}})
	if err != nil {
		t.Fatal(err)
	}
	if ladder[0].ContentType != JPEGContentType {
		t.Errorf("got %s, want %s", ladder[0].ContentType, JPEGContentType)
	}
}
