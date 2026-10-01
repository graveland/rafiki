package imagefit

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/multigres/testkit/assert"
)

// nrgbaOf builds a w×h image.NRGBA filled with a colour, for tests that
// re-encode it themselves (jpegOf) rather than want PNG bytes.
func nrgbaOf(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 40, G: 120, B: 200, A: 255})
		}
	}
	return img
}

// pngOf builds a w×h PNG: an image.NRGBA filled with a colour.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	return encodePNG(t, nrgbaOf(w, h))
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func jpegOf(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

// exifJPEG encodes img as a JPEG and splices an APP1 segment carrying the
// given orientation right after the two-byte SOI.
func exifJPEG(t *testing.T, img image.Image, o uint16) []byte {
	t.Helper()
	base := jpegOf(t, img)
	seg := []byte{
		0xFF, 0xE1, 0x00, 0x22, // APP1 marker, segment length 34 including itself
		'E', 'x', 'i', 'f', 0x00, 0x00,
		'M', 'M', 0x00, 0x2A, // big-endian TIFF
		0x00, 0x00, 0x00, 0x08, // IFD0 sits at tiff[8]
		0x00, 0x01, // one IFD entry
		0x01, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01, // tag 0x0112, type SHORT, count 1
		byte(o >> 8), byte(o), 0x00, 0x00, // the orientation, as a SHORT
		0x00, 0x00, 0x00, 0x00, // next IFD: none
	}
	out := make([]byte, 0, len(base)+len(seg))
	out = append(out, base[:2]...)
	out = append(out, seg...)
	out = append(out, base[2:]...)
	return out
}

// pixelBombPNG builds a PNG whose IHDR claims w×h, with no pixel data —
// DecodeConfig reads only IHDR, so that is all a pixel bomb needs.
func pixelBombPNG(w, h uint32) []byte {
	out := []byte("\x89PNG\r\n\x1a\n")
	ihdr := []byte("IHDR")
	var meta [13]byte
	binary.BigEndian.PutUint32(meta[0:4], w)
	binary.BigEndian.PutUint32(meta[4:8], h)
	meta[8] = 8  // bit depth
	meta[9] = 6  // colour type: RGBA
	meta[10] = 0 // compression
	meta[11] = 0 // filter
	meta[12] = 0 // interlace
	ihdr = append(ihdr, meta[:]...)
	var lenBE, crcBE [4]byte
	binary.BigEndian.PutUint32(lenBE[:], 13)
	binary.BigEndian.PutUint32(crcBE[:], crc32.ChecksumIEEE(ihdr))
	out = append(out, lenBE[:]...)
	out = append(out, ihdr...)
	return append(out, crcBE[:]...)
}

var webpBytes = []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")

func TestFitPassesThroughAnImageInsideTheBox(t *testing.T) {
	c := assert.NewAborting(t)
	src := pngOf(t, 100, 50)
	got, media, err := Fit(src, "image/png", Box{MaxWidth: 200, MaxHeight: 200})
	c.NoError(err)
	c.Eq("image/png", media)
	c.EqDeep(src, got)
}

func TestFitDownscalesIntoWidthAndHeight(t *testing.T) {
	c := assert.NewAborting(t)
	got, media, err := Fit(pngOf(t, 400, 300), "image/png", Box{MaxWidth: 100, MaxHeight: 100})
	c.NoError(err)
	c.Eq("image/png", media)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got))
	c.NoError(err)
	c.Eq(100, cfg.Width)
	c.Eq(75, cfg.Height)
}

func TestFitDownscalesIntoMaxPixels(t *testing.T) {
	c := assert.NewAborting(t)
	got, media, err := Fit(pngOf(t, 400, 300), "image/png", Box{MaxPixels: 3000})
	c.NoError(err)
	c.Eq("image/png", media)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got))
	c.NoError(err)
	c.Eq(63, cfg.Width)
	c.Eq(47, cfg.Height)
}

func TestFitUsesTheAnthropicBox(t *testing.T) {
	c := assert.NewAborting(t)
	got, media, err := Fit(pngOf(t, 2000, 1000), "image/png", Box{MaxWidth: 1568, MaxHeight: 1568, MaxPixels: 1_150_000})
	c.NoError(err)
	c.Eq("image/png", media)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got))
	c.NoError(err)
	c.True(cfg.Width <= 1568, "width %d exceeds 1568", cfg.Width)
	c.True(cfg.Height <= 1568, "height %d exceeds 1568", cfg.Height)
	c.True(cfg.Width*cfg.Height <= 1_150_000, "%dx%d = %d exceeds 1_150_000", cfg.Width, cfg.Height, cfg.Width*cfg.Height)
	c.True(cfg.Width >= 1500, "width %d lost too much detail, want >= 1500", cfg.Width)
}

func TestFitNeverUpscales(t *testing.T) {
	c := assert.NewAborting(t)
	src := pngOf(t, 10, 10)
	got, media, err := Fit(src, "image/png", Box{MaxWidth: 1000, MaxHeight: 1000})
	c.NoError(err)
	c.Eq("image/png", media)
	c.EqDeep(src, got)
}

func TestFitZeroBoxIsUnbounded(t *testing.T) {
	c := assert.NewAborting(t)
	src := pngOf(t, 400, 300)
	got, media, err := Fit(src, "image/png", Box{})
	c.NoError(err)
	c.Eq("image/png", media)
	c.EqDeep(src, got)
}

func TestFitKeepsJPEGAsJPEG(t *testing.T) {
	c := assert.NewAborting(t)
	got, media, err := Fit(jpegOf(t, nrgbaOf(400, 300)), "image/jpeg", Box{MaxWidth: 100, MaxHeight: 100})
	c.NoError(err)
	c.Eq("image/jpeg", media)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(got))
	c.NoError(err)
	c.Eq("jpeg", format)
	c.Eq(100, cfg.Width)
	c.Eq(75, cfg.Height)
}

// gifOf builds a w×h GIF from a paletted two-colour checkerboard.
func gifOf(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	pm := image.NewPaletted(image.Rect(0, 0, w, h), pal)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pm.SetColorIndex(x, y, uint8((x+y)%2))
		}
	}
	var buf bytes.Buffer
	if err := gif.Encode(&buf, pm, nil); err != nil {
		t.Fatalf("gif.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestFitCorrectsAMislabelledGIF(t *testing.T) {
	c := assert.NewAborting(t)
	// The box is small enough that a GIF riding the PNG path would be
	// resized; the gif-relabel arm must return it untouched as image/gif.
	src := gifOf(t, 40, 40)
	got, media, err := Fit(src, "image/png", Box{MaxWidth: 10, MaxHeight: 10})
	c.NoError(err)
	c.Eq("image/gif", media)
	c.EqDeep(src, got)
}

func TestFitCorrectsAMislabelledImage(t *testing.T) {
	c := assert.NewAborting(t)
	src := jpegOf(t, nrgbaOf(400, 300))
	got, media, err := Fit(src, "image/png", Box{MaxWidth: 100, MaxHeight: 100})
	c.NoError(err)
	c.Eq("image/jpeg", media)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got))
	c.NoError(err)
	c.Eq(100, cfg.Width)
	c.Eq(75, cfg.Height)

	inside, media, err := Fit(src, "image/png", Box{MaxWidth: 1000, MaxHeight: 1000})
	c.NoError(err)
	c.Eq("image/jpeg", media)
	c.EqDeep(src, inside)
}

func TestFitPassesOtherFormatsThrough(t *testing.T) {
	c := assert.NewAborting(t)
	got, media, err := Fit(webpBytes, "image/webp", Box{MaxWidth: 10, MaxHeight: 10})
	c.NoError(err)
	c.Eq("image/webp", media)
	c.EqDeep(webpBytes, got)

	src := gifOf(t, 4, 4)
	got, media, err = Fit(src, "image/gif", Box{MaxWidth: 10, MaxHeight: 10})
	c.NoError(err)
	c.Eq("image/gif", media)
	c.EqDeep(src, got)
}

func TestFitRefusesUndecodableBytes(t *testing.T) {
	c := assert.NewAborting(t)
	_, _, err := Fit([]byte("\x89PNGfake"), "image/png", Box{MaxWidth: 10, MaxHeight: 10})
	c.Error(err)
}

func TestFitRefusesAPixelBomb(t *testing.T) {
	c := assert.NewAborting(t)
	_, _, err := Fit(pixelBombPNG(10000, 10000), "image/png", Box{MaxWidth: 1568})
	c.ErrorIs(err, ErrTooLarge)
}

func TestOrientationReadsTheExifTag(t *testing.T) {
	c := assert.NewAborting(t)
	img := image.NewNRGBA(image.Rect(0, 0, 64, 32))
	c.Eq(6, orientation(exifJPEG(t, img, 6)))
	c.Eq(1, orientation(jpegOf(t, img)))
	c.Eq(1, orientation(pngOf(t, 4, 4)))
	c.Eq(1, orientation([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x22, 'E', 'x', 'i', 'f'})) // APP1 cut off mid-TIFF
}

// redBlueHalves builds a 64×32 image whose left half is pure red and right
// half pure blue — the fixture that makes an orientation visible in pixels.
func redBlueHalves() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			if x < 32 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	return img
}

func TestFitAppliesJPEGOrientation(t *testing.T) {
	c := assert.NewAborting(t)
	img := redBlueHalves()
	got, media, err := Fit(exifJPEG(t, img, 6), "image/jpeg", Box{MaxWidth: 32, MaxHeight: 32})
	c.NoError(err)
	c.Eq("image/jpeg", media)
	// Orientation 6 turns the 64×32 source into a 32×64 display; scaled by
	// half that is 16×32, with the red half on top and the blue half below.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got))
	c.NoError(err)
	c.Eq(16, cfg.Width)
	c.Eq(32, cfg.Height)
	decoded, _, err := image.Decode(bytes.NewReader(got))
	c.NoError(err)
	r, _, b, _ := decoded.At(8, 4).RGBA()
	c.True(r>>8 > 150, "pixel (8,4) should be red-dominant, R=%d", r>>8)
	c.True(b>>8 < 100, "pixel (8,4) should be red-dominant, B=%d", b>>8)
	r, _, b, _ = decoded.At(8, 28).RGBA()
	c.True(b>>8 > 150, "pixel (8,28) should be blue-dominant, B=%d", b>>8)
	c.True(r>>8 < 100, "pixel (8,28) should be blue-dominant, R=%d", r>>8)
}

func TestThumbnailFitsTheBox(t *testing.T) {
	c := assert.NewAborting(t)
	got, err := Thumbnail(pngOf(t, 400, 300), Box{MaxWidth: 96, MaxHeight: 48})
	c.NoError(err)
	c.Eq(64, got.Bounds().Dx())
	c.Eq(48, got.Bounds().Dy())
}

func TestThumbnailAppliesJPEGOrientation(t *testing.T) {
	c := assert.NewAborting(t)
	// Box {64,64,0} leaves the 32×64 display untouched, so what is under
	// test is the orientation alone: the returned image is the oriented
	// 32×64, never the raw 64×32.
	got, err := Thumbnail(exifJPEG(t, redBlueHalves(), 6), Box{MaxWidth: 64, MaxHeight: 64})
	c.NoError(err)
	c.Eq(32, got.Bounds().Dx())
	c.Eq(64, got.Bounds().Dy())
}

func TestThumbnailRefusesAPixelBomb(t *testing.T) {
	c := assert.NewAborting(t)
	_, err := Thumbnail(pixelBombPNG(10000, 10000), Box{MaxWidth: 96, MaxHeight: 96})
	c.ErrorIs(err, ErrTooLarge)
}

func TestThumbnailRefusesUnsupportedFormats(t *testing.T) {
	c := assert.NewAborting(t)
	// With only the png/jpeg/gif decoders registered, unknown bytes fail in
	// DecodeConfig rather than reaching the ErrUnsupported branch; either
	// refusal satisfies the guard, so assert the error, not its identity.
	_, err := Thumbnail(webpBytes, Box{MaxWidth: 10, MaxHeight: 10})
	c.Error(err)
}
