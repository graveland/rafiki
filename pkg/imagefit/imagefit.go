// Package imagefit downscales images into a caller-chosen box. It is the
// image-normalization handler of the attachment-materialization design: the
// daemon resizes pasted images on ingest with it, and the cockpit makes
// terminal thumbnails with it. It holds no provider ceilings; every box is
// the caller's.
package imagefit

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // registered so DecodeConfig sniffs GIFs and Thumbnail decodes their first frame
	"image/jpeg"
	"image/png"
	"math"

	"golang.org/x/image/draw"
)

// Box bounds an image. Zero on any field means unbounded on that axis.
type Box struct{ MaxWidth, MaxHeight, MaxPixels int }

// MaxSourcePixels refuses a source image larger than this before decoding it:
// a 20000×20000 PNG is 1.6GB of RGBA.
const MaxSourcePixels = 50_000_000

var (
	ErrTooLarge    = errors.New("imagefit: source image exceeds MaxSourcePixels")
	ErrUnsupported = errors.New("imagefit: unsupported image format")
)

// Fit downscales data into box, returning the resized bytes and the media
// type of what they hold. The sniffed format, not mediaType, decides the
// output media type, which corrects a mislabelled image: a JPEG declared
// image/png comes back as image/jpeg, resized or not. A source whose media
// type is neither PNG nor JPEG passes through untouched, and so does a source
// already inside the box — byte-identical, never upscaled.
func Fit(data []byte, mediaType string, box Box) ([]byte, string, error) {
	switch mediaType {
	case "image/png", "image/jpeg":
	default:
		return data, mediaType, nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("imagefit: decode %s: %w", mediaType, err)
	}
	var out string
	switch format {
	case "png":
		out = "image/png"
	case "jpeg":
		out = "image/jpeg"
	case "gif":
		// A GIF labelled PNG or JPEG is corrected, not resized.
		return data, "image/gif", nil
	default:
		return data, mediaType, nil
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return nil, "", fmt.Errorf("%w: %dx%d", ErrTooLarge, cfg.Width, cfg.Height)
	}
	o := 1
	if format == "jpeg" {
		o = orientation(data)
	}
	dw, dh := cfg.Width, cfg.Height
	if o >= 5 && o <= 8 {
		dw, dh = dh, dw // EXIF orientations 5-8 rotate the image; scale on what the reader sees
	}
	scale := fitScale(dw, dh, box)
	if scale >= 1 {
		return data, out, nil
	}
	nw := max(1, int(math.Floor(float64(dw)*scale)))
	nh := max(1, int(math.Floor(float64(dh)*scale)))
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("imagefit: decode %s: %w", format, err)
	}
	if format == "jpeg" {
		img = orient(img, o)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if format == "png" {
		err = png.Encode(&buf, dst)
	} else {
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85})
	}
	if err != nil {
		return nil, "", fmt.Errorf("imagefit: encode %s: %w", format, err)
	}
	return buf.Bytes(), out, nil
}

// Thumbnail decodes data and returns it upright (EXIF orientation applied)
// and scaled into box as an in-memory image; the caller encodes it, and the
// cockpit hands it to the Kitty encoder as PNG. GIFs are accepted as their
// first frame.
func Thumbnail(data []byte, box Box) (image.Image, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("imagefit: decode: %w", err)
	}
	switch format {
	case "png", "jpeg", "gif":
	default:
		return nil, ErrUnsupported
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxSourcePixels {
		return nil, fmt.Errorf("%w: %dx%d", ErrTooLarge, cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("imagefit: decode: %w", err)
	}
	o := 1
	if format == "jpeg" {
		o = orientation(data)
		img = orient(img, o)
	}
	dw, dh := cfg.Width, cfg.Height
	if o >= 5 && o <= 8 {
		dw, dh = dh, dw
	}
	scale := fitScale(dw, dh, box)
	if scale >= 1 {
		return img, nil
	}
	nw := max(1, int(math.Floor(float64(dw)*scale)))
	nh := max(1, int(math.Floor(float64(dh)*scale)))
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
	return dst, nil
}

// fitScale returns how far dw×dh must shrink to enter box: the minimum of the
// per-axis ratios and the pixel-budget ratio, each term only when its bound
// is set, capped at 1 so nothing is ever upscaled.
func fitScale(dw, dh int, box Box) float64 {
	scale := 1.0
	if box.MaxWidth > 0 {
		scale = math.Min(scale, float64(box.MaxWidth)/float64(dw))
	}
	if box.MaxHeight > 0 {
		scale = math.Min(scale, float64(box.MaxHeight)/float64(dh))
	}
	if box.MaxPixels > 0 {
		scale = math.Min(scale, math.Sqrt(float64(box.MaxPixels)/(float64(dw)*float64(dh))))
	}
	return scale
}
