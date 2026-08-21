package video

import (
	"bytes"
	"context"
	"errors"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "golang.org/x/image/webp"
)

// testVideo writes a short synthetic clip. It is generated rather than
// committed because a binary fixture in a repository is a thing nobody can
// read, review, or regenerate.
func testVideo(t *testing.T, width, height int, seconds string) string {
	t.Helper()
	requireFFmpeg(t)

	path := filepath.Join(t.TempDir(), "source.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size="+itoa(width)+"x"+itoa(height)+":rate=15",
		"-t", seconds, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make test video: %v: %s", err, out)
	}
	return path
}

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if !Available() {
		t.Skip("ffmpeg is not installed")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

func TestLadderEncodesEveryWidthSmallerThanTheSource(t *testing.T) {
	source := testVideo(t, 1280, 720, "2")

	ladder, err := Ladder(context.Background(), source, Options{
		Widths: []int{640, 960, 1920},
		Preset: "ultrafast",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 640 and 960 are narrower than the source; 1920 is not. Plus a poster.
	if len(ladder) != 3 {
		t.Fatalf("got %d renditions, want 3", len(ladder))
	}
	for i, want := range []int{640, 960} {
		got := ladder[i]
		if got.Name != "w"+itoa(want) || got.Width != want {
			t.Errorf("rendition %d is %q at %dpx, want w%d", i, got.Name, got.Width, want)
		}
		if got.ContentType != OutputContentType {
			t.Errorf("rendition %d is %s, want %s", i, got.ContentType, OutputContentType)
		}
		// 16:9 in, 16:9 out.
		if got.Height != want*9/16 {
			t.Errorf("rendition %d is %dx%d, which is not the source's shape", i, got.Width, got.Height)
		}
		if len(got.Bytes) == 0 {
			t.Errorf("rendition %d is empty", i)
		}
	}
}

func TestEncodesArePlayableAndSmallerThanTheSource(t *testing.T) {
	source := testVideo(t, 1280, 720, "2")
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	ladder, err := Ladder(context.Background(), source, Options{
		Widths: []int{640},
		Preset: "ultrafast",
	})
	if err != nil {
		t.Fatal(err)
	}

	encoded := filepath.Join(t.TempDir(), "w640.mp4")
	if err := os.WriteFile(encoded, ladder[0].Bytes, 0o600); err != nil {
		t.Fatal(err)
	}

	probed, err := probe(context.Background(), encoded)
	if err != nil {
		t.Fatalf("the encode is not readable: %v", err)
	}
	if probed.Width != 640 || probed.Height != 360 {
		t.Errorf("the encode is %dx%d, want 640x360", probed.Width, probed.Height)
	}
	if len(ladder[0].Bytes) >= len(original) {
		t.Errorf("the encode is %d bytes and the source is %d; shrinking it is the point",
			len(ladder[0].Bytes), len(original))
	}
}

func TestPosterIsAWebPImageOfTheFrame(t *testing.T) {
	source := testVideo(t, 1280, 720, "2")

	ladder, err := Ladder(context.Background(), source, Options{
		Widths:      []int{640},
		Preset:      "ultrafast",
		PosterWidth: 320,
	})
	if err != nil {
		t.Fatal(err)
	}

	poster := ladder[len(ladder)-1]
	if poster.Name != PosterName {
		t.Fatalf("last rendition is %q, want the poster", poster.Name)
	}
	if poster.ContentType != "image/webp" {
		t.Errorf("poster is %s, want image/webp", poster.ContentType)
	}

	decoded, format, err := image.Decode(bytes.NewReader(poster.Bytes))
	if err != nil {
		t.Fatalf("poster does not decode: %v", err)
	}
	if format != "webp" {
		t.Errorf("poster decoded as %s", format)
	}
	if got := decoded.Bounds().Dx(); got != 320 {
		t.Errorf("poster is %dpx wide, want 320", got)
	}
}

func TestAVideoAlreadySmallEnoughStillGetsAPoster(t *testing.T) {
	source := testVideo(t, 320, 240, "1")

	ladder, err := Ladder(context.Background(), source, Options{
		Widths: []int{960, 1920},
		Preset: "ultrafast",
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(ladder) != 1 || ladder[0].Name != PosterName {
		t.Fatalf("got %d renditions, want only the poster", len(ladder))
	}
}

func TestBytesThatAreNotAVideoAreNotWorthRetrying(t *testing.T) {
	requireFFmpeg(t)

	path := filepath.Join(t.TempDir(), "not-a-video.mp4")
	if err := os.WriteFile(path, []byte("this is text"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Ladder(context.Background(), path, Options{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got %v, want ErrUnsupported", err)
	}
}

func TestSupportedNamesTheContainersFFmpegReads(t *testing.T) {
	requireFFmpeg(t)

	for _, contentType := range []string{"video/mp4", "video/quicktime", "video/webm"} {
		if !Supported(contentType) {
			t.Errorf("%s should be supported", contentType)
		}
	}
	for _, contentType := range []string{"image/png", "application/pdf", "text/plain"} {
		if Supported(contentType) {
			t.Errorf("%s should not be supported", contentType)
		}
	}
}
