package scanning

import (
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

// JPEGSupported reports whether WriteJPEGPDF can embed a JPEG whose
// jpeg.DecodeConfig colour model is m (grey or YCbCr; not CMYK).
func JPEGSupported(m color.Model) bool {
	_, ok := colorSpace(m)
	return ok
}

func colorSpace(m color.Model) (string, bool) {
	switch m {
	case color.GrayModel:
		return "/DeviceGray", true
	case color.YCbCrModel, color.RGBAModel:
		return "/DeviceRGB", true
	}
	return "", false
}

// WriteJPEGPDF writes a PDF with one page per JPEG image, embedded unchanged
// (DCTDecode). Each page's MediaBox is the image size in pixels at dpi.
// open(i) returns page i (0-based); it is read, seeked back, copied and
// closed before the next page opens, so no page is ever held in memory.
func WriteJPEGPDF(w io.Writer, pages int, open func(i int) (io.ReadSeekCloser, error), dpi int) error {
	return WriteRotatedJPEGPDF(w, pages, open, dpi, 0)
}

// WriteRotatedJPEGPDF is WriteJPEGPDF with every page shown turned clockwise
// by rotate degrees (0, 90, 180 or 270), through the page's /Rotate entry.
func WriteRotatedJPEGPDF(w io.Writer, pages int, open func(i int) (io.ReadSeekCloser, error), dpi, rotate int) error {
	if pages <= 0 {
		return errors.New("pdf: no pages")
	}
	if dpi <= 0 {
		return fmt.Errorf("pdf: invalid resolution %d dpi", dpi)
	}
	if rotate != 0 && rotate != 90 && rotate != 180 && rotate != 270 {
		return fmt.Errorf("pdf: invalid rotation %d", rotate)
	}

	// Objects: 1 catalog, 2 page tree, then per page i: 3+3i page, 4+3i image, 5+3i content.
	cw := &countWriter{w: w}
	offsets := make([]int64, 0, 2+3*pages)
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
	cw.printf(" ] /Count %d >>\nendobj\n", pages)

	for i := range pages {
		if err := writePage(cw, begin, open, i, dpi, rotate); err != nil {
			return err
		}
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

// writePage writes page i's page, image and content objects.
func writePage(cw *countWriter, begin func(int), open func(int) (io.ReadSeekCloser, error), i, dpi, rotate int) error {
	page, image, content := 3+3*i, 4+3*i, 5+3*i
	r, err := open(i)
	if err != nil {
		return fmt.Errorf("pdf: page %d: %w", i+1, err)
	}
	defer func() { _ = r.Close() }()
	size, err := r.Seek(0, io.SeekEnd)
	if err == nil {
		_, err = r.Seek(0, io.SeekStart)
	}
	if err != nil {
		return fmt.Errorf("pdf: page %d: %w", i+1, err)
	}
	cfg, err := jpeg.DecodeConfig(r)
	if err != nil {
		return fmt.Errorf("pdf: page %d: %w", i+1, err)
	}
	space, ok := colorSpace(cfg.ColorModel)
	if !ok {
		return fmt.Errorf("pdf: page %d: unsupported JPEG colour model", i+1)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("pdf: page %d: %w", i+1, err)
	}
	width := pt(cfg.Width, dpi)
	height := pt(cfg.Height, dpi)

	rot := ""
	if rotate != 0 {
		rot = fmt.Sprintf(" /Rotate %d", rotate)
	}
	begin(page)
	cw.printf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %s %s]%s /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>\nendobj\n",
		width, height, rot, image, content)
	begin(image)
	cw.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace %s /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n",
		cfg.Width, cfg.Height, space, size)
	if _, err := io.CopyN(cw, r, size); err != nil {
		return fmt.Errorf("pdf: page %d: %w", i+1, err)
	}
	_, _ = io.WriteString(cw, "\nendstream\nendobj\n")
	draw := fmt.Sprintf("q %s 0 0 %s 0 0 cm /Im0 Do Q", width, height)
	begin(content)
	cw.printf("<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(draw), draw)
	return cw.err
}
