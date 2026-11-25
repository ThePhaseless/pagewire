// Package ippcaps is the one codec between IPP printer attributes and
// pagewire's print option kinds (A21): it parses -supported/-default
// attributes into device.PrinterCaps, and encodes chosen options as job
// attributes (Job) and as the PAPPL queue defaults PAPPL can set (Defaults).
package ippcaps

import (
	"math"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// Requested is every attribute Parse reads, for Get-Printer-Attributes'
// requested-attributes.
var Requested = []string{
	"media-supported", "media-default",
	"media-source-supported", "media-type-supported", "media-col-default",
	"sides-supported", "sides-default",
	"print-color-mode-supported", "print-color-mode-default",
	"print-quality-supported", "print-quality-default",
	"printer-resolution-supported", "printer-resolution-default",
	"print-content-optimize-supported", "print-content-optimize-default",
	"orientation-requested-supported", "orientation-requested-default",
	"number-up-supported", "number-up-default",
	"print-scaling-supported", "print-scaling-default",
	"output-bin-supported", "output-bin-default",
	"finishings-supported", "finishings-default",
	"copies-supported", "copies-default",
	"document-format-supported",
}

// PAPPLSettable is every option default Defaults may send, all of which PAPPL's
// Set-Printer-Attributes accepts (papplfake refuses the rest, as PAPPL does).
//
// Confirmed against pappl/printer-ipp.c `_papplPrinterSetAttributes` at
// PAPPL_COMMIT 6db8e137557ad84662e78d24fdb2a591c621f4ac (v1.4.12, the pin in
// images/driver/Dockerfile): of the option defaults, its pattrs[] table lists
// exactly these (beside media-ready, media-col-ready and label, darkness,
// speed, contact, location and Wi-Fi attributes), matched on name and value
// tag. print-scaling-default has a handler branch but no pattrs[] row, so it is
// answered as unsupported and the whole request fails: not settable. No finishings-,
// number-up- or copies-default. `_papplMediaColImport` copies only the members
// present onto the current media default, so a media-col-default holding only
// media-source changes only the tray.
var PAPPLSettable = []string{
	"media-default",
	"media-col-default",
	"orientation-requested-default",
	"output-bin-default",
	"print-color-mode-default",
	"print-content-optimize-default",
	"print-quality-default",
	"printer-resolution-default",
	"sides-default",
}

// Qualities maps print-quality keywords to their IPP enum values.
var Qualities = map[string]goipp.Integer{"draft": 3, "normal": 4, "high": 5}

// Orientations maps orientation-requested keywords to their IPP enum values.
// 7 (none) is left out on purpose: it is not a choice.
var Orientations = map[string]goipp.Integer{
	"portrait": 3, "landscape": 4, "reverse-landscape": 5, "reverse-portrait": 6,
}

// Keyword returns the keyword of v in table, or "" when v is not in it.
func Keyword(table map[string]goipp.Integer, v goipp.Integer) string {
	for k, n := range table {
		if n == v {
			return k
		}
	}
	return ""
}

// numberUps are the pages-per-sheet values a number-up-supported range expands to.
var numberUps = []int{1, 2, 4, 6, 9, 16}

// Parse reads the printer capabilities from Get-Printer-Attributes. Missing
// attributes give empty lists and "" or 0 defaults; no list is nil. Keyword
// values are kept as reported, even outside pagewire's enums.
func Parse(attrs goipp.Attributes) device.PrinterCaps {
	get := func(name string) goipp.Values {
		for _, a := range attrs {
			if a.Name == name {
				return a.Values
			}
		}
		return nil
	}
	c := device.PrinterCaps{
		Media:           strs(get("media-supported")),
		MediaSources:    strs(get("media-source-supported")),
		MediaTypes:      strs(get("media-type-supported")),
		Sides:           strs(get("sides-supported")),
		ColorModes:      strs(get("print-color-mode-supported")),
		Qualities:       mapped(get("print-quality-supported"), Qualities),
		ContentOptimize: strs(get("print-content-optimize-supported")),
		Orientations:    mapped(get("orientation-requested-supported"), Orientations),
		PrintScalings:   strs(get("print-scaling-supported")),
		OutputBins:      strs(get("output-bin-supported")),
		Finishings:      mapped(get("finishings-supported"), Finishings),
		Formats:         strs(get("document-format-supported")),
		Resolutions:     resolutions(get("printer-resolution-supported")),
		NumberUp:        numberUp(get("number-up-supported")),

		MediaDefault:           first(strs(get("media-default"))),
		SidesDefault:           first(strs(get("sides-default"))),
		ColorModeDefault:       first(strs(get("print-color-mode-default"))),
		QualityDefault:         first(mapped(get("print-quality-default"), Qualities)),
		ContentOptimizeDefault: first(strs(get("print-content-optimize-default"))),
		OrientationDefault:     first(mapped(get("orientation-requested-default"), Orientations)),
		PrintScalingDefault:    first(strs(get("print-scaling-default"))),
		OutputBinDefault:       first(strs(get("output-bin-default"))),
		FinishingsDefault:      first(mapped(get("finishings-default"), Finishings)),
	}
	if r := resolutions(get("printer-resolution-default")); len(r) > 0 {
		c.ResolutionDefault = r[0]
	}
	if n := numberUp(get("number-up-default")); len(n) > 0 {
		c.NumberUpDefault = n[0]
	}
	for _, v := range get("copies-supported") {
		if r, ok := v.V.(goipp.Range); ok {
			c.CopiesMax = r.Upper
		}
	}
	for _, v := range get("media-col-default") {
		if col, ok := v.V.(goipp.Collection); ok {
			for _, m := range col {
				switch m.Name {
				case "media-source":
					c.MediaSourceDefault = first(strs(m.Values))
				case "media-type":
					c.MediaTypeDefault = first(strs(m.Values))
				}
			}
		}
	}
	return c
}

// Job returns the job-template attributes for every set option (A21's Submit
// column). A tray or media type sends media-col (with media-size-name when media
// is set, never media-size) instead of media; copies is sent only above 1.
func Job(o apitypes.PrintOptions) goipp.Attributes {
	var out goipp.Attributes
	if n, ok := ippInt(o.Copies); ok && n > 1 {
		out.Add(goipp.MakeAttribute("copies", goipp.TagInteger, n))
	}
	out = append(out, encode(o.PrintSettings, false)...)
	return out
}

// Defaults returns the PAPPL -default attributes for every set option PAPPL can
// set (PAPPLSettable); number-up, print-scaling and finishings are skipped.
func Defaults(d device.PrintSettings) goipp.Attributes {
	return encode(d, true)
}

func encode(s device.PrintSettings, defaults bool) goipp.Attributes {
	var out goipp.Attributes
	name := func(n string) string {
		if defaults {
			return n + "-default"
		}
		return n
	}
	keyword := func(n, v string) {
		if v != "" {
			out.Add(goipp.MakeAttribute(name(n), goipp.TagKeyword, goipp.String(v)))
		}
	}
	enum := func(n, v string, table map[string]goipp.Integer) {
		if e, ok := table[v]; ok {
			out.Add(goipp.MakeAttribute(name(n), goipp.TagEnum, e))
		}
	}

	if s.MediaSource != "" || s.MediaType != "" {
		var col goipp.Collection
		if s.Media != "" {
			col.Add(goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(s.Media)))
		}
		if s.MediaSource != "" {
			col.Add(goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(s.MediaSource)))
		}
		if s.MediaType != "" {
			col.Add(goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(s.MediaType)))
		}
		out.Add(goipp.MakeAttribute(name("media-col"), goipp.TagBeginCollection, col))
	} else {
		keyword("media", s.Media)
	}
	keyword("sides", s.Sides)
	keyword("print-color-mode", s.ColorMode)
	enum("print-quality", s.Quality, Qualities)
	if s.Resolution > 0 {
		out.Add(goipp.MakeAttribute(name("printer-resolution"), goipp.TagResolution,
			goipp.Resolution{Xres: s.Resolution, Yres: s.Resolution, Units: goipp.UnitsDpi}))
	}
	keyword("print-content-optimize", s.ContentOptimize)
	enum("orientation-requested", s.Orientation, Orientations)
	if !defaults { // not settable on PAPPL
		if n, ok := ippInt(s.NumberUp); ok {
			out.Add(goipp.MakeAttribute("number-up", goipp.TagInteger, n))
		}
		keyword("print-scaling", s.PrintScaling)
	}
	keyword("output-bin", s.OutputBin)
	if !defaults { // not settable on PAPPL
		// shortcut: one finishing per job (no staple plus punch together); upgrade to a multi-select when a device with several finishings is in use.
		enum("finishings", s.Finishings, Finishings)
	}
	return out
}

// strs returns the string values (keyword, name, text, mime type) of vs.
func strs(vs goipp.Values) []string {
	out := []string{}
	for _, v := range vs {
		switch s := v.V.(type) {
		case goipp.String:
			out = append(out, string(s))
		case goipp.TextWithLang:
			out = append(out, s.Text)
		}
	}
	return out
}

// mapped returns the keywords of the enum values in vs; unknown values are dropped.
func mapped(vs goipp.Values, table map[string]goipp.Integer) []string {
	out := []string{}
	for _, v := range vs {
		if n, ok := v.V.(goipp.Integer); ok {
			if k := Keyword(table, n); k != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// resolutions returns the square resolutions in vs in dpi (dots per cm
// converted and rounded); non-square ones are dropped.
func resolutions(vs goipp.Values) []int {
	out := []int{}
	for _, v := range vs {
		r, ok := v.V.(goipp.Resolution)
		if !ok || r.Xres != r.Yres || r.Xres <= 0 {
			continue
		}
		switch r.Units {
		case goipp.UnitsDpi:
			out = append(out, r.Xres)
		case goipp.UnitsDpcm:
			out = append(out, int(math.Round(float64(r.Xres)*2.54)))
		}
	}
	return out
}

// numberUp returns the integers in vs, with a range expanded to the usual
// pages-per-sheet values inside it.
func numberUp(vs goipp.Values) []int {
	out := []int{}
	for _, v := range vs {
		switch n := v.V.(type) {
		case goipp.Integer:
			out = append(out, int(n))
		case goipp.Range:
			for _, u := range numberUps {
				if u >= n.Lower && u <= n.Upper {
					out = append(out, u)
				}
			}
		}
	}
	return out
}

func first(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}

// ippInt converts a positive int to an IPP integer, refusing values outside
// 1..MaxInt32 that would otherwise wrap around.
func ippInt(n int) (goipp.Integer, bool) {
	if n <= 0 || n > math.MaxInt32 {
		return 0, false
	}
	return goipp.Integer(n), true
}
