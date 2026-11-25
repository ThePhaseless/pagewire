package ippcaps

import (
	"reflect"
	"slices"
	"testing"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func kw(name string, v ...string) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, s := range v {
		a.Values.Add(goipp.TagKeyword, goipp.String(s))
	}
	return a
}

func enums(name string, v ...int32) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, n := range v {
		a.Values.Add(goipp.TagEnum, goipp.Integer(n))
	}
	return a
}

func dpi(name string, v ...int) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, n := range v {
		a.Values.Add(goipp.TagResolution, goipp.Resolution{Xres: n, Yres: n, Units: goipp.UnitsDpi})
	}
	return a
}

func rng(name string, lo, hi int) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagRange, goipp.Range{Lower: lo, Upper: hi})
}

func integer(name string, v ...int32) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, n := range v {
		a.Values.Add(goipp.TagInteger, goipp.Integer(n))
	}
	return a
}

func mediaCol(name string, members ...goipp.Attribute) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagBeginCollection, goipp.Collection(members))
}

// empty is PrinterCaps with every list non-nil and empty.
func empty() device.PrinterCaps {
	return device.PrinterCaps{
		Media: []string{}, MediaSources: []string{}, MediaTypes: []string{}, Sides: []string{},
		ColorModes: []string{}, Qualities: []string{}, ContentOptimize: []string{}, Orientations: []string{},
		PrintScalings: []string{}, OutputBins: []string{}, Finishings: []string{}, Formats: []string{},
		Resolutions: []int{}, NumberUp: []int{},
	}
}

func TestParseSamsung(t *testing.T) {
	attrs := goipp.Attributes{
		kw("sides-supported", "one-sided"), kw("sides-default", "one-sided"),
		kw("print-color-mode-supported", "monochrome"), kw("print-color-mode-default", "monochrome"),
		enums("print-quality-supported", 3, 4, 5), enums("print-quality-default", 4),
		dpi("printer-resolution-supported", 300, 600), dpi("printer-resolution-default", 600),
		kw("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in"), kw("media-default", "na_letter_8.5x11in"),
	}
	want := empty()
	want.Sides, want.SidesDefault = []string{"one-sided"}, "one-sided"
	want.ColorModes, want.ColorModeDefault = []string{"monochrome"}, "monochrome"
	want.Qualities, want.QualityDefault = []string{"draft", "normal", "high"}, "normal"
	want.Resolutions, want.ResolutionDefault = []int{300, 600}, 600
	want.Media, want.MediaDefault = []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, "na_letter_8.5x11in"
	if got := Parse(attrs); !reflect.DeepEqual(got, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseEmpty(t *testing.T) {
	if got := Parse(nil); !reflect.DeepEqual(got, empty()) {
		t.Errorf("Parse(nil) = %+v", got)
	}
}

func TestParseHP(t *testing.T) {
	attrs := goipp.Attributes{
		kw("media-supported", "iso_a4_210x297mm", "na_letter_8.5x11in"), kw("media-default", "iso_a4_210x297mm"),
		kw("media-source-supported", "auto", "tray-1", "manual"),
		kw("media-type-supported", "stationery", "photographic-glossy"),
		mediaCol("media-col-default",
			kw("media-size-name", "iso_a4_210x297mm"), kw("media-source", "tray-1"), kw("media-type", "stationery")),
		kw("sides-supported", "one-sided", "two-sided-long-edge", "two-sided-short-edge"), kw("sides-default", "two-sided-long-edge"),
		kw("print-color-mode-supported", "auto", "color", "monochrome", "auto-monochrome"), kw("print-color-mode-default", "auto"),
		enums("print-quality-supported", 3, 4, 5), enums("print-quality-default", 5),
		dpi("printer-resolution-supported", 300, 600), dpi("printer-resolution-default", 300),
		kw("print-content-optimize-supported", "auto", "photo", "text"), kw("print-content-optimize-default", "auto"),
		enums("orientation-requested-supported", 3, 4, 7), enums("orientation-requested-default", 4),
		integer("number-up-supported", 1, 2, 4), integer("number-up-default", 1),
		kw("print-scaling-supported", "auto", "fit", "fill", "none"), kw("print-scaling-default", "auto"),
		kw("output-bin-supported", "face-down"), kw("output-bin-default", "face-down"),
		enums("finishings-supported", 3, 4, 74), enums("finishings-default", 3),
		rng("copies-supported", 1, 99), integer("copies-default", 1),
		goipp.MakeAttribute("document-format-supported", goipp.TagMimeType, goipp.String("application/pdf")),
	}
	want := device.PrinterCaps{
		Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaDefault: "iso_a4_210x297mm",
		MediaSources: []string{"auto", "tray-1", "manual"}, MediaSourceDefault: "tray-1",
		MediaTypes: []string{"stationery", "photographic-glossy"}, MediaTypeDefault: "stationery",
		Sides: []string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}, SidesDefault: "two-sided-long-edge",
		// Values outside pagewire's enums stay in the caps (A21); the UI filters them.
		ColorModes: []string{"auto", "color", "monochrome", "auto-monochrome"}, ColorModeDefault: "auto",
		Qualities: []string{"draft", "normal", "high"}, QualityDefault: "high",
		Resolutions: []int{300, 600}, ResolutionDefault: 300,
		ContentOptimize: []string{"auto", "photo", "text"}, ContentOptimizeDefault: "auto",
		Orientations: []string{"portrait", "landscape"}, OrientationDefault: "landscape", // 7 (none) dropped
		NumberUp: []int{1, 2, 4}, NumberUpDefault: 1,
		PrintScalings: []string{"auto", "fit", "fill", "none"}, PrintScalingDefault: "auto",
		OutputBins: []string{"face-down"}, OutputBinDefault: "face-down",
		Finishings: []string{"none", "staple", "punch-dual-left"}, FinishingsDefault: "none",
		CopiesMax: 99,
		Formats:   []string{"application/pdf"},
	}
	if got := Parse(attrs); !reflect.DeepEqual(got, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", got, want)
	}
}

func TestParseNumberUpRange(t *testing.T) {
	got := Parse(goipp.Attributes{rng("number-up-supported", 1, 4)})
	if !slices.Equal(got.NumberUp, []int{1, 2, 4}) {
		t.Errorf("NumberUp = %v", got.NumberUp)
	}
	got = Parse(goipp.Attributes{rng("number-up-supported", 1, 16)})
	if !slices.Equal(got.NumberUp, []int{1, 2, 4, 6, 9, 16}) {
		t.Errorf("NumberUp = %v", got.NumberUp)
	}
}

func TestParseResolution(t *testing.T) {
	a := goipp.Attribute{Name: "printer-resolution-supported"}
	a.Values.Add(goipp.TagResolution, goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi})
	a.Values.Add(goipp.TagResolution, goipp.Resolution{Xres: 600, Yres: 1200, Units: goipp.UnitsDpi}) // not square
	a.Values.Add(goipp.TagResolution, goipp.Resolution{Xres: 236, Yres: 236, Units: goipp.UnitsDpcm}) // 599.44 dpi
	got := Parse(goipp.Attributes{a})
	if !slices.Equal(got.Resolutions, []int{300, 599}) {
		t.Errorf("Resolutions = %v", got.Resolutions)
	}
}

func TestParseUnknownFinishingsDropped(t *testing.T) {
	got := Parse(goipp.Attributes{enums("finishings-supported", 3, 9999), enums("finishings-default", 9999)})
	if !slices.Equal(got.Finishings, []string{"none"}) || got.FinishingsDefault != "" {
		t.Errorf("Finishings = %v, default %q", got.Finishings, got.FinishingsDefault)
	}
}

func TestParseCopies(t *testing.T) {
	if got := Parse(goipp.Attributes{rng("copies-supported", 1, 999)}); got.CopiesMax != 999 {
		t.Errorf("CopiesMax = %d", got.CopiesMax)
	}
}

func find(attrs goipp.Attributes, name string) (goipp.Attribute, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a, true
		}
	}
	return goipp.Attribute{}, false
}

func names(attrs goipp.Attributes) []string {
	var out []string
	for _, a := range attrs {
		out = append(out, a.Name)
	}
	return out
}

func member(t *testing.T, col goipp.Attribute, name string) string {
	t.Helper()
	c, ok := col.Values[0].V.(goipp.Collection)
	if !ok {
		t.Fatalf("%s is %T, not a collection", col.Name, col.Values[0].V)
	}
	a, ok := find(goipp.Attributes(c), name)
	if !ok {
		return ""
	}
	return a.Values[0].V.String()
}

func TestJobMediaCol(t *testing.T) {
	o := apitypes.PrintOptions{PrintSettings: device.PrintSettings{
		Media: "iso_a4_210x297mm", MediaSource: "tray-1", MediaType: "stationery",
	}}
	attrs := Job(o)
	if _, ok := find(attrs, "media"); ok {
		t.Error("media sent beside media-col")
	}
	col, ok := find(attrs, "media-col")
	if !ok {
		t.Fatalf("no media-col in %v", names(attrs))
	}
	if col.Values[0].T != goipp.TagBeginCollection {
		t.Errorf("media-col tag %v", col.Values[0].T)
	}
	for k, v := range map[string]string{
		"media-size-name": "iso_a4_210x297mm", "media-source": "tray-1", "media-type": "stationery",
	} {
		if got := member(t, col, k); got != v {
			t.Errorf("media-col.%s = %q, want %q", k, got, v)
		}
	}
	if got := member(t, col, "media-size"); got != "" {
		t.Errorf("media-size sent: %q", got)
	}

	// Tray alone: no media-size-name.
	col, _ = find(Job(apitypes.PrintOptions{PrintSettings: device.PrintSettings{MediaSource: "manual"}}), "media-col")
	if got := member(t, col, "media-size-name"); got != "" {
		t.Errorf("media-size-name without media: %q", got)
	}
}

func TestJobMediaOnly(t *testing.T) {
	attrs := Job(apitypes.PrintOptions{PrintSettings: device.PrintSettings{Media: "na_letter_8.5x11in"}})
	if !slices.Equal(names(attrs), []string{"media"}) {
		t.Fatalf("attrs = %v", names(attrs))
	}
	if attrs[0].Values[0].T != goipp.TagKeyword || attrs[0].Values[0].V.String() != "na_letter_8.5x11in" {
		t.Errorf("media = %v", attrs[0].Values)
	}
}

func TestJobCopies(t *testing.T) {
	for _, n := range []int{0, 1} {
		if attrs := Job(apitypes.PrintOptions{Copies: n}); len(attrs) != 0 {
			t.Errorf("copies %d: attrs %v", n, names(attrs))
		}
	}
	a, ok := find(Job(apitypes.PrintOptions{Copies: 3}), "copies")
	if !ok || a.Values[0].V != goipp.Integer(3) || a.Values[0].T != goipp.TagInteger {
		t.Errorf("copies 3: %v", a.Values)
	}
}

func full() device.PrintSettings {
	return device.PrintSettings{
		Media: "iso_a4_210x297mm", MediaSource: "tray-1", MediaType: "stationery", Sides: "one-sided",
		ColorMode: "monochrome", Quality: "high", Resolution: 600, ContentOptimize: "text",
		Orientation: "landscape", NumberUp: 2, PrintScaling: "fit", OutputBin: "face-down", Finishings: "staple",
	}
}

func TestJobEveryKind(t *testing.T) {
	attrs := Job(apitypes.PrintOptions{Copies: 2, PrintSettings: full()})
	want := map[string]goipp.TaggedValue{
		"sides":                  {T: goipp.TagKeyword, V: goipp.String("one-sided")},
		"print-color-mode":       {T: goipp.TagKeyword, V: goipp.String("monochrome")},
		"print-quality":          {T: goipp.TagEnum, V: goipp.Integer(5)},
		"printer-resolution":     {T: goipp.TagResolution, V: goipp.Resolution{Xres: 600, Yres: 600, Units: goipp.UnitsDpi}},
		"print-content-optimize": {T: goipp.TagKeyword, V: goipp.String("text")},
		"orientation-requested":  {T: goipp.TagEnum, V: goipp.Integer(4)},
		"number-up":              {T: goipp.TagInteger, V: goipp.Integer(2)},
		"print-scaling":          {T: goipp.TagKeyword, V: goipp.String("fit")},
		"output-bin":             {T: goipp.TagKeyword, V: goipp.String("face-down")},
		"finishings":             {T: goipp.TagEnum, V: goipp.Integer(4)},
		"copies":                 {T: goipp.TagInteger, V: goipp.Integer(2)},
	}
	for name, tv := range want {
		a, ok := find(attrs, name)
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		if len(a.Values) != 1 || a.Values[0].T != tv.T || !reflect.DeepEqual(a.Values[0].V, tv.V) {
			t.Errorf("%s = %v, want %v", name, a.Values, tv)
		}
	}
	if _, ok := find(attrs, "media-col"); !ok {
		t.Error("no media-col")
	}
	if len(attrs) != len(want)+1 {
		t.Errorf("attrs = %v", names(attrs))
	}
	if attrs := Job(apitypes.PrintOptions{}); len(attrs) != 0 {
		t.Errorf("empty options send %v", names(attrs))
	}
}

func TestDefaults(t *testing.T) {
	attrs := Defaults(full())
	for _, a := range attrs {
		if !slices.Contains(PAPPLSettable, a.Name) {
			t.Errorf("%s is not settable on PAPPL", a.Name)
		}
		if len(a.Values) != 1 {
			t.Errorf("%s has %d values", a.Name, len(a.Values))
		}
	}
	for _, bad := range []string{"number-up-default", "print-scaling-default", "finishings-default", "media-default"} {
		if _, ok := find(attrs, bad); ok {
			t.Errorf("Defaults sent %s", bad)
		}
	}
	// PAPPL matches pattrs[] on name and value tag, so the tags must be exact.
	tags := map[string]goipp.Tag{
		"media-col-default":              goipp.TagBeginCollection,
		"sides-default":                  goipp.TagKeyword,
		"print-color-mode-default":       goipp.TagKeyword,
		"print-quality-default":          goipp.TagEnum,
		"printer-resolution-default":     goipp.TagResolution,
		"print-content-optimize-default": goipp.TagKeyword,
		"orientation-requested-default":  goipp.TagEnum,
		"output-bin-default":             goipp.TagKeyword,
	}
	for name, tag := range tags {
		a, ok := find(attrs, name)
		if !ok {
			t.Errorf("missing %s", name)
			continue
		}
		if a.Values[0].T != tag {
			t.Errorf("%s tag %v, want %v", name, a.Values[0].T, tag)
		}
	}
	if len(attrs) != len(tags) {
		t.Errorf("attrs = %v", names(attrs))
	}
	col, _ := find(attrs, "media-col-default")
	if member(t, col, "media-size-name") != "iso_a4_210x297mm" || member(t, col, "media-source") != "tray-1" || member(t, col, "media-type") != "stationery" {
		t.Errorf("media-col-default = %v", col.Values)
	}

	attrs = Defaults(device.PrintSettings{Media: "na_letter_8.5x11in", NumberUp: 2, PrintScaling: "fit", Finishings: "staple"})
	if !slices.Equal(names(attrs), []string{"media-default"}) {
		t.Errorf("media only: %v", names(attrs))
	}
	if len(Defaults(device.PrintSettings{})) != 0 {
		t.Error("empty defaults send attributes")
	}
}

func TestDefaultsReadBack(t *testing.T) {
	// What Defaults sends, parsed as the printer reports it, gives the same defaults.
	d := full()
	got := Parse(Defaults(d))
	if got.MediaDefault != "" || got.MediaSourceDefault != d.MediaSource || got.MediaTypeDefault != d.MediaType ||
		got.SidesDefault != d.Sides || got.ColorModeDefault != d.ColorMode || got.QualityDefault != d.Quality ||
		got.ResolutionDefault != d.Resolution || got.ContentOptimizeDefault != d.ContentOptimize ||
		got.OrientationDefault != d.Orientation || got.OutputBinDefault != d.OutputBin {
		t.Errorf("read back %+v", got)
	}
}

func TestFinishingsTable(t *testing.T) {
	if len(Finishings) != 70 {
		t.Errorf("%d finishings, want 70 (IANA registry)", len(Finishings))
	}
	for k, v := range Finishings {
		if got := Keyword(Finishings, v); got != k {
			t.Errorf("Keyword(%d) = %q, want %q", v, got, k)
		}
		a, _ := find(Job(apitypes.PrintOptions{PrintSettings: device.PrintSettings{Finishings: k}}), "finishings")
		if got := Parse(goipp.Attributes{goipp.Attribute{Name: "finishings-supported", Values: a.Values}}); !slices.Equal(got.Finishings, []string{k}) {
			t.Errorf("%s round trip = %v", k, got.Finishings)
		}
	}
	if Keyword(Finishings, 9999) != "" {
		t.Error("unknown enum has a keyword")
	}
}

func TestTables(t *testing.T) {
	for k, v := range Qualities {
		if Keyword(Qualities, v) != k {
			t.Errorf("quality %s", k)
		}
	}
	for k, v := range Orientations {
		if Keyword(Orientations, v) != k {
			t.Errorf("orientation %s", k)
		}
	}
}

func TestRequested(t *testing.T) {
	for _, n := range []string{
		"media-supported", "media-default", "media-source-supported", "media-type-supported", "media-col-default",
		"sides-supported", "sides-default", "print-color-mode-supported", "print-color-mode-default",
		"print-quality-supported", "print-quality-default", "printer-resolution-supported", "printer-resolution-default",
		"print-content-optimize-supported", "print-content-optimize-default", "orientation-requested-supported",
		"orientation-requested-default", "number-up-supported", "number-up-default", "print-scaling-supported",
		"print-scaling-default", "output-bin-supported", "output-bin-default", "finishings-supported",
		"finishings-default", "copies-supported", "copies-default", "document-format-supported",
	} {
		if !slices.Contains(Requested, n) {
			t.Errorf("Requested lacks %s", n)
		}
	}
}
