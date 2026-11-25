package device

import (
	"slices"
	"strconv"
)

// Dropped is one wanted option value the printer does not offer. Value is the
// wanted value and Using the printer's default instead ("" when it reports
// none that it offers); ints are decimal strings.
type Dropped struct {
	Option string `json:"option"` // an Option* kind
	Value  string `json:"value"`
	Using  string `json:"using"`
}

// FitPrint drops every wanted value that caps does not list for its kind. An
// empty list means "not reported", so the value is dropped too: the printer
// cannot be asked for an option it does not report. The drops come in
// PrintSettings field order.
func FitPrint(want PrintSettings, caps PrinterCaps) (PrintSettings, []Dropped) {
	var dropped []Dropped
	str := func(option string, v *string, supported []string, def string) {
		if *v == "" || slices.Contains(supported, *v) {
			return
		}
		if !slices.Contains(supported, def) {
			def = ""
		}
		dropped = append(dropped, Dropped{Option: option, Value: *v, Using: def})
		*v = ""
	}
	num := func(option string, v *int, supported []int, def int) {
		if *v == 0 || slices.Contains(supported, *v) {
			return
		}
		using := ""
		if def != 0 && slices.Contains(supported, def) {
			using = strconv.Itoa(def)
		}
		dropped = append(dropped, Dropped{Option: option, Value: strconv.Itoa(*v), Using: using})
		*v = 0
	}
	str(OptionMedia, &want.Media, caps.Media, caps.MediaDefault)
	str(OptionMediaSource, &want.MediaSource, caps.MediaSources, caps.MediaSourceDefault)
	str(OptionMediaType, &want.MediaType, caps.MediaTypes, caps.MediaTypeDefault)
	str(OptionSides, &want.Sides, caps.Sides, caps.SidesDefault)
	str(OptionColorMode, &want.ColorMode, caps.ColorModes, caps.ColorModeDefault)
	str(OptionQuality, &want.Quality, caps.Qualities, caps.QualityDefault)
	num(OptionResolution, &want.Resolution, caps.Resolutions, caps.ResolutionDefault)
	str(OptionContentOptimize, &want.ContentOptimize, caps.ContentOptimize, caps.ContentOptimizeDefault)
	str(OptionOrientation, &want.Orientation, caps.Orientations, caps.OrientationDefault)
	num(OptionNumberUp, &want.NumberUp, caps.NumberUp, caps.NumberUpDefault)
	str(OptionPrintScaling, &want.PrintScaling, caps.PrintScalings, caps.PrintScalingDefault)
	str(OptionOutputBin, &want.OutputBin, caps.OutputBins, caps.OutputBinDefault)
	str(OptionFinishings, &want.Finishings, caps.Finishings, caps.FinishingsDefault)
	return want, dropped
}
