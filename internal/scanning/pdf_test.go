package scanning

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// jpegBytes encodes a w x h JPEG, grey when gray is set, else RGB.
func jpegBytes(t testing.TB, w, h int, gray bool) []byte {
	t.Helper()
	var img image.Image
	if gray {
		g := image.NewGray(image.Rect(0, 0, w, h))
		for i := range g.Pix {
			g.Pix[i] = uint8(i)
		}
		img = g
	} else {
		c := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := range h {
			for x := range w {
				c.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
			}
		}
		img = c
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// memPage is an in-memory page that records whether it was closed.
type memPage struct {
	*bytes.Reader
	open *int
}

func (p memPage) Close() error { *p.open--; return nil }

// memPages serves pages from memory and fails the test when two are open at once.
func memPages(t *testing.T, pages [][]byte) (func(int) (io.ReadSeekCloser, error), *int) {
	t.Helper()
	open := new(int)
	return func(i int) (io.ReadSeekCloser, error) {
		if *open != 0 {
			t.Errorf("page %d opened while %d page(s) are still open", i, *open)
		}
		*open++
		return memPage{bytes.NewReader(pages[i]), open}, nil
	}, open
}

func TestWriteJPEGPDF(t *testing.T) {
	var out bytes.Buffer
	pages := [][]byte{jpegBytes(t, 300, 150, false), jpegBytes(t, 40, 20, true)}
	open, n := memPages(t, pages)
	if err := WriteJPEGPDF(&out, len(pages), open, 150); err != nil {
		t.Fatal(err)
	}
	if *n != 0 {
		t.Errorf("%d page(s) left open", *n)
	}
	pdf := out.String()

	if !strings.HasPrefix(pdf, "%PDF-") {
		t.Fatalf("missing %%PDF- header: %q", pdf[:20])
	}
	if !strings.Contains(pdf, "/Count 2") {
		t.Error("missing /Count 2")
	}
	if !strings.Contains(pdf, "/MediaBox [0 0 144 72]") {
		t.Error("300x150 px at 150 dpi should give a 144x72 pt MediaBox")
	}
	if !strings.Contains(pdf, "/ColorSpace /DeviceRGB") || !strings.Contains(pdf, "/ColorSpace /DeviceGray") {
		t.Error("expected one RGB and one grey image")
	}
	if !strings.HasSuffix(pdf, "%%EOF\n") {
		t.Error("missing EOF trailer")
	}

	// startxref points at the xref table, and each entry at its "n 0 obj" line.
	m := regexp.MustCompile(`startxref\n(\d+)\n`).FindStringSubmatch(pdf)
	if m == nil {
		t.Fatal("no startxref")
	}
	xref, _ := strconv.Atoi(m[1])
	if !strings.HasPrefix(pdf[xref:], "xref\n") {
		t.Fatalf("startxref %d does not point at xref", xref)
	}
	lines := strings.Split(pdf[xref:], "\n")
	count, err := strconv.Atoi(strings.TrimPrefix(lines[1], "0 "))
	if err != nil {
		t.Fatal(err)
	}
	if count != 1+2+3*len(pages) {
		t.Fatalf("xref has %d entries, want %d", count, 1+2+3*len(pages))
	}
	for i := 1; i < count; i++ {
		entry := lines[2+i]
		if len(entry)+1 != 20 {
			t.Errorf("xref entry %d is %d bytes, want 20", i, len(entry)+1)
		}
		off, _ := strconv.Atoi(entry[:10])
		if want := strconv.Itoa(i) + " 0 obj"; !strings.HasPrefix(pdf[off:], want) {
			t.Errorf("xref entry %d points at %q, want %q", i, pdf[off:off+10], want)
		}
	}
}

func TestWriteJPEGPDFRejects(t *testing.T) {
	page := jpegBytes(t, 8, 8, false)
	for name, tc := range map[string]struct {
		pages [][]byte
		dpi   int
	}{
		"no pages": {nil, 300},
		"zero dpi": {[][]byte{page}, 0},
		"not jpeg": {[][]byte{[]byte("not a jpeg")}, 300},
		"cmyk":     {[][]byte{cmykJPEG(t)}, 300},
	} {
		t.Run(name, func(t *testing.T) {
			open, _ := memPages(t, tc.pages)
			if err := WriteJPEGPDF(&bytes.Buffer{}, len(tc.pages), open, tc.dpi); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	t.Run("rotation not a quarter turn", func(t *testing.T) {
		open, _ := memPages(t, [][]byte{page})
		if err := WriteRotatedJPEGPDF(&bytes.Buffer{}, 1, open, 300, 45); err == nil {
			t.Fatal("expected an error")
		}
	})
	t.Run("open fails", func(t *testing.T) {
		boom := errors.New("boom")
		err := WriteJPEGPDF(&bytes.Buffer{}, 1, func(int) (io.ReadSeekCloser, error) { return nil, boom }, 300)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
}

func TestJPEGSupported(t *testing.T) {
	for _, tc := range []struct {
		img  []byte
		want bool
	}{
		{jpegBytes(t, 8, 8, false), true},
		{jpegBytes(t, 8, 8, true), true},
		{cmykJPEG(t), false},
	} {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(tc.img))
		if err != nil {
			t.Fatal(err)
		}
		if got := JPEGSupported(cfg.ColorModel); got != tc.want {
			t.Errorf("JPEGSupported(%T) = %v, want %v", cfg.ColorModel, got, tc.want)
		}
	}
}

// cmykJPEG returns the header of an 8x8 four-component (CMYK) JPEG, enough
// for jpeg.DecodeConfig; image/jpeg cannot encode CMYK.
func cmykJPEG(t testing.TB) []byte {
	t.Helper()
	b := []byte{0xff, 0xd8, 0xff, 0xc0, 0, 20, 8, 0, 8, 0, 8, 4}
	for id := range byte(4) {
		b = append(b, id+1, 0x11, 0)
	}
	b = append(b, 0xff, 0xda, 0, 14, 4, 1, 0, 2, 0, 3, 0, 4, 0, 0, 63, 0)
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(b)); err != nil || cfg.ColorModel != color.CMYKModel {
		t.Fatalf("cmykJPEG: DecodeConfig = %+v, %v; want a CMYK config", cfg, err)
	}
	return b
}
