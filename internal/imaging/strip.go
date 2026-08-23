package imaging

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
)

// FullName is what the full-resolution publishable copy is called in a ladder.
const FullName = "full"

// Full returns the form of an image that may be published: the same pixels,
// exactly, with everything a camera wrote about where it stood and what it was
// taken off. It is a byte walk rather than a re-encode -- the pixels are
// untouched, so this is lossless and costs no quality and almost no time.
//
// What is kept is what the picture needs to look right: JFIF and the ICC
// profile, because colour is not a secret and dropping a profile shifts every
// colour on a wide-gamut screen; Adobe's APP14, which says how the components
// are to be read; and the orientation tag, which says which way up the pixels
// go rather than anything about the photographer. Everything else -- EXIF's
// GPS position, capture time, serial numbers, XMP, comments -- goes.
//
// JPEG and PNG only. A WebP or a GIF has no publishable copy here, for the
// same reason its orientation is not read: nothing that uploads here arrives
// from a camera in one, and it would mean walking a third container format
// for a case that has not happened.
func Full(src []byte) (Rendition, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(src))
	if err != nil {
		return Rendition{}, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}

	rendition := Rendition{Name: FullName}
	switch format {
	case "jpeg":
		rendition.ContentType, rendition.Extension = JPEGContentType, JPEGExtension
		rendition.Bytes = stripJPEG(src)
	case "png":
		rendition.ContentType, rendition.Extension = PNGContentType, PNGExtension
		rendition.Bytes = stripPNG(src)
	default:
		return Rendition{}, fmt.Errorf("%w: %s has no publishable copy", ErrUnsupported, format)
	}

	// The size a page has to reserve for it, which is the size it is looked
	// at: the orientation tag survives the strip, so this stays true.
	rendition.Width, rendition.Height = UprightSize(src, config.Width, config.Height)
	return rendition, nil
}

// stripJPEG copies the segments a picture needs and drops the ones that
// describe the person and the place. The scan itself -- everything from SOS
// on, which is the pixels -- is copied whole.
//
// Bytes that do not walk as a JPEG are returned as they came, because the
// caller has already decoded them as one: an unwalkable file is a bug here,
// not a licence to publish a half-copied image.
func stripJPEG(src []byte) []byte {
	if len(src) < 4 || src[0] != 0xFF || src[1] != 0xD8 {
		return src
	}

	out := make([]byte, 0, len(src))
	out = append(out, src[:2]...) // SOI

	for i := 2; i+4 <= len(src); {
		if src[i] != 0xFF {
			return src
		}
		marker := src[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker >= 0xD0 && marker <= 0xD9:
			out = append(out, src[i:i+2]...)
			i += 2
			continue
		case marker == 0xDA:
			// The scan, and everything after it, is the image.
			return append(out, src[i:]...)
		}

		length := int(binary.BigEndian.Uint16(src[i+2:]))
		if length < 2 || i+2+length > len(src) {
			return src
		}
		segment := src[i : i+2+length]
		payload := segment[4:]

		switch {
		case marker == 0xE1 && bytes.HasPrefix(payload, exifHeader):
			// The one field in here that is about the picture rather than
			// about who took it and where.
			if o := tagInIFD0(payload[len(exifHeader):]); o > upright && o <= 8 {
				out = append(out, orientationSegment(o)...)
			}
		case marker == 0xE0, // JFIF: density, and what a decoder expects first
			marker == 0xE2 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")),
			marker == 0xEE && bytes.HasPrefix(payload, []byte("Adobe")):
			out = append(out, segment...)
		case marker >= 0xE0 && marker <= 0xEF, marker == 0xFE:
			// Every other APPn, and COM. EXIF, XMP, Photoshop's own, and
			// whatever a phone invents next: none of it belongs on a page.
		default:
			out = append(out, segment...)
		}
		i += 2 + length
	}
	return out
}

var exifHeader = []byte("Exif\x00\x00")

// orientationSegment is an APP1 holding one tag and nothing else: which way up
// the pixels go. A stripped JPEG keeps it because dropping it would turn every
// photograph taken sideways back on its side -- the exact thing the ladder
// bakes out.
func orientationSegment(orientation int) []byte {
	tiff := []byte("MM")
	tiff = binary.BigEndian.AppendUint16(tiff, 42)
	tiff = binary.BigEndian.AppendUint32(tiff, 8) // IFD0 follows the header
	tiff = binary.BigEndian.AppendUint16(tiff, 1) // holding one entry
	tiff = binary.BigEndian.AppendUint16(tiff, 0x0112)
	tiff = binary.BigEndian.AppendUint16(tiff, 3) // SHORT
	tiff = binary.BigEndian.AppendUint32(tiff, 1)
	tiff = binary.BigEndian.AppendUint16(tiff, uint16(orientation))
	tiff = append(tiff, 0, 0)                     // the rest of the value field
	tiff = binary.BigEndian.AppendUint32(tiff, 0) // and no directory after it

	payload := append(append([]byte{}, exifHeader...), tiff...)
	segment := []byte{0xFF, 0xE1}
	segment = binary.BigEndian.AppendUint16(segment, uint16(2+len(payload)))
	return append(segment, payload...)
}

// stripPNG drops the ancillary chunks that carry metadata: EXIF, the text
// chunks a tool writes its name into, and the timestamp. Everything a decoder
// needs -- the header, the palette, the pixels, the profile in iCCP, and the
// gamma and chromaticity beside it -- is kept.
//
// Unwalkable bytes are returned unchanged, for the reason stripJPEG gives.
func stripPNG(src []byte) []byte {
	const signature = "\x89PNG\r\n\x1a\n"
	if !bytes.HasPrefix(src, []byte(signature)) {
		return src
	}

	out := make([]byte, 0, len(src))
	out = append(out, src[:len(signature)]...)

	for i := len(signature); i+12 <= len(src); {
		length := int(binary.BigEndian.Uint32(src[i:]))
		if length < 0 || i+12+length > len(src) {
			return src
		}
		name := string(src[i+4 : i+8])
		chunk := src[i : i+12+length]

		switch name {
		case "eXIf", "tEXt", "iTXt", "zTXt", "tIME":
		default:
			out = append(out, chunk...)
		}
		i += 12 + length

		if name == "IEND" {
			break
		}
	}
	return out
}
