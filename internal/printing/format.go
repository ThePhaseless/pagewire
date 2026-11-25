package printing

import (
	"bytes"
	"fmt"
	"mime"
	"strings"
	"unicode/utf8"

	"github.com/ThePhaseless/printscan/internal/apitypes"
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

// Negotiate returns the format to send the printer: the detected one when
// the printer's document-format-supported lists it, else the format
// printscan converts it to (see Conversions), else *UnsupportedFormatError.
// Parameters such as charset are ignored. An unknown format is always
// refused, even when the printer lists application/octet-stream, since
// printscan cannot tell what the printer would make of it.
func Negotiate(detected string, supported []string) (string, error) {
	if detected != FormatUnknown && listed(detected, supported) {
		return detected, nil
	}
	if to := convertTo(detected, supported); to != "" {
		return to, nil
	}
	return "", &UnsupportedFormatError{Detected: detected, Supported: supported}
}

// Conversions lists the formats printscan converts for a printer with this
// document-format-supported list (what Negotiate converts).
func Conversions(supported []string) []apitypes.FormatConversion {
	out := []apitypes.FormatConversion{}
	for _, from := range []string{FormatPNG, FormatJPEG} {
		if to := convertTo(from, supported); to != "" && !listed(from, supported) {
			out = append(out, apitypes.FormatConversion{From: from, To: to})
		}
	}
	return out
}

// convertTo is the conversion target for an image the printer does not take
// as is: JPEG for a PNG when the printer takes JPEG, else a PDF. "" when none.
func convertTo(from string, supported []string) string {
	switch {
	case from == FormatPNG && listed(FormatJPEG, supported):
		return FormatJPEG
	case (from == FormatPNG || from == FormatJPEG) && listed(FormatPDF, supported):
		return FormatPDF
	}
	return ""
}

// listed reports whether supported holds format, ignoring case and parameters.
func listed(format string, supported []string) bool {
	for _, s := range supported {
		if t, _, err := mime.ParseMediaType(s); err == nil && t == format {
			return true
		}
	}
	return false
}
