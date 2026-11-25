package scanning

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// xmlScanner is the fallback client on encoding/xml and net/http, selected
// with --escl-client=xml. Element names follow go-mfp and sane-airscan; the
// decoder matches local names only, whatever prefixes the scanner uses.
type xmlScanner struct {
	base *url.URL // eSCL root, e.g. http://host:8090/eSCL
	hc   *http.Client
}

// NewXML returns a Scanner for the eSCL root URL built on encoding/xml.
func NewXML(rawURL string) (Scanner, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("eSCL URL %q: not an http(s) URL", rawURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return &xmlScanner{base: u, hc: &http.Client{}}, nil
}

type xmlInputCaps struct {
	MaxWidth  int `xml:"MaxWidth"`
	MaxHeight int `xml:"MaxHeight"`
	// Scanners name SupportedIntents' children either way (go-mfp decodes both).
	Intents          []string `xml:"SupportedIntents>Intent"`
	SupportedIntents []string `xml:"SupportedIntents>SupportedIntent"`
	Profiles         []struct {
		ColorModes []string `xml:"ColorModes>ColorMode"`
		Res        []struct {
			Discrete []struct {
				X int `xml:"XResolution"`
				Y int `xml:"YResolution"`
			} `xml:"DiscreteResolutions>DiscreteResolution"`
			Range *struct {
				X xmlRange `xml:"XResolutionRange"`
				Y xmlRange `xml:"YResolutionRange"`
			} `xml:"ResolutionRange"`
		} `xml:"SupportedResolutions"`
	} `xml:"SettingProfiles>SettingProfile"`
}

type xmlRange struct {
	Min  int `xml:"Min"`
	Max  int `xml:"Max"`
	Step int `xml:"Step"`
}

type xmlCaps struct {
	Version string        `xml:"Version"`
	Platen  *xmlInputCaps `xml:"Platen>PlatenInputCaps"`
	Simplex *xmlInputCaps `xml:"Adf>AdfSimplexInputCaps"`
	Duplex  *xmlInputCaps `xml:"Adf>AdfDuplexInputCaps"`
}

// shortcut: profiles referenced from the root SettingProfiles by "ref" are not
// resolved; upgrade if a scanner that needs them turns up.
func (c *xmlCaps) inputs() []input {
	var inputs []input
	for _, src := range []struct {
		name string
		ic   *xmlInputCaps
	}{{"platen", c.Platen}, {"adf", c.Simplex}, {"adf-duplex", c.Duplex}} {
		if src.ic == nil {
			continue
		}
		in := input{source: src.name, maxW: src.ic.MaxWidth, maxH: src.ic.MaxHeight}
		for _, it := range slices.Concat(src.ic.Intents, src.ic.SupportedIntents) {
			in.addIntent(it)
		}
		for _, p := range src.ic.Profiles {
			in.modes = append(in.modes, p.ColorModes...)
			for _, sr := range p.Res {
				for _, d := range sr.Discrete {
					if d.X == d.Y {
						in.res = append(in.res, d.X)
					}
				}
				if r := sr.Range; r != nil {
					in.ranges = append(in.ranges, [2]rng{{r.X.Min, r.X.Max, r.X.Step}, {r.Y.Min, r.Y.Max, r.Y.Step}})
				}
			}
		}
		inputs = append(inputs, in)
	}
	return inputs
}

// url resolves an eSCL path: relative to the root, or absolute (job paths).
func (x *xmlScanner) url(path string) string {
	u := *x.base
	if strings.HasPrefix(path, "/") {
		u.Path = path
	} else {
		u.Path = x.base.Path + "/" + path
	}
	u.RawQuery = ""
	return u.String()
}

func (x *xmlScanner) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	rq, err := http.NewRequestWithContext(ctx, method, x.url(path), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		rq.Header.Set("Content-Type", "text/xml")
	}
	return x.hc.Do(rq)
}

// getXML GETs path and decodes the XML body into v.
func (x *xmlScanner) getXML(ctx context.Context, path string, v any) error {
	rsp, err := x.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("eSCL %s: %w", path, err)
	}
	defer func() { _ = rsp.Body.Close() }()
	if rsp.StatusCode/100 != 2 {
		return fmt.Errorf("eSCL %s: HTTP %s", path, rsp.Status)
	}
	if err := xml.NewDecoder(rsp.Body).Decode(v); err != nil {
		return fmt.Errorf("eSCL %s: %w", path, err)
	}
	return nil
}

func (x *xmlScanner) caps(ctx context.Context) (*xmlCaps, error) {
	var c xmlCaps
	if err := x.getXML(ctx, "ScannerCapabilities", &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (x *xmlScanner) Capabilities(ctx context.Context) (Caps, error) {
	c, err := x.caps(ctx)
	if err != nil {
		return Caps{}, err
	}
	return summarize(c.inputs()), nil
}

func (x *xmlScanner) Status(ctx context.Context) (State, error) {
	var st struct {
		State string `xml:"State"`
	}
	if err := x.getXML(ctx, "ScannerStatus", &st); err != nil {
		return StateUnknown, err
	}
	return stateOf(st.State), nil
}

func (x *xmlScanner) Start(ctx context.Context, s Settings) (string, error) {
	c, err := x.caps(ctx)
	if err != nil {
		return "", err
	}
	rq, err := newRequest(s, c.inputs())
	if err != nil {
		return "", err
	}
	version := c.Version
	if version == "" {
		version = "2.0"
	}
	var major, minor int
	_, _ = fmt.Sscanf(version, "%d.%d", &major, &minor)

	// Every interpolated value is a validated enum or an integer.
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScanSettings xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
<pwg:Version>%d.%d</pwg:Version>
%s<pwg:ScanRegions><pwg:ScanRegion><pwg:Height>%d</pwg:Height><pwg:ContentRegionUnits>escl:ThreeHundredthsOfInches</pwg:ContentRegionUnits><pwg:Width>%d</pwg:Width><pwg:XOffset>0</pwg:XOffset><pwg:YOffset>0</pwg:YOffset></pwg:ScanRegion></pwg:ScanRegions>
<pwg:DocumentFormat>image/jpeg</pwg:DocumentFormat>
`, major, minor, intentXML(rq.intent), rq.h, rq.w)
	if useFormatExt(major, minor) {
		b.WriteString("<scan:DocumentFormatExt>image/jpeg</scan:DocumentFormatExt>\n")
	}
	fmt.Fprintf(&b, "<pwg:InputSource>%s</pwg:InputSource>\n<scan:XResolution>%d</scan:XResolution>\n<scan:YResolution>%d</scan:YResolution>\n<scan:ColorMode>%s</scan:ColorMode>\n",
		rq.input, rq.res, rq.res, rq.colorMode)
	if rq.input == "Feeder" {
		fmt.Fprintf(&b, "<scan:Duplex>%t</scan:Duplex>\n", rq.duplex)
	}
	b.WriteString("</scan:ScanSettings>\n")

	rsp, err := x.do(ctx, http.MethodPost, "ScanJobs", strings.NewReader(b.String()))
	if err != nil {
		return "", fmt.Errorf("eSCL scan job: %w", err)
	}
	_, _ = io.Copy(io.Discard, rsp.Body)
	_ = rsp.Body.Close()
	if rsp.StatusCode/100 != 2 {
		return "", fmt.Errorf("eSCL scan job: HTTP %s", rsp.Status)
	}
	// Keep only the path: some scanners put unresolvable host names here.
	loc, err := url.Parse(rsp.Header.Get("Location"))
	if err != nil || !strings.Contains(loc.Path, "/ScanJobs/") {
		return "", fmt.Errorf("eSCL scan job: invalid Location %q", rsp.Header.Get("Location"))
	}
	return loc.Path, nil
}

// intentXML is the scan:Intent element for an eSCL intent, "" for none.
func intentXML(intent string) string {
	if intent == "" {
		return ""
	}
	return "<scan:Intent>" + intent + "</scan:Intent>\n"
}

func (x *xmlScanner) NextPage(ctx context.Context, job string) (io.ReadCloser, error) {
	rsp, err := x.do(ctx, http.MethodGet, job+"/NextDocument", nil)
	if err != nil {
		return nil, fmt.Errorf("eSCL next document: %w", err)
	}
	if rsp.StatusCode/100 == 2 {
		return rsp.Body, nil
	}
	_ = rsp.Body.Close()
	if rsp.StatusCode == http.StatusNotFound {
		return nil, io.EOF
	}
	return nil, fmt.Errorf("eSCL next document: HTTP %s", rsp.Status)
}

func (x *xmlScanner) Cancel(ctx context.Context, job string) error {
	rsp, err := x.do(ctx, http.MethodDelete, job, nil)
	if err != nil {
		return fmt.Errorf("eSCL cancel: %w", err)
	}
	_ = rsp.Body.Close()
	if rsp.StatusCode/100 != 2 && rsp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("eSCL cancel: HTTP %s", rsp.Status)
	}
	return nil
}
