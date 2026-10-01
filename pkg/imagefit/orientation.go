package imagefit

import (
	"encoding/binary"
	"image"
)

// The EXIF orientation tag, inside IFD0.
const orientationTag = 0x0112

// orientation reads the EXIF orientation of a JPEG, 1..8, and returns 1 for
// anything it cannot read: no JPEG start, no APP1 EXIF segment before the
// first scan, a truncated or malformed TIFF block, or an out-of-range tag
// value. It never allocates; callers can afford it on every ingest.
func orientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+1 < len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		if marker == 0xFF {
			i++ // fill bytes may pad the gap before a marker
			continue
		}
		if marker == 0xDA {
			break // SOS: entropy-coded data follows; EXIF never comes after it
		}
		if i+4 > len(data) {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(data[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(data) {
			return 1
		}
		seg := data[i+4 : i+2+segLen] // the length counts its own two bytes
		if marker == 0xE1 && len(seg) >= len("Exif\x00\x00") && string(seg[:6]) == "Exif\x00\x00" {
			if o := ifdOrientation(seg[6:]); o != 1 {
				return o
			}
		}
		i += 2 + segLen
	}
	return 1
}

// ifdOrientation reads the orientation tag out of a TIFF block (everything
// after the six-byte "Exif\000\000" signature). Every index is bounds-checked
// against the segment, which is the only trust this file has.
func ifdOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var bo binary.ByteOrder = binary.LittleEndian
	switch string(tiff[:2]) {
	case "II":
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(tiff[4:8]))
	if off < 2 || off+2 > len(tiff) {
		return 1
	}
	n := int(bo.Uint16(tiff[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(tiff) {
			return 1
		}
		if bo.Uint16(tiff[e:e+2]) != orientationTag {
			continue
		}
		o := int(bo.Uint16(tiff[e+8 : e+10]))
		if o >= 1 && o <= 8 {
			return o
		}
		return 1
	}
	return 1
}

// orient rewrites img for an EXIF orientation, returning a new *image.NRGBA
// whose bounds start at 0,0. Orientation 1 and anything out of range return
// img unchanged. Each mapping is the inverse of where EXIF says a source
// pixel lands, so the loop reads the source rather than writing the
// destination twice.
func orient(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			var sx, sy int
			switch o {
			case 2: // left-right flip
				sx, sy = w-1-dx, dy
			case 3: // 180°
				sx, sy = w-1-dx, h-1-dy
			case 4: // top-bottom flip
				sx, sy = dx, h-1-dy
			case 5: // transpose
				sx, sy = dy, dx
			case 6: // 90° clockwise
				sx, sy = dy, h-1-dx
			case 7: // transverse
				sx, sy = w-1-dy, h-1-dx
			case 8: // 90° counter-clockwise
				sx, sy = w-1-dy, dx
			}
			dst.Set(dx, dy, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}
