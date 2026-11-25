// Package scanning talks eSCL to the scanner, keeps scan jobs on disk and
// assembles their PDF or ZIP downloads.
package scanning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/OpenPrinting/go-mfp/proto/escl"
	"github.com/OpenPrinting/go-mfp/transport"
	"github.com/OpenPrinting/go-mfp/util/optional"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
)

// Caps is the scanner capability summary of GET /scanners/{id}/capabilities.
type Caps = apitypes.ScannerCaps

// Settings is a scan request. Format only matters to the Service: pages are
// always fetched as JPEG.
type Settings = apitypes.ScanSettings

// State is the scanner state as Status reports it.
type State string

// Scanner states.
const (
	StateIdle       State = apitypes.StateIdle
	StateProcessing State = apitypes.StateProcessing
	StateStopped    State = apitypes.StateStopped
	StateUnknown    State = apitypes.StateUnknown
)

// Scanner is an eSCL scanner. Job strings are the URL paths of eSCL jobs.
type Scanner interface {
	Capabilities(ctx context.Context) (Caps, error)
	Status(ctx context.Context) (State, error)
	Start(ctx context.Context, s Settings) (job string, err error)
	// NextPage returns the next JPEG page, or io.EOF when the job has no more.
	NextPage(ctx context.Context, job string) (io.ReadCloser, error)
	// Cancel stops a job; a job that has already finished is not an error.
	Cancel(ctx context.Context, job string) error
}

// ErrInvalidSettings wraps every rejection of a setting value outside the API
// enums and ranges.
var ErrInvalidSettings = errors.New("invalid scan settings")

// Validate checks s against the API's enums and ranges.
func Validate(s Settings) error {
	if _, _, err := esclSource(s.Source); err != nil {
		return err
	}
	if _, err := esclColorMode(s.ColorMode); err != nil {
		return err
	}
	if s.Resolution < 75 || s.Resolution > 1200 {
		return fmt.Errorf("%w: resolution %d outside 75..1200", ErrInvalidSettings, s.Resolution)
	}
	if s.Format != "pdf" && s.Format != "jpeg" {
		return fmt.Errorf("%w: format %q", ErrInvalidSettings, s.Format)
	}
	if _, err := esclIntent(s.Intent); err != nil {
		return err
	}
	return nil
}

// scanIntents maps eSCL intents to the API's intent keywords, in eSCL order.
var scanIntents = []struct{ escl, api string }{
	{"Document", "document"},
	{"TextAndGraphic", "text-and-graphic"},
	{"Photo", "photo"},
	{"Preview", "preview"},
	{"Object", "object"},
	{"BusinessCard", "business-card"},
}

// esclIntent maps an API intent to the eSCL Intent; "" stays "" (no intent).
func esclIntent(intent string) (string, error) {
	if intent == "" {
		return "", nil
	}
	names := make([]string, len(scanIntents))
	for i, it := range scanIntents {
		if it.api == intent {
			return it.escl, nil
		}
		names[i] = it.api
	}
	return "", fmt.Errorf("%w: intent %q: must be one of %s", ErrInvalidSettings, intent, strings.Join(names, ", "))
}

// addIntent appends the API keyword of eSCL intent to in.intents once; an
// intent outside the API's set is dropped.
func (in *input) addIntent(intent string) {
	for _, it := range scanIntents {
		if it.escl == intent && !slices.Contains(in.intents, it.api) {
			in.intents = append(in.intents, it.api)
		}
	}
}

// esclSource maps an API source to the eSCL InputSource and Duplex values.
func esclSource(source string) (input string, duplex bool, err error) {
	switch source {
	case "platen":
		return "Platen", false, nil
	case "adf":
		return "Feeder", false, nil
	case "adf-duplex":
		return "Feeder", true, nil
	}
	return "", false, fmt.Errorf("%w: source %q", ErrInvalidSettings, source)
}

// esclColorMode maps an API colour mode to the eSCL ColorMode.
func esclColorMode(mode string) (string, error) {
	switch mode {
	case "color":
		return "RGB24", nil
	case "grayscale":
		return "Grayscale8", nil
	case "blackandwhite":
		return "BlackAndWhite1", nil
	}
	return "", fmt.Errorf("%w: color mode %q", ErrInvalidSettings, mode)
}

// apiColorMode maps an eSCL ColorMode to the API colour mode, "" if none.
func apiColorMode(mode string) string {
	switch mode {
	case "RGB24", "RGB48":
		return "color"
	case "Grayscale8", "Grayscale16":
		return "grayscale"
	case "BlackAndWhite1":
		return "blackandwhite"
	}
	return ""
}

// stateOf maps an eSCL scanner State to a Status state.
func stateOf(s string) State {
	switch s {
	case "Idle":
		return StateIdle
	case "Processing", "Testing":
		return StateProcessing
	case "Stopped", "Down":
		return StateStopped
	}
	return StateUnknown
}

// input is one eSCL input source as both clients read it from the
// capabilities.
type input struct {
	source     string // platen, adf or adf-duplex
	maxW, maxH int    // 1/300 inch
	modes      []string
	res        []int    // discrete square resolutions
	ranges     [][2]rng // X and Y resolution ranges
	intents    []string // API intent keywords, from SupportedIntents
}

type rng struct{ min, max, step int }

func (r rng) allows(v int) bool {
	return v >= r.min && v <= r.max && (r.step <= 0 || (v-r.min)%r.step == 0)
}

// commonResolutions are the values offered from a resolution range.
var commonResolutions = []int{75, 100, 150, 200, 240, 300, 400, 600, 1200}

// summarize builds the API capability summary from the inputs, limited to the
// API's 75..1200 dpi.
func summarize(inputs []input) Caps {
	c := Caps{Sources: []string{}, Resolutions: []int{}, ColorModes: []string{}, Intents: []string{}}
	for _, in := range inputs {
		c.Sources = append(c.Sources, in.source)
		c.ADF = c.ADF || in.source != "platen"
		c.Duplex = c.Duplex || in.source == "adf-duplex"
		for _, it := range in.intents {
			if !slices.Contains(c.Intents, it) {
				c.Intents = append(c.Intents, it)
			}
		}
		for _, m := range in.modes {
			if am := apiColorMode(m); am != "" && !slices.Contains(c.ColorModes, am) {
				c.ColorModes = append(c.ColorModes, am)
			}
		}
		res := slices.Clone(in.res)
		for _, r := range in.ranges {
			for _, v := range commonResolutions {
				if r[0].allows(v) && r[1].allows(v) {
					res = append(res, v)
				}
			}
		}
		for _, v := range res {
			if v >= 75 && v <= 1200 && !slices.Contains(c.Resolutions, v) {
				c.Resolutions = append(c.Resolutions, v)
			}
		}
	}
	slices.Sort(c.Resolutions)
	order := []string{"color", "grayscale", "blackandwhite"}
	slices.SortFunc(c.ColorModes, func(a, b string) int { return slices.Index(order, a) - slices.Index(order, b) })
	intentIndex := func(api string) int {
		return slices.IndexFunc(scanIntents, func(it struct{ escl, api string }) bool { return it.api == api })
	}
	slices.SortFunc(c.Intents, func(a, b string) int { return intentIndex(a) - intentIndex(b) })
	return c
}

// request is a scan request in eSCL terms.
type request struct {
	input, colorMode string
	intent           string // eSCL Intent, "" for none
	duplex           bool
	res, w, h        int
}

// newRequest maps s to eSCL values for one of the scanner's inputs, scanning
// the input's full area (some scanners refuse jobs without a ScanRegion).
func newRequest(s Settings, inputs []input) (request, error) {
	in, duplex, err := esclSource(s.Source)
	if err != nil {
		return request{}, err
	}
	mode, err := esclColorMode(s.ColorMode)
	if err != nil {
		return request{}, err
	}
	intent, err := esclIntent(s.Intent)
	if err != nil {
		return request{}, err
	}
	i := slices.IndexFunc(inputs, func(in input) bool { return in.source == s.Source })
	if i < 0 {
		return request{}, fmt.Errorf("scanner has no %s input", s.Source)
	}
	if intent != "" && !slices.Contains(inputs[i].intents, s.Intent) {
		return request{}, fmt.Errorf("scanner has no %s intent for %s", s.Intent, s.Source)
	}
	return request{input: in, colorMode: mode, intent: intent, duplex: duplex, res: s.Resolution, w: inputs[i].maxW, h: inputs[i].maxH}, nil
}

// useFormatExt reports whether the scanner's eSCL version (2.1 and later)
// takes DocumentFormatExt.
func useFormatExt(major, minor int) bool { return major > 2 || (major == 2 && minor >= 1) }

// mfpScanner is the primary client, on go-mfp's eSCL client.
type mfpScanner struct{ c *escl.Client }

// NewMFP returns a Scanner for the eSCL root URL (e.g. http://host:8090/eSCL)
// built on go-mfp.
func NewMFP(url string) (Scanner, error) {
	u, err := transport.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("eSCL URL %q: %w", url, err)
	}
	return &mfpScanner{c: escl.NewClient(u, nil)}, nil
}

func (m *mfpScanner) caps(ctx context.Context) (*escl.ScannerCapabilities, []input, error) {
	caps, _, err := m.c.GetScannerCapabilities(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("eSCL capabilities: %w", err)
	}
	var inputs []input
	add := func(source string, ic *escl.InputSourceCaps) {
		if ic == nil {
			return
		}
		in := input{source: source, maxW: ic.MaxWidth, maxH: ic.MaxHeight}
		for _, it := range ic.SupportedIntents {
			in.addIntent(it.String())
		}
		for _, p := range ic.SettingProfiles {
			for _, cm := range p.ColorModes {
				in.modes = append(in.modes, cm.String())
			}
			for _, sr := range p.SupportedResolutions {
				for _, d := range sr.DiscreteResolutions {
					if d.XResolution == d.YResolution {
						in.res = append(in.res, d.XResolution)
					}
				}
				if r := sr.ResolutionRange; r != nil {
					x, y := r.XResolutionRange, r.YResolutionRange
					in.ranges = append(in.ranges, [2]rng{{x.Min, x.Max, step(x.Step)}, {y.Min, y.Max, step(y.Step)}})
				}
			}
		}
		inputs = append(inputs, in)
	}
	if caps.Platen != nil {
		add("platen", caps.Platen.PlatenInputCaps)
	}
	if caps.ADF != nil {
		add("adf", caps.ADF.ADFSimplexInputCaps)
		add("adf-duplex", caps.ADF.ADFDuplexInputCaps)
	}
	return caps, inputs, nil
}

func (m *mfpScanner) Capabilities(ctx context.Context) (Caps, error) {
	_, inputs, err := m.caps(ctx)
	if err != nil {
		return Caps{}, err
	}
	return summarize(inputs), nil
}

func (m *mfpScanner) Status(ctx context.Context) (State, error) {
	st, _, err := m.c.GetScannerStatus(ctx)
	if err != nil {
		return StateUnknown, fmt.Errorf("eSCL status: %w", err)
	}
	return stateOf(st.State.String()), nil
}

func (m *mfpScanner) Start(ctx context.Context, s Settings) (string, error) {
	caps, inputs, err := m.caps(ctx)
	if err != nil {
		return "", err
	}
	rq, err := newRequest(s, inputs)
	if err != nil {
		return "", err
	}
	ss := escl.ScanSettings{
		Version:        caps.Version,
		ScanRegions:    []escl.ScanRegion{{Width: rq.w, Height: rq.h, ContentRegionUnits: escl.ThreeHundredthsOfInches}},
		DocumentFormat: optional.New("image/jpeg"),
		InputSource:    optional.New(escl.InputPlaten),
		XResolution:    optional.New(rq.res),
		YResolution:    optional.New(rq.res),
		ColorMode:      optional.New(colorModes[rq.colorMode]),
	}
	if rq.intent != "" {
		ss.Intent = optional.New(escl.DecodeIntent(rq.intent))
	}
	if useFormatExt(caps.Version.Major(), caps.Version.Minor()) {
		ss.DocumentFormatExt = optional.New("image/jpeg")
	}
	if rq.input == "Feeder" {
		ss.InputSource = optional.New(escl.InputFeeder)
		ss.Duplex = optional.New(rq.duplex)
	}
	job, _, err := m.c.Scan(ctx, ss)
	if err != nil {
		return "", fmt.Errorf("eSCL scan job: %w", err)
	}
	return job, nil
}

// step reads an optional range step; 0 means any value.
func step(s *int) int {
	if s == nil {
		return 0
	}
	return *s
}

var colorModes = map[string]escl.ColorMode{
	"RGB24":          escl.RGB24,
	"Grayscale8":     escl.Grayscale8,
	"BlackAndWhite1": escl.BlackAndWhite1,
}

func (m *mfpScanner) NextPage(ctx context.Context, job string) (io.ReadCloser, error) {
	doc, _, err := m.c.NextDocument(ctx, job)
	if errors.Is(err, io.EOF) {
		if doc != nil {
			_ = doc.Close()
		}
		return nil, io.EOF
	}
	if err != nil {
		return nil, fmt.Errorf("eSCL next document: %w", err)
	}
	return doc, nil
}

func (m *mfpScanner) Cancel(ctx context.Context, job string) error {
	if _, err := m.c.Cancel(ctx, job); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("eSCL cancel: %w", err)
	}
	return nil
}
