package printing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math"
	"os"

	"github.com/ThePhaseless/printscan/internal/scanning"
)

// MaxDecodeBytes caps the memory one PNG conversion may use: the decoded
// image plus the RGBA copy that flattening alpha onto white makes. The
// control plane runs with a 256 MiB limit; lower this if OOM kills appear.
const MaxDecodeBytes = 128 << 20

const jpegQuality = 90

// Conversion errors (see Convert).
var (
	// ErrBusy refuses a PNG conversion while another one is decoding.
	ErrBusy = errors.New("another image is being converted")
	// ErrUnsupportedJPEG is a JPEG whose colour model (CMYK, for example)
	// the PDF writer cannot embed.
	ErrUnsupportedJPEG = errors.New("the JPEG colour model cannot be embedded in a PDF")
)

// InvalidImageError is an upload that does not decode as the image it claims to be.
type InvalidImageError struct{ Err error }

func (e *InvalidImageError) Error() string { return "invalid image: " + e.Err.Error() }
func (e *InvalidImageError) Unwrap() error { return e.Err }

// ImageTooLargeError is a PNG whose decode would exceed MaxDecodeBytes.
type ImageTooLargeError struct{ Width, Height int }

func (e *ImageTooLargeError) Error() string {
	return fmt.Sprintf("the image is %d x %d pixels, too large to convert", e.Width, e.Height)
}

// pngSlot lets one PNG decode run at a time; a second one gets ErrBusy.
var pngSlot = make(chan struct{}, 1)

// Convert writes src, in format from, to dst in format to: a pair Negotiate
// returned. A JPEG goes into a one-page PDF unchanged; a PNG is decoded,
// flattened onto white and encoded as JPEG (quality 90), then wrapped in a
// PDF when to is PDF. Errors: *InvalidImageError, *ImageTooLargeError,
// ErrBusy and ErrUnsupportedJPEG, or a write error.
func Convert(dst io.Writer, src io.ReadSeeker, from, to string) error {
	switch {
	case from == FormatJPEG && to == FormatPDF:
		return jpegToPDF(dst, src)
	case from == FormatPNG && (to == FormatJPEG || to == FormatPDF):
	default:
		return fmt.Errorf("printing: no conversion from %s to %s", from, to)
	}

	cfg, err := png.DecodeConfig(src)
	if err != nil {
		return &InvalidImageError{err}
	}
	if !fitsBudget(cfg) {
		return &ImageTooLargeError{cfg.Width, cfg.Height}
	}
	select {
	case pngSlot <- struct{}{}:
	default:
		return ErrBusy
	}
	defer func() { <-pngSlot }()
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	img, err := png.Decode(src)
	if err != nil {
		return &InvalidImageError{err}
	}
	img = flatten(img)
	if to == FormatJPEG {
		return jpeg.Encode(dst, img, &jpeg.Options{Quality: jpegQuality})
	}

	tmp, err := os.CreateTemp("", "printscan-print-*")
	if err != nil {
		return err
	}
	defer func() {
		_ = tmp.Close()
		if err := os.Remove(tmp.Name()); err != nil {
			slog.Warn("cannot remove print conversion file", "path", tmp.Name(), "err", err)
		}
	}()
	if err := jpeg.Encode(tmp, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return err
	}
	return jpegToPDF(dst, tmp)
}

// jpegToPDF wraps a JPEG, unchanged and never decoded, in a one-page PDF
// that fits inside 7.5 x 10 in, turned upright by its EXIF Orientation.
func jpegToPDF(dst io.Writer, src io.ReadSeeker) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	cfg, err := jpeg.DecodeConfig(src)
	if err != nil {
		return &InvalidImageError{err}
	}
	if !scanning.JPEGSupported(cfg.ColorModel) {
		return ErrUnsupportedJPEG
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	head, err := io.ReadAll(io.LimitReader(src, exifScanBytes))
	if err != nil {
		return err
	}
	page := func(int) (io.ReadSeekCloser, error) {
		return struct {
			io.ReadSeeker
			io.Closer
		}{src, io.NopCloser(nil)}, nil
	}
	return scanning.WriteRotatedJPEGPDF(dst, 1, page, pdfDPI(cfg.Width, cfg.Height), exifRotation(head))
}

// exifScanBytes is how far into a JPEG exifRotation looks; cameras put the
// EXIF segment first.
const exifScanBytes = 64 << 10

// exifRotation is the clockwise turn (0, 90, 180 or 270) that shows a JPEG
// upright, from the EXIF Orientation tag in an APP1 segment of b. Phones
// store portrait photos as landscape pixels plus this tag, which PDF's
// DCTDecode ignores. Mirrored orientations (2, 4, 5, 7) keep their turn and
// lose the mirroring. 0 when there is no tag or it cannot be read.
func exifRotation(b []byte) int {
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xd8 {
		return 0
	}
	for i := 2; i+4 <= len(b) && b[i] == 0xff; {
		marker := b[i+1]
		switch marker {
		case 0xff: // fill byte
			i++
			continue
		case 0xda, 0xd9: // image data or end: no EXIF after this
			return 0
		}
		n := int(binary.BigEndian.Uint16(b[i+2:]))
		if n < 2 || i+2+n > len(b) {
			return 0
		}
		if seg := b[i+4 : i+2+n]; marker == 0xe1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			switch tiffOrientation(seg[6:]) {
			case 3, 4:
				return 180
			case 6, 7:
				return 90
			case 5, 8:
				return 270
			}
			return 0
		}
		i += 2 + n
	}
	return 0
}

// tiffOrientation is the Orientation (tag 0x0112) in IFD0 of a TIFF header
// t, or 0.
func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	size := uint64(len(t))
	off := uint64(bo.Uint32(t[4:]))
	if bo.Uint16(t[2:]) != 42 || off+2 > size {
		return 0
	}
	for e, n := off+2, bo.Uint16(t[off:]); n > 0 && e+12 <= size; e, n = e+12, n-1 {
		if bo.Uint16(t[e:]) == 0x0112 && bo.Uint16(t[e+2:]) == 3 { // SHORT
			return int(bo.Uint16(t[e+8:]))
		}
	}
	return 0
}

// pdfDPI is the resolution at which a w x h pixel image fits inside 7.5 x
// 10 in, never below 72 dpi.
func pdfDPI(w, h int) int {
	long, short := float64(max(w, h)), float64(min(w, h))
	return max(72, int(math.Ceil(max(long/10, short/7.5))))
}

// fitsBudget reports whether width x height x (bytes per pixel + 4) is at
// most MaxDecodeBytes; the +4 is the RGBA copy flatten makes.
func fitsBudget(cfg image.Config) bool {
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}
	px := int64(cfg.Width) * int64(cfg.Height)
	return px <= MaxDecodeBytes/(bytesPerPixel(cfg.ColorModel)+4)
}

// bytesPerPixel is the size of one pixel png.Decode returns for model m.
func bytesPerPixel(m color.Model) int64 {
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	switch m {
	case color.GrayModel:
		return 1
	case color.Gray16Model:
		return 2
	case color.RGBAModel, color.NRGBAModel:
		return 4
	}
	return 8 // RGBA64, NRGBA64 and anything unknown
}

// flatten draws an image with transparency onto white; opaque ones are kept.
func flatten(img image.Image) image.Image {
	if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
		return img
	}
	b := img.Bounds()
	out := image.NewRGBA(b)
	draw.Draw(out, b, image.White, image.Point{}, draw.Src)
	draw.Draw(out, b, img, b.Min, draw.Over)
	return out
}
