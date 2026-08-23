package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
)

// upright is orientation 1: the stored pixels are already the right way up,
// and what every unreadable or missing tag means.
const upright = 1

// HeaderBytes is how much of a file's front is enough to find an orientation
// tag. An APP1 segment is at most 64 KiB and sits among a JPEG's first
// segments, so twice that covers one preceded by a thumbnail or a comment.
// It lets a caller measure a camera file without reading all of it.
const HeaderBytes = 128 << 10

// UprightSize returns the size an image of width x height has once its EXIF
// orientation is applied: the axes exchanged for the four values that turn it
// a quarter of the way round, and unchanged for the rest.
//
// src need only be the front of the file -- HeaderBytes of it is enough.
func UprightSize(src []byte, width, height int) (int, int) {
	if swapsAxes(orientation(src)) {
		return height, width
	}
	return width, height
}

// swapsAxes reports whether an orientation puts the stored image on its side.
// The four that do are the four that make a page reserve the wrong box.
func swapsAxes(orientation int) bool {
	return orientation >= 5 && orientation <= 8
}

// orientation reads the EXIF orientation tag (0x0112) out of a JPEG's APP1
// segment: 1 through 8, saying which way up the sensor was held. Anything
// else -- no tag, no EXIF, bytes that do not parse -- is 1, which is the
// identity, so a file this cannot read is left exactly as it is.
//
// JPEG only, on purpose. A WebP can carry an EXIF chunk too, but nothing that
// uploads here produces a rotated WebP, and handling it would mean walking a
// second container format for a case that has never arrived. When one does,
// it is another few lines in front of the same TIFF reader below.
func orientation(src []byte) int {
	if len(src) < 4 || src[0] != 0xFF || src[1] != 0xD8 { // SOI
		return upright
	}

	// Walk the marker segments. Each is 0xFF, a marker, and a big-endian
	// length that counts itself; a run of 0xFF before a marker is padding.
	for i := 2; i+4 <= len(src); {
		if src[i] != 0xFF {
			return upright // not where a marker has to be
		}
		marker := src[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0xD8, marker >= 0xD0 && marker <= 0xD7:
			i += 2 // stands alone, carries no length
			continue
		case marker == 0xDA, marker == 0xD9:
			return upright // the scan: everything past here is pixels
		}

		length := int(binary.BigEndian.Uint16(src[i+2:]))
		if length < 2 || i+2+length > len(src) {
			return upright
		}
		segment := src[i+4 : i+2+length]
		if marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return tagInIFD0(segment[6:])
		}
		i += 2 + length
	}
	return upright
}

// tagInIFD0 reads the orientation out of the TIFF block an APP1 segment
// carries: a header naming its byte order, then the first image file
// directory, which is a count and that many twelve-byte entries.
func tagInIFD0(tiff []byte) int {
	if len(tiff) < 8 {
		return upright
	}

	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return upright
	}
	if order.Uint16(tiff[2:]) != 42 { // the format's own check that the order is right
		return upright
	}

	offset := int(order.Uint32(tiff[4:]))
	if offset < 8 || offset+2 > len(tiff) {
		return upright
	}
	for i := range int(order.Uint16(tiff[offset:])) {
		entry := offset + 2 + i*12
		if entry+12 > len(tiff) {
			return upright
		}
		// Tag, type, count, then the value itself in the last four bytes --
		// where a SHORT fits, and orientation is always one.
		if order.Uint16(tiff[entry:]) != 0x0112 || order.Uint16(tiff[entry+2:]) != 3 {
			continue
		}
		if value := int(order.Uint16(tiff[entry+8:])); value >= 1 && value <= 8 {
			return value
		}
		return upright
	}
	return upright
}

// orient turns img the way the tag says it should be displayed, so that
// everything downstream sees pixels that are already the right way up.
//
// Values 2, 4, 5 and 7 are mirrored as well as turned -- rare, but a phone
// held facing the wrong way produces them, and reading half the tag would be
// worse than reading none of it.
func orient(img image.Image, orientation int) image.Image {
	if orientation <= upright || orientation > 8 {
		return img
	}

	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	dstWidth, dstHeight := width, height
	if swapsAxes(orientation) {
		dstWidth, dstHeight = height, width
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))

	for y := range height {
		for x := range width {
			var dx, dy int
			switch orientation {
			case 2: // mirrored
				dx, dy = width-1-x, y
			case 3: // half turn
				dx, dy = width-1-x, height-1-y
			case 4: // mirrored, half turn
				dx, dy = x, height-1-y
			case 5: // mirrored across the diagonal
				dx, dy = y, x
			case 6: // a quarter turn clockwise
				dx, dy = height-1-y, x
			case 7: // mirrored across the other diagonal
				dx, dy = height-1-y, width-1-x
			case 8: // a quarter turn anticlockwise
				dx, dy = y, width-1-x
			}
			dst.Set(dx, dy, img.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return dst
}
