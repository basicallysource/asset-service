package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// photograph is the same plausible image the rest of these tests use, as an
// image rather than as bytes.
func photograph(t *testing.T, width, height int) image.Image {
	t.Helper()

	decoded, err := png.Decode(bytes.NewReader(testImage(t, width, height)))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// jpegOf encodes an image as a JPEG, which is the only format here that
// carries an orientation tag.
func jpegOf(t *testing.T, img image.Image) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withOrientation splices in the APP1 segment a camera writes: the EXIF
// header, a TIFF block in the named byte order ("MM" or "II"), and one IFD0
// entry saying which way up the sensor was held.
func withOrientation(t *testing.T, jpg []byte, orientation int, byteOrder string) []byte {
	t.Helper()

	order := binary.AppendByteOrder(binary.BigEndian)
	if byteOrder == "II" {
		order = binary.LittleEndian
	}

	tiff := []byte(byteOrder)
	tiff = order.AppendUint16(tiff, 42) // the format's own check of the order
	tiff = order.AppendUint32(tiff, 8)  // IFD0 starts straight after this header
	tiff = order.AppendUint16(tiff, 1)  // holding one entry
	tiff = order.AppendUint16(tiff, 0x0112)
	tiff = order.AppendUint16(tiff, 3) // SHORT
	tiff = order.AppendUint32(tiff, 1) // one of them
	tiff = order.AppendUint16(tiff, uint16(orientation))
	tiff = append(tiff, 0, 0)          // the rest of the four-byte value field
	tiff = order.AppendUint32(tiff, 0) // and no directory after this one

	return withAPP1(t, jpg, append([]byte("Exif\x00\x00"), tiff...))
}

// withAPP1 puts an APP1 segment where a camera's EXIF goes: in front of
// everything but the SOI marker.
func withAPP1(t *testing.T, jpg, payload []byte) []byte {
	t.Helper()
	return withSegment(t, jpg, 0xE1, payload)
}

func TestALadderComesOutUprightWhateverTheTagSays(t *testing.T) {
	// 1200x900 of stored pixels. The four values above four say the camera was
	// held sideways, which makes the picture 900x1200 to look at -- so a 320px
	// rung of it is 320x426 rather than 320x240.
	for _, want := range []struct {
		orientation   int
		width, height int
	}{
		{1, 320, 240},
		{2, 320, 240}, // mirrored
		{3, 320, 240},
		{5, 320, 426}, // mirrored, and on its side
		{6, 320, 426},
		{8, 320, 426},
	} {
		src := withOrientation(t, jpegOf(t, photograph(t, 1200, 900)), want.orientation, "MM")

		ladder, err := Ladder(src, Options{Widths: []int{320}})
		if err != nil {
			t.Fatalf("orientation %d: %v", want.orientation, err)
		}
		if len(ladder) != 1 {
			t.Fatalf("orientation %d: got %d renditions, want 1", want.orientation, len(ladder))
		}
		if ladder[0].Width != want.width || ladder[0].Height != want.height {
			t.Errorf("orientation %d: rung says %dx%d, want %dx%d",
				want.orientation, ladder[0].Width, ladder[0].Height, want.width, want.height)
		}

		// And the bytes agree with what the rung claims about itself.
		cfg, _, err := image.DecodeConfig(bytes.NewReader(ladder[0].Bytes))
		if err != nil {
			t.Fatalf("orientation %d: rung does not decode: %v", want.orientation, err)
		}
		if cfg.Width != want.width || cfg.Height != want.height {
			t.Errorf("orientation %d: rung decodes %dx%d, want %dx%d",
				want.orientation, cfg.Width, cfg.Height, want.width, want.height)
		}
	}
}

func TestTheTurnGoesTheWayTheTagMeans(t *testing.T) {
	// Half red, half blue, so the rung says where the pixels went. Dimensions
	// alone cannot tell a quarter turn from its opposite, or a mirror from
	// nothing at all.
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := range 600 {
		for x := range 800 {
			half := color.RGBA{R: 220, A: 255}
			if x >= 400 {
				half = color.RGBA{B: 220, A: 255}
			}
			img.Set(x, y, half)
		}
	}
	stored := jpegOf(t, img)

	for _, want := range []struct {
		orientation int
		red         string // where the left half of the stored frame ends up
	}{
		{2, "right"},  // mirrored
		{6, "top"},    // a quarter turn clockwise
		{8, "bottom"}, // and anticlockwise
	} {
		ladder, err := Ladder(withOrientation(t, stored, want.orientation, "MM"), Options{Widths: []int{300}})
		if err != nil {
			t.Fatalf("orientation %d: %v", want.orientation, err)
		}
		rung, _, err := image.Decode(bytes.NewReader(ladder[0].Bytes))
		if err != nil {
			t.Fatalf("orientation %d: %v", want.orientation, err)
		}

		bounds := rung.Bounds()
		var redAt, blueAt image.Point
		switch want.red {
		case "right":
			redAt = image.Pt(bounds.Dx()*3/4, bounds.Dy()/2)
			blueAt = image.Pt(bounds.Dx()/4, bounds.Dy()/2)
		case "top":
			redAt = image.Pt(bounds.Dx()/2, bounds.Dy()/8)
			blueAt = image.Pt(bounds.Dx()/2, bounds.Dy()*7/8)
		case "bottom":
			redAt = image.Pt(bounds.Dx()/2, bounds.Dy()*7/8)
			blueAt = image.Pt(bounds.Dx()/2, bounds.Dy()/8)
		}

		if !redder(rung, redAt) {
			t.Errorf("orientation %d: the red half is not at the %s of the rung", want.orientation, want.red)
		}
		if redder(rung, blueAt) {
			t.Errorf("orientation %d: the blue half is not opposite the %s", want.orientation, want.red)
		}
	}
}

func redder(img image.Image, at image.Point) bool {
	r, _, b, _ := img.At(at.X, at.Y).RGBA()
	return r > b
}

func TestAMissingOrUnreadableTagMeansUpright(t *testing.T) {
	// Anything this cannot read leaves the pixels exactly as they were stored,
	// which is the only safe answer: turning a picture that was already up is
	// as wrong as leaving a sideways one alone.
	plain := jpegOf(t, photograph(t, 1200, 900))
	for what, src := range map[string][]byte{
		"a JPEG with no APP1 at all":  plain,
		"a PNG, which carries no tag": testImage(t, 1200, 900),
		"a value outside 1-8":         withOrientation(t, plain, 9, "MM"),
		"a value of zero":             withOrientation(t, plain, 0, "II"),
		"an APP1 that is not EXIF":    withAPP1(t, plain, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta/>")),
		"an EXIF with no byte order":  withAPP1(t, plain, []byte("Exif\x00\x00this is not a TIFF header")),
		"an EXIF cut short":           withAPP1(t, plain, []byte("Exif\x00\x00MM\x00")),
	} {
		ladder, err := Ladder(src, Options{Widths: []int{320}})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ladder[0].Width != 320 || ladder[0].Height != 240 {
			t.Errorf("%s: rung is %dx%d, want the stored 320x240", what, ladder[0].Width, ladder[0].Height)
		}
	}
}

func TestUprightSizeSwapsOnlyTheQuarterTurns(t *testing.T) {
	// What the manifest reports, which has to be the shape the ladder is built
	// in or a page reserves a box the picture does not fit.
	plain := jpegOf(t, photograph(t, 400, 300))

	for orientation, sideways := range map[int]bool{
		1: false, 2: false, 3: false, 4: false,
		5: true, 6: true, 7: true, 8: true,
	} {
		wantWidth, wantHeight := 400, 300
		if sideways {
			wantWidth, wantHeight = 300, 400
		}
		// Both byte orders, because a TIFF block names its own and phones
		// disagree about which they write.
		for _, byteOrder := range []string{"MM", "II"} {
			width, height := UprightSize(withOrientation(t, plain, orientation, byteOrder), 400, 300)
			if width != wantWidth || height != wantHeight {
				t.Errorf("orientation %d (%s): %dx%d, want %dx%d",
					orientation, byteOrder, width, height, wantWidth, wantHeight)
			}
		}
	}

	if width, height := UprightSize(plain, 400, 300); width != 400 || height != 300 {
		t.Errorf("with no tag at all: %dx%d, want 400x300", width, height)
	}
}
