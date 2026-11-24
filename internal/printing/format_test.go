package printing

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteSampleJPEG regenerates testdata/sample.jpg (an 8x8 JPEG) when
// UPDATE_TESTDATA=1; the committed file is what every other test reads.
func TestWriteSampleJPEG(t *testing.T) {
	if os.Getenv("UPDATE_TESTDATA") != "1" {
		t.Skip("set UPDATE_TESTDATA=1 to regenerate testdata/sample.jpg")
	}
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := range uint8(8) {
		for y := range uint8(8) {
			img.Set(int(x), int(y), color.RGBA{x * 32, y * 32, 128, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sample.jpg"), b.Bytes(), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed test path
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name     string
		head     []byte
		filename string
		want     string
	}{
		{"pdf fixture", readTestdata(t, "sample.pdf"), "x.bin", FormatPDF},
		{"jpeg fixture", readTestdata(t, "sample.jpg"), "x.pdf", FormatJPEG},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00"), "", FormatPNG},
		{"pwg raster", []byte("RaS2PwgRaster\x00"), "", FormatPWGRaster},
		{"urf", []byte("UNIRAST\x00\x00\x00\x00\x01"), "", FormatURF},
		{"postscript", []byte("%!PS-Adobe-3.0\n%%Pages: 1\n"), "", FormatPostScript},
		{"plain text", []byte("Hello,\tworld\r\nsecond line\f"), "notes", FormatText},
		{"utf-8 text", []byte("Zażółć gęślą jaźń\n"), "", FormatText},
		{"text cut mid-rune at the 512-byte boundary", append(bytes.Repeat([]byte("a"), 510), 0xc5), "", FormatText},
		{"binary", []byte{0x00, 0x01, 0x02, 0xff, 0xfe}, "", FormatUnknown},
		{"binary named .jpg", []byte{0x00, 0x01, 0x02}, "photo.JPG", FormatUnknown},
		{"binary named .txt", []byte{0x00, 0x01, 0x02}, "notes.txt", FormatUnknown},
		{"empty named .pdf", nil, "doc.pdf", FormatUnknown},
		{"empty", nil, "", FormatUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(tt.head, tt.filename); got != tt.want {
				t.Errorf("Detect() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNegotiate(t *testing.T) {
	supported := []string{"application/pdf", "image/urf", "Image/JPEG", "text/plain; charset=utf-8", "application/octet-stream"}
	for _, ok := range []string{FormatPDF, FormatURF, FormatJPEG, FormatText} {
		if err := Negotiate(ok, supported); err != nil {
			t.Errorf("Negotiate(%q) = %v, want nil", ok, err)
		}
	}

	for _, bad := range []string{FormatPNG, FormatPWGRaster, FormatUnknown} {
		err := Negotiate(bad, supported)
		var ufe *UnsupportedFormatError
		if !errors.As(err, &ufe) {
			t.Fatalf("Negotiate(%q) = %v, want *UnsupportedFormatError", bad, err)
		}
		if ufe.Detected != bad || len(ufe.Supported) != len(supported) {
			t.Errorf("error = %+v", ufe)
		}
		if ufe.Error() == "" {
			t.Error("empty error message")
		}
	}

	if err := Negotiate(FormatPDF, nil); err == nil {
		t.Error("Negotiate with no supported formats succeeded")
	}
}
