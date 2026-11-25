package printing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"github.com/ThePhaseless/printscan/internal/apitypes"
)

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngHeader is a PNG signature and IHDR chunk (8-bit RGBA, w x h): enough
// for png.DecodeConfig, without the pixels.
func pngHeader(w, h uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, w)
	ihdr = binary.BigEndian.AppendUint32(ihdr, h)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	b := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 13)
	b = append(b, "IHDR"...)
	b = append(b, ihdr...)
	return binary.BigEndian.AppendUint32(b, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
}

// cmykJPEG is the header of a four-component JPEG; image/jpeg cannot encode CMYK.
func cmykJPEG(t *testing.T) []byte {
	t.Helper()
	b := []byte{0xff, 0xd8, 0xff, 0xc0, 0, 20, 8, 0, 8, 0, 8, 4}
	for id := range byte(4) {
		b = append(b, id+1, 0x11, 0)
	}
	b = append(b, 0xff, 0xda, 0, 14, 4, 1, 0, 2, 0, 3, 0, 4, 0, 0, 63, 0)
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(b)); err != nil || cfg.ColorModel != color.CMYKModel {
		t.Fatalf("cmykJPEG: DecodeConfig = %+v, %v", cfg, err)
	}
	return b
}

func convert(from, to string, src []byte) ([]byte, error) {
	var out bytes.Buffer
	err := Convert(&out, bytes.NewReader(src), from, to)
	return out.Bytes(), err
}

func TestConversions(t *testing.T) {
	png2jpeg := apitypes.FormatConversion{From: FormatPNG, To: FormatJPEG}
	png2pdf := apitypes.FormatConversion{From: FormatPNG, To: FormatPDF}
	jpeg2pdf := apitypes.FormatConversion{From: FormatJPEG, To: FormatPDF}
	for _, tc := range []struct {
		supported []string
		want      []apitypes.FormatConversion
	}{
		{[]string{"application/pdf", "Image/JPEG; q=1"}, []apitypes.FormatConversion{png2jpeg}},
		{[]string{"application/pdf"}, []apitypes.FormatConversion{png2pdf, jpeg2pdf}},
		{[]string{"image/jpeg"}, []apitypes.FormatConversion{png2jpeg}},
		{[]string{"image/png", "image/jpeg", "application/pdf"}, []apitypes.FormatConversion{}},
		{[]string{"image/urf"}, []apitypes.FormatConversion{}},
		{nil, []apitypes.FormatConversion{}},
	} {
		if got := Conversions(tc.supported); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Conversions(%v) = %v, want %v", tc.supported, got, tc.want)
		}
	}
}

func TestConvertPNGToJPEGFlattensAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for x := range 20 { // left half opaque red, right half transparent
		for y := range 20 {
			img.Set(x, y, color.NRGBA{255, 0, 0, 255})
		}
	}
	out, err := convert(FormatPNG, FormatJPEG, pngBytes(t, img))
	if err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a JPEG: %v", err)
	}
	if got.Bounds().Dx() != 40 || got.Bounds().Dy() != 20 {
		t.Fatalf("size %v, want 40x20", got.Bounds())
	}
	r, g, b, _ := got.At(35, 10).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Errorf("transparent pixel became (%d,%d,%d), want white", r>>8, g>>8, b>>8)
	}
	r, g, _, _ = got.At(5, 10).RGBA()
	if r>>8 < 200 || g>>8 > 60 {
		t.Errorf("opaque red pixel became (%d,%d)", r>>8, g>>8)
	}
}

func TestConvertToPDF(t *testing.T) {
	t.Run("png", func(t *testing.T) {
		// 3000x1500: long side 3000/10 = 300 dpi beats short side 1500/7.5 = 200.
		out, err := convert(FormatPNG, FormatPDF, pngBytes(t, image.NewGray(image.Rect(0, 0, 3000, 1500))))
		if err != nil {
			t.Fatal(err)
		}
		pdf := string(out)
		if !strings.HasPrefix(pdf, "%PDF-") || !strings.Contains(pdf, "/DCTDecode") || !strings.Contains(pdf, "/MediaBox [0 0 720 360]") {
			t.Fatalf("not a one-page 10x5 in PDF: %.300q", pdf)
		}
	})
	t.Run("jpeg embedded unchanged", func(t *testing.T) {
		src := jpegOf(t, 100, 50) // small: the 72 dpi floor applies
		out, err := convert(FormatJPEG, FormatPDF, src)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out, src) || !bytes.Contains(out, []byte("/MediaBox [0 0 100 50]")) {
			t.Fatalf("PDF does not embed the JPEG at 72 dpi: %.300q", out)
		}
	})
}

func TestPDFDPI(t *testing.T) {
	for _, tc := range []struct{ w, h, want int }{
		{100, 50, 72},     // floor
		{3000, 1500, 300}, // long side decides
		{1500, 3000, 300},
		{2000, 1950, 260}, // short side decides: 1950/7.5 = 260
		{2481, 1000, 249}, // rounded up
	} {
		if got := pdfDPI(tc.w, tc.h); got != tc.want {
			t.Errorf("pdfDPI(%d, %d) = %d, want %d", tc.w, tc.h, got, tc.want)
		}
	}
}

func TestConvertRefusals(t *testing.T) {
	var invalid *InvalidImageError
	for name, tc := range map[string]struct {
		from, to string
		src      []byte
		want     func(error) bool
	}{
		"cmyk jpeg":       {FormatJPEG, FormatPDF, cmykJPEG(t), func(err error) bool { return errors.Is(err, ErrUnsupportedJPEG) }},
		"broken jpeg":     {FormatJPEG, FormatPDF, []byte("\xff\xd8\xffjunk"), func(err error) bool { return errors.As(err, &invalid) }},
		"broken png":      {FormatPNG, FormatJPEG, []byte("\x89PNG\r\n\x1a\njunk"), func(err error) bool { return errors.As(err, &invalid) }},
		"truncated png":   {FormatPNG, FormatPDF, pngHeader(10, 10), func(err error) bool { return errors.As(err, &invalid) }},
		"oversized png":   {FormatPNG, FormatJPEG, pngHeader(20000, 20000), func(err error) bool { var e *ImageTooLargeError; return errors.As(err, &e) }},
		"no such pairing": {FormatText, FormatPDF, []byte("hi"), func(err error) bool { return err != nil }},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := convert(tc.from, tc.to, tc.src); !tc.want(err) {
				t.Fatalf("Convert = %v", err)
			}
		})
	}
}

func TestDecodeBudget(t *testing.T) {
	if MaxDecodeBytes != 128<<20 {
		t.Fatalf("MaxDecodeBytes = %d, want 128 MiB", MaxDecodeBytes)
	}
	gray := int64(MaxDecodeBytes) / (1 + 4)
	for _, tc := range []struct {
		w, h  int
		model color.Model
		ok    bool
	}{
		{int(gray), 1, color.GrayModel, true},
		{int(gray) + 1, 1, color.GrayModel, false},
		{4096, 4096, color.NRGBAModel, true}, // 16 Mpx * 8 = 128 MiB
		{4096, 4097, color.NRGBAModel, false},
		{4096, 2048, color.NRGBA64Model, true},         // 8 Mpx * 12 = 96 MiB
		{4096, 4096, color.Palette{color.Black}, true}, // 16 Mpx * 5 = 80 MiB
		{1 << 30, 1 << 30, color.GrayModel, false},     // no overflow
	} {
		if got := fitsBudget(image.Config{Width: tc.w, Height: tc.h, ColorModel: tc.model}); got != tc.ok {
			t.Errorf("fitsBudget(%dx%d %T) = %v, want %v", tc.w, tc.h, tc.model, got, tc.ok)
		}
	}
}

func TestConvertOnePNGAtATime(t *testing.T) {
	src := pngBytes(t, image.NewGray(image.Rect(0, 0, 8, 8)))
	pngSlot <- struct{}{} // another conversion is decoding
	_, err := convert(FormatPNG, FormatJPEG, src)
	<-pngSlot
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Convert while busy = %v, want ErrBusy", err)
	}
	// A JPEG needs no decode, so it does not wait for the slot.
	pngSlot <- struct{}{}
	_, err = convert(FormatJPEG, FormatPDF, jpegOf(t, 8, 8))
	<-pngSlot
	if err != nil {
		t.Fatalf("JPEG to PDF while a PNG decodes: %v", err)
	}
	if _, err := convert(FormatPNG, FormatJPEG, src); err != nil {
		t.Fatalf("Convert after the slot is free: %v", err)
	}
}

// withOrientation splices an EXIF APP1 segment holding Orientation o into a
// JPEG, right after SOI, in little-endian (II) or big-endian (MM) byte order.
func withOrientation(src []byte, o uint16, bo binary.AppendByteOrder) []byte {
	tiff := []byte("MM")
	if bo == binary.LittleEndian {
		tiff = []byte("II")
	}
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	tiff = bo.AppendUint16(tiff, 1)      // one IFD entry
	tiff = bo.AppendUint16(tiff, 0x0112) // Orientation
	tiff = bo.AppendUint16(tiff, 3)      // SHORT
	tiff = bo.AppendUint32(tiff, 1)      // count
	tiff = bo.AppendUint16(tiff, o)      // value, left-justified
	tiff = append(tiff, 0, 0)            // padding
	tiff = bo.AppendUint32(tiff, 0)      // no next IFD
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := binary.BigEndian.AppendUint16([]byte{0xff, 0xe1}, uint16(len(seg)+2)) //nolint:gosec // a fixture of a few dozen bytes
	out := append([]byte{}, src[:2]...)
	out = append(out, app1...)
	out = append(out, seg...)
	return append(out, src[2:]...)
}

// A phone saves a portrait photo as landscape pixels plus an EXIF
// Orientation; the PDF page carries the matching /Rotate.
func TestConvertJPEGOrientation(t *testing.T) {
	for name, tc := range map[string]struct {
		src  []byte
		want string
	}{
		"none":          {jpegOf(t, 40, 30), ""},
		"1 upright":     {withOrientation(jpegOf(t, 40, 30), 1, binary.LittleEndian), ""},
		"3 upside down": {withOrientation(jpegOf(t, 40, 30), 3, binary.BigEndian), "/Rotate 180"},
		"6 II":          {withOrientation(jpegOf(t, 40, 30), 6, binary.LittleEndian), "/Rotate 90"},
		"6 MM":          {withOrientation(jpegOf(t, 40, 30), 6, binary.BigEndian), "/Rotate 90"},
		"8 II":          {withOrientation(jpegOf(t, 40, 30), 8, binary.LittleEndian), "/Rotate 270"},
		"7 mirrored":    {withOrientation(jpegOf(t, 40, 30), 7, binary.BigEndian), "/Rotate 90"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := convert(FormatJPEG, FormatPDF, tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out, tc.src) {
				t.Fatal("the JPEG is not embedded unchanged")
			}
			got := ""
			if i := bytes.Index(out, []byte("/Rotate ")); i >= 0 {
				got = string(out[i : i+bytes.IndexAny(out[i+8:], " >")+8])
			}
			if got != tc.want {
				t.Fatalf("page rotation %q, want %q", got, tc.want)
			}
		})
	}
}

// A malformed EXIF segment never panics and reads as upright.
func TestExifRotationMalformed(t *testing.T) {
	good := withOrientation([]byte{0xff, 0xd8, 0xff, 0xd9}, 6, binary.LittleEndian)
	if got := exifRotation(good); got != 90 {
		t.Fatalf("exifRotation(good) = %d, want 90", got)
	}
	bad := bytes.Clone(good)
	bad[4+2+6+4] = 0xff // IFD offset far past the end
	for name, b := range map[string][]byte{
		"cut entry":    good[:len(good)-12],
		"short length": {0xff, 0xd8, 0xff, 0xe1, 0, 1},
		"long length":  {0xff, 0xd8, 0xff, 0xe1, 0xff, 0xff, 'E', 'x', 'i', 'f', 0, 0},
		"bad offset":   bad,
		"not a jpeg":   []byte("not a jpeg"),
	} {
		if got := exifRotation(b); got != 0 {
			t.Errorf("%s: exifRotation = %d, want 0", name, got)
		}
	}
}
