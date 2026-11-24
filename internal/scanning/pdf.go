package scanning

import (
	"bytes"
	"errors"
	"fmt"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"strconv"
)

// countWriter counts bytes written and keeps the first write error.
type countWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.err = err
	return n, err
}

// printf writes to c; the first error is kept in c.err.
func (c *countWriter) printf(format string, a ...any) { _, _ = fmt.Fprintf(c, format, a...) }

// WriteJPEGPDF writes a PDF with one page per JPEG image, embedded unchanged
// (DCTDecode). Each page's MediaBox is the image size in pixels at dpi.
func WriteJPEGPDF(w io.Writer, pages [][]byte, dpi int) error {
	if len(pages) == 0 {
		return errors.New("pdf: no pages")
	}
	if dpi <= 0 {
		return fmt.Errorf("pdf: invalid resolution %d dpi", dpi)
	}

	// Objects: 1 catalog, 2 page tree, then per page i: 3+3i page, 4+3i image, 5+3i content.
	cw := &countWriter{w: w}
	offsets := make([]int64, 0, 2+3*len(pages))
	begin := func(num int) {
		offsets = append(offsets, cw.n)
		cw.printf("%d 0 obj\n", num)
	}

	_, _ = io.WriteString(cw, "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	begin(1)
	_, _ = io.WriteString(cw, "<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	begin(2)
	_, _ = io.WriteString(cw, "<< /Type /Pages /Kids [")
	for i := range pages {
		cw.printf(" %d 0 R", 3+3*i)
	}
	cw.printf(" ] /Count %d >>\nendobj\n", len(pages))

	for i, img := range pages {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(img))
		if err != nil {
			return fmt.Errorf("pdf: page %d: %w", i+1, err)
		}
		var space string
		switch cfg.ColorModel {
		case color.GrayModel:
			space = "/DeviceGray"
		case color.YCbCrModel, color.RGBAModel:
			space = "/DeviceRGB"
		default:
			return fmt.Errorf("pdf: page %d: unsupported JPEG colour model", i+1)
		}
		width := pt(cfg.Width, dpi)
		height := pt(cfg.Height, dpi)
		page, image, content := 3+3*i, 4+3*i, 5+3*i

		begin(page)
		cw.printf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s] /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>\nendobj\n",
			width, height, image, content)
		begin(image)
		cw.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n",
			cfg.Width, cfg.Height, space, len(img))
		_, _ = cw.Write(img)
		_, _ = io.WriteString(cw, "\nendstream\nendobj\n")
		draw := fmt.Sprintf("q %s 0 0 %s 0 0 cm /Im0 Do Q", width, height)
		begin(content)
		cw.printf("<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(draw), draw)
	}

	xref := cw.n
	cw.printf("xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		cw.printf("%010d 00000 n \n", off)
	}
	cw.printf("trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return cw.err
}

// pt converts pixels at dpi to PDF points (1/72 inch).
func pt(px, dpi int) string {
	return strconv.FormatFloat(math.Round(float64(px)*7200/float64(dpi))/100, 'f', -1, 64)
}
