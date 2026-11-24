package scanning

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
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

func TestWriteJPEGPDF(t *testing.T) {
	var out bytes.Buffer
	pages := [][]byte{jpegBytes(t, 300, 150, false), jpegBytes(t, 40, 20, true)}
	if err := WriteJPEGPDF(&out, pages, 150); err != nil {
		t.Fatal(err)
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
	} {
		t.Run(name, func(t *testing.T) {
			if err := WriteJPEGPDF(&bytes.Buffer{}, tc.pages, tc.dpi); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
