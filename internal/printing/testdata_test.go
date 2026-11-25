package printing

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// TestTestdataPDFsValid checks the hand-written PDF fixtures byte by byte: each
// in-use xref entry points at its "N 0 obj", startxref at the xref table, and
// each stream's /Length matches its data. A rename or edit that shifts a byte
// breaks them silently otherwise.
func TestTestdataPDFsValid(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.pdf"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no PDF fixtures: %v", err)
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			b := readTestdata(t, filepath.Base(p))
			for _, e := range pdfProblems(b) {
				t.Error(e)
			}
		})
	}
}

var (
	startxrefRe = regexp.MustCompile(`startxref\r?\n(\d+)\r?\n%%EOF\r?\n?$`)
	xrefEntryRe = regexp.MustCompile(`^(\d{10}) (\d{5}) ([nf])\s*$`)
	streamRe    = regexp.MustCompile(`(?s)<<[^>]*/Length (\d+)[^>]*>>\r?\nstream\r?\n`)
)

func pdfProblems(b []byte) []string {
	var out []string
	m := startxrefRe.FindSubmatch(b)
	if m == nil {
		return []string{"no startxref at the end"}
	}
	off, _ := strconv.Atoi(string(m[1]))
	if off >= len(b) || !bytes.HasPrefix(b[off:], []byte("xref")) {
		return []string{fmt.Sprintf("startxref %d does not point at xref", off)}
	}
	lines := bytes.Split(b[off:], []byte("\n"))
	if len(lines) < 2 {
		return []string{"short xref table"}
	}
	var first, count int
	if _, err := fmt.Sscanf(string(lines[1]), "%d %d", &first, &count); err != nil {
		return []string{"bad xref subsection header: " + err.Error()}
	}
	for i := range count {
		if 2+i >= len(lines) {
			return append(out, "xref table shorter than its count")
		}
		e := xrefEntryRe.FindSubmatch(lines[2+i])
		if e == nil {
			out = append(out, fmt.Sprintf("bad xref entry %q", lines[2+i]))
			continue
		}
		if string(e[3]) != "n" {
			continue
		}
		at, _ := strconv.Atoi(string(e[1]))
		want := fmt.Sprintf("%d 0 obj", first+i)
		if at >= len(b) || !bytes.HasPrefix(b[at:], []byte(want)) {
			out = append(out, fmt.Sprintf("object %d: offset %d does not point at %q (want %d)", first+i, at, want, bytes.Index(b, []byte("\n"+want))+1))
		}
	}
	for _, s := range streamRe.FindAllSubmatchIndex(b, -1) {
		n, _ := strconv.Atoi(string(b[s[2]:s[3]]))
		data := b[s[1]:]
		end := bytes.Index(data, []byte("endstream"))
		got := bytes.TrimRight(data[:max(end, 0)], "\r\n")
		if end < 0 || len(got) != n {
			out = append(out, fmt.Sprintf("stream /Length %d, data is %d bytes", n, len(got)))
		}
	}
	return out
}
