package printing

import (
	"bytes"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"
)

// Document formats as IPP document-format MIME types.
const (
	FormatPDF        = "application/pdf"
	FormatPostScript = "application/postscript"
	FormatJPEG       = "image/jpeg"
	FormatPNG        = "image/png"
	FormatPWGRaster  = "image/pwg-raster" //nolint:gosec // G101 false positive: a MIME type
	FormatURF        = "image/urf"
	FormatText       = "text/plain"
	FormatUnknown    = "application/octet-stream"
)

var magics = []struct {
	prefix string
	format string
}{
	{"%PDF-", FormatPDF},
	{"%!", FormatPostScript},
	{"\xff\xd8\xff", FormatJPEG},
	{"\x89PNG\r\n\x1a\n", FormatPNG},
	{"RaS2", FormatPWGRaster},
	{"UNIRAST", FormatURF},
}

// Detect returns the document format of a file from its first bytes (512 are
// enough), else FormatUnknown. Only the content decides: the filename is
// ignored, because an extension can only mislabel bytes that failed every
// signature check, and mime.TypeByExtension varies with the host's mime.types.
func Detect(head []byte, _ string) string {
	for _, m := range magics {
		if bytes.HasPrefix(head, []byte(m.prefix)) {
			return m.format
		}
	}
	if len(head) > 0 && isText(head) {
		return FormatText
	}
	return FormatUnknown
}

// isText reports whether b is UTF-8 without control characters other than
// tab, newline, form feed and carriage return. A rune cut off at the end is allowed.
func isText(b []byte) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n <= 1 {
			return !utf8.FullRune(b)
		}
		if r < 0x20 && r != '\t' && r != '\n' && r != '\f' && r != '\r' || r == 0x7f {
			return false
		}
		b = b[n:]
	}
	return true
}

// UnsupportedFormatError is returned by Negotiate when the printer does not
// list the detected format in document-format-supported.
type UnsupportedFormatError struct {
	Detected  string
	Supported []string
}

func (e *UnsupportedFormatError) Error() string {
	return fmt.Sprintf("document format %s is not supported by the printer (accepted: %s)",
		e.Detected, strings.Join(e.Supported, ", "))
}

// Negotiate checks a detected format against the printer's
// document-format-supported list (decision 10: no conversion). Parameters
// such as charset are ignored. An unknown format is always refused, even when
// the printer lists application/octet-stream, since printscan cannot tell what
// the printer would make of it.
func Negotiate(detected string, supported []string) error {
	if detected != FormatUnknown {
		for _, s := range supported {
			if t, _, err := mime.ParseMediaType(s); err == nil && t == detected {
				return nil
			}
		}
	}
	return &UnsupportedFormatError{Detected: detected, Supported: supported}
}
