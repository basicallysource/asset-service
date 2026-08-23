package imaging

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"testing"
)

// withSegment splices one marker segment in behind the SOI, which is where a
// camera's own are.
func withSegment(t *testing.T, jpg []byte, marker byte, payload []byte) []byte {
	t.Helper()
	if len(jpg) < 2 || jpg[0] != 0xFF || jpg[1] != 0xD8 {
		t.Fatal("that is not a JPEG")
	}

	segment := []byte{0xFF, marker}
	segment = binary.BigEndian.AppendUint16(segment, uint16(2+len(payload)))
	segment = append(segment, payload...)

	out := append([]byte{}, jpg[:2]...)
	out = append(out, segment...)
	return append(out, jpg[2:]...)
}

// cameraJPEG is what comes off a phone, in the parts that matter here: a
// position and a capture time in EXIF, the same again in XMP, a comment, and
// two things that are about the picture rather than about the photographer --
// an ICC profile and Adobe's colour segment.
func cameraJPEG(t *testing.T, width, height int) []byte {
	t.Helper()

	jpg := jpegOf(t, photograph(t, width, height))
	jpg = withSegment(t, jpg, 0xEE, []byte("Adobe\x00\x64\x00\x00\x00\x00\x00"))
	jpg = withSegment(t, jpg, 0xE2, []byte("ICC_PROFILE\x00\x01\x01keep-my-colours"))
	jpg = withSegment(t, jpg, 0xFE, []byte("photographed by a person, at their house"))
	jpg = withSegment(t, jpg, 0xE1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>51.5074,-0.1278</x:xmpmeta>"))

	// EXIF: the orientation the picture needs, and beside it the things it does
	// not. A real GPS position is a second directory the header points at; what
	// this test asks is that nothing in the segment but the orientation lives
	// through the copy, whatever it was.
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0}
	exif := append([]byte("Exif\x00\x00"), tiff...)
	exif = append(exif, []byte("GPS 51.5074,-0.1278 taken 2026-08-23 by serial 51.5074")...)
	jpg = withSegment(t, jpg, 0xE1, exif)

	// JFIF, which the standard library's encoder does not write but a camera
	// does, and which a decoder expects to find first.
	return withSegment(t, jpg, 0xE0, []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00"))
}

func TestAPublishableCopyKeepsThePixelsAndDropsTheCamera(t *testing.T) {
	src := cameraJPEG(t, 600, 400)

	full, err := Full(src)
	if err != nil {
		t.Fatal(err)
	}
	if full.Name != FullName || full.ContentType != JPEGContentType || full.Extension != JPEGExtension {
		t.Errorf("full copy = %+v", Rendition{Name: full.Name, ContentType: full.ContentType, Extension: full.Extension})
	}
	// The tag says the camera was on its side, so this is 400x600 to look at.
	if full.Width != 400 || full.Height != 600 {
		t.Errorf("full copy is %dx%d, want 400x600", full.Width, full.Height)
	}

	for _, gone := range []string{"51.5074", "xmpmeta", "photographed by a person"} {
		if bytes.Contains(full.Bytes, []byte(gone)) {
			t.Errorf("the published copy still carries %q", gone)
		}
	}
	for _, kept := range []string{"ICC_PROFILE", "keep-my-colours", "Adobe", "JFIF"} {
		if !bytes.Contains(full.Bytes, []byte(kept)) {
			t.Errorf("the published copy lost %q, which is about the picture rather than the person", kept)
		}
	}

	// Which way up it goes is not a secret, and dropping it would put every
	// photograph taken sideways back on its side.
	if o := orientation(full.Bytes); o != 6 {
		t.Errorf("orientation = %d, want 6", o)
	}
}

func TestAPublishableCopyIsTheSamePixelsExactly(t *testing.T) {
	// Byte-level, not a re-encode: every pixel has to come back identical, or
	// publishing costs quality every time an image is uploaded.
	src := cameraJPEG(t, 320, 240)

	full, err := Full(src)
	if err != nil {
		t.Fatal(err)
	}

	before, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	after, _, err := image.Decode(bytes.NewReader(full.Bytes))
	if err != nil {
		t.Fatalf("the published copy does not decode: %v", err)
	}
	if before.Bounds() != after.Bounds() {
		t.Fatalf("bounds %v became %v", before.Bounds(), after.Bounds())
	}
	for y := before.Bounds().Min.Y; y < before.Bounds().Max.Y; y++ {
		for x := before.Bounds().Min.X; x < before.Bounds().Max.X; x++ {
			if before.At(x, y) != after.At(x, y) {
				t.Fatalf("pixel %d,%d changed: %v became %v", x, y, before.At(x, y), after.At(x, y))
			}
		}
	}
	if len(full.Bytes) >= len(src) {
		t.Errorf("the copy is %d bytes and the original %d; nothing was dropped", len(full.Bytes), len(src))
	}
}

func TestAJPEGWithNothingToHideIsLeftAlone(t *testing.T) {
	// The standard library's encoder writes no APPn at all, so there is
	// nothing to take out and the copy is the same file, byte for byte.
	plain := jpegOf(t, photograph(t, 200, 150))

	full, err := Full(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full.Bytes, plain) {
		t.Errorf("a JPEG with no metadata came back changed: %d bytes became %d", len(plain), len(full.Bytes))
	}
}

// chunk builds a PNG chunk: length, name, data, and the CRC over the last two.
func chunk(name string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	body := append([]byte(name), data...)
	out = append(out, body...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}

func TestAPNGLosesItsTextAndTimeAndKeepsItsPixels(t *testing.T) {
	plain := testImage(t, 300, 200)

	// After the signature and the header chunk, which is where a writer puts
	// them. IHDR is always 13 bytes of data.
	const signature, header = 8, 12 + 13
	src := append([]byte{}, plain[:signature+header]...)
	src = append(src, chunk("tEXt", []byte("Author\x00a person"))...)
	src = append(src, chunk("iTXt", []byte("Comment\x00\x00\x00\x00\x00taken at 51.5074,-0.1278"))...)
	src = append(src, chunk("eXIf", []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x00GPS-51.5074"))...)
	src = append(src, chunk("tIME", []byte{0x07, 0xEA, 8, 23, 12, 0, 0})...)
	src = append(src, plain[signature+header:]...)

	full, err := Full(src)
	if err != nil {
		t.Fatal(err)
	}
	if full.ContentType != PNGContentType || full.Extension != PNGExtension {
		t.Errorf("a PNG's copy is %s%s", full.ContentType, full.Extension)
	}
	if full.Width != 300 || full.Height != 200 {
		t.Errorf("full copy is %dx%d, want 300x200", full.Width, full.Height)
	}

	for _, gone := range []string{"tEXt", "iTXt", "eXIf", "tIME", "a person", "51.5074"} {
		if bytes.Contains(full.Bytes, []byte(gone)) {
			t.Errorf("the published copy still carries %q", gone)
		}
	}
	// What is left is exactly the file that was encoded in the first place.
	if !bytes.Equal(full.Bytes, plain) {
		t.Errorf("stripping a PNG did not leave the bytes it started as: %d vs %d", len(full.Bytes), len(plain))
	}
}

func TestWhatHasNoPublishableCopy(t *testing.T) {
	// A GIF and a WebP are left as they were uploaded, so their originals stay
	// public and nothing here is asked to walk a third container format.
	for _, contentType := range []string{"image/gif", "image/webp", "video/mp4", "model/stl"} {
		if Publishable(contentType) {
			t.Errorf("Publishable(%q) = true", contentType)
		}
	}
	for _, contentType := range []string{"image/jpeg", "image/jpg", "image/png"} {
		if !Publishable(contentType) {
			t.Errorf("Publishable(%q) = false", contentType)
		}
	}

	if _, err := Full([]byte("this is not an image at all")); err == nil {
		t.Error("bytes that are not an image produced a publishable copy")
	}
}
