package scanning

import (
	"bytes"
	"context"
	"errors"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/go-mfp/abstract"
	"github.com/OpenPrinting/go-mfp/proto/escl"
	"github.com/OpenPrinting/go-mfp/transport"
	"github.com/OpenPrinting/go-mfp/util/generic"
)

// The virtual scanner's images are A4 at 75 dpi, so a 75 dpi scan needs no
// resampling.
const (
	a4Width75  = 620
	a4Height75 = 877
)

// virtualScanner serves go-mfp's AbstractServer over a VirtualScanner with a
// platen and, when adf is set, a three-page ADF (simplex and duplex). last
// returns the ScanSettings of the latest ScanJobs request as the server decoded it.
func virtualScanner(t *testing.T, adf bool) (url string, last func() *escl.ScanSettings) {
	t.Helper()
	input := &abstract.InputCapabilities{
		MaxWidth:  abstract.A4Width,
		MaxHeight: abstract.A4Height,
		Profiles: []abstract.SettingsProfile{{
			ColorModes:  generic.MakeBitset(abstract.ColorModeColor, abstract.ColorModeMono),
			Depths:      generic.MakeBitset(abstract.ColorDepth8),
			Resolutions: []abstract.Resolution{{XResolution: 75, YResolution: 75}, {XResolution: 150, YResolution: 150}},
		}},
		Intents: generic.MakeBitset(abstract.IntentDocument, abstract.IntentPhoto),
	}
	caps := &abstract.ScannerCapabilities{
		MakeAndModel:    "Virtual scanner",
		DocumentFormats: []string{"image/jpeg"},
		Platen:          input,
	}
	page := jpegBytes(t, a4Width75, a4Height75, false)
	vs := &abstract.VirtualScanner{
		ScanCaps:    caps,
		Resolution:  abstract.Resolution{XResolution: 75, YResolution: 75},
		PlatenImage: page,
	}
	if adf {
		caps.ADFSimplex, caps.ADFDuplex, caps.ADFCapacity = input, input, 50
		vs.ADFImages = [][]byte{page, page, page}
	}
	var mu sync.Mutex
	var got *escl.ScanSettings
	hooks := escl.ServerHooks{OnScanJobsRequest: func(_ *transport.ServerQuery, ss *escl.ScanSettings) *escl.ScanSettings {
		mu.Lock()
		got = ss
		mu.Unlock()
		return nil
	}}
	srv := httptest.NewServer(escl.NewAbstractServer(escl.AbstractServerOptions{Scanner: vs, BasePath: "/eSCL", Hooks: hooks}))
	t.Cleanup(srv.Close)
	return srv.URL + "/eSCL", func() *escl.ScanSettings { mu.Lock(); defer mu.Unlock(); return got }
}

var clients = map[string]func(url string) (Scanner, error){
	"mfp": NewMFP,
	"xml": NewXML,
}

// forEachClient runs f once per client against a fresh virtual scanner each.
func forEachClient(t *testing.T, adf bool, f func(t *testing.T, s Scanner)) {
	for name, newClient := range clients {
		t.Run(name, func(t *testing.T) {
			url, _ := virtualScanner(t, adf)
			s, err := newClient(url)
			if err != nil {
				t.Fatal(err)
			}
			f(t, s)
		})
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// readPages reads pages until io.EOF and checks each one decodes as JPEG.
func readPages(t *testing.T, s Scanner, job string) int {
	t.Helper()
	n := 0
	for {
		body, err := s.NextPage(ctx(t), job)
		if errors.Is(err, io.EOF) {
			return n
		}
		if err != nil {
			t.Fatalf("NextPage %d: %v", n+1, err)
		}
		b, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("page %d is not a JPEG: %v", n+1, err)
		}
		if cfg.Width == 0 || cfg.Height == 0 {
			t.Fatalf("page %d is empty", n+1)
		}
		n++
	}
}

func settings(source string, res int, mode string) Settings {
	return Settings{Source: source, Resolution: res, ColorMode: mode, Format: "jpeg"}
}

func TestConformanceCaps(t *testing.T) {
	forEachClient(t, true, func(t *testing.T, s Scanner) {
		got, err := s.Capabilities(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		want := Caps{
			Sources:     []string{"platen", "adf", "adf-duplex"},
			Resolutions: []int{75, 150},
			ColorModes:  []string{"color", "grayscale"},
			ADF:         true,
			Duplex:      true,
			Intents:     []string{"document", "photo"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("caps = %+v, want %+v", got, want)
		}
		st, err := s.Status(ctx(t))
		if err != nil || st != StateIdle {
			t.Fatalf("status = %q, %v; want idle", st, err)
		}
	})
	forEachClient(t, false, func(t *testing.T, s Scanner) {
		got, err := s.Capabilities(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Sources, []string{"platen"}) || got.ADF || got.Duplex {
			t.Fatalf("platen-only caps = %+v", got)
		}
	})
}

func TestConformanceSinglePage(t *testing.T) {
	forEachClient(t, true, func(t *testing.T, s Scanner) {
		job, err := s.Start(ctx(t), settings("platen", 75, "color"))
		if err != nil {
			t.Fatal(err)
		}
		body, err := s.NextPage(ctx(t), job)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := jpeg.DecodeConfig(body)
		_ = body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Width != a4Width75 || cfg.Height != a4Height75 {
			t.Errorf("page is %dx%d, want %dx%d", cfg.Width, cfg.Height, a4Width75, a4Height75)
		}
		if _, err := s.NextPage(ctx(t), job); !errors.Is(err, io.EOF) {
			t.Fatalf("second NextPage = %v, want io.EOF", err)
		}
		if st, _ := s.Status(ctx(t)); st != StateIdle {
			t.Errorf("status after the job = %q, want idle", st)
		}
	})
}

func TestConformanceADF(t *testing.T) {
	for _, source := range []string{"adf", "adf-duplex"} {
		t.Run(source, func(t *testing.T) {
			forEachClient(t, true, func(t *testing.T, s Scanner) {
				job, err := s.Start(ctx(t), settings(source, 75, "grayscale"))
				if err != nil {
					t.Fatal(err)
				}
				if n := readPages(t, s, job); n != 3 {
					t.Fatalf("got %d pages, want 3", n)
				}
			})
		})
	}
}

func TestConformanceCancel(t *testing.T) {
	forEachClient(t, true, func(t *testing.T, s Scanner) {
		job, err := s.Start(ctx(t), settings("adf", 75, "color"))
		if err != nil {
			t.Fatal(err)
		}
		body, err := s.NextPage(ctx(t), job)
		if err != nil {
			t.Fatal(err)
		}
		_ = body.Close()
		if err := s.Cancel(ctx(t), job); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if err := s.Cancel(ctx(t), job); err != nil {
			t.Fatalf("Cancel of a finished job: %v", err)
		}
		if st, _ := s.Status(ctx(t)); st != StateIdle {
			t.Errorf("status after cancel = %q, want idle", st)
		}
		// The scanner is free again.
		job, err = s.Start(ctx(t), settings("platen", 75, "color"))
		if err != nil {
			t.Fatalf("Start after cancel: %v", err)
		}
		if n := readPages(t, s, job); n != 1 {
			t.Fatalf("got %d pages, want 1", n)
		}
	})
}

func TestConformanceBadSettings(t *testing.T) {
	forEachClient(t, false, func(t *testing.T, s Scanner) {
		for name, set := range map[string]Settings{
			"unsupported resolution": settings("platen", 600, "color"),
			"source not present":     settings("adf", 75, "color"),
			"unsupported color mode": settings("platen", 75, "blackandwhite"),
		} {
			if _, err := s.Start(ctx(t), set); err == nil {
				t.Errorf("%s: Start succeeded", name)
			}
		}
		for name, set := range map[string]Settings{
			"unknown source":     settings("camera", 75, "color"),
			"unknown color mode": settings("platen", 75, "sepia"),
		} {
			if _, err := s.Start(ctx(t), set); !errors.Is(err, ErrInvalidSettings) {
				t.Errorf("%s: err = %v, want ErrInvalidSettings", name, err)
			}
		}
		// None of the refusals left a job behind.
		job, err := s.Start(ctx(t), settings("platen", 75, "color"))
		if err != nil {
			t.Fatalf("Start after refusals: %v", err)
		}
		readPages(t, s, job)
	})
}

// Both clients put the same request on the wire: input, duplex, resolution,
// colour mode, JPEG format (also as DocumentFormatExt on eSCL 2.1+) and the
// full scan region of the input.
func TestConformanceWire(t *testing.T) {
	for name, newClient := range clients {
		t.Run(name, func(t *testing.T) {
			url, last := virtualScanner(t, true)
			s, err := newClient(url)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				set    Settings
				input  escl.InputSource
				duplex *bool
				mode   escl.ColorMode
			}{
				{settings("platen", 75, "color"), escl.InputPlaten, nil, escl.RGB24},
				{settings("adf", 150, "grayscale"), escl.InputFeeder, ptrBool(false), escl.Grayscale8},
				{settings("adf-duplex", 75, "color"), escl.InputFeeder, ptrBool(true), escl.RGB24},
			} {
				job, err := s.Start(ctx(t), tc.set)
				if err != nil {
					t.Fatal(err)
				}
				ss := last()
				if err := s.Cancel(ctx(t), job); err != nil {
					t.Fatal(err)
				}
				if ss.InputSource == nil || *ss.InputSource != tc.input {
					t.Errorf("%s: InputSource %v, want %v", tc.set.Source, ss.InputSource, tc.input)
				}
				if d := (*bool)(ss.Duplex); (d == nil) != (tc.duplex == nil) || (d != nil && *d != *tc.duplex) {
					t.Errorf("%s: Duplex %v, want %v", tc.set.Source, d, tc.duplex)
				}
				if ss.ColorMode == nil || *ss.ColorMode != tc.mode {
					t.Errorf("%s: ColorMode %v, want %v", tc.set.Source, ss.ColorMode, tc.mode)
				}
				if ss.XResolution == nil || *ss.XResolution != tc.set.Resolution || ss.YResolution == nil || *ss.YResolution != tc.set.Resolution {
					t.Errorf("%s: resolution %v x %v, want %d", tc.set.Source, ss.XResolution, ss.YResolution, tc.set.Resolution)
				}
				if ss.DocumentFormat == nil || *ss.DocumentFormat != "image/jpeg" || ss.DocumentFormatExt == nil || *ss.DocumentFormatExt != "image/jpeg" {
					t.Errorf("%s: formats %v / %v, want image/jpeg for both", tc.set.Source, ss.DocumentFormat, ss.DocumentFormatExt)
				}
				// A4 in 1/300 inch, as the server advertises it (rounded down).
				want := []escl.ScanRegion{{Width: 2480, Height: 3507, ContentRegionUnits: escl.ThreeHundredthsOfInches}}
				if !reflect.DeepEqual(ss.ScanRegions, want) {
					t.Errorf("%s: ScanRegions %+v, want %+v", tc.set.Source, ss.ScanRegions, want)
				}
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

// The XML client reads resolution ranges too (the virtual scanner only
// advertises discrete ones).
func TestXMLCapsResolutionRange(t *testing.T) {
	const capsXML = `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScannerCapabilities xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
 <pwg:Version>2.63</pwg:Version>
 <scan:Platen><scan:PlatenInputCaps>
  <scan:MaxWidth>2550</scan:MaxWidth><scan:MaxHeight>3508</scan:MaxHeight>
  <scan:SettingProfiles><scan:SettingProfile>
   <scan:ColorModes><scan:ColorMode>BlackAndWhite1</scan:ColorMode><scan:ColorMode>RGB24</scan:ColorMode></scan:ColorModes>
   <scan:SupportedResolutions><scan:ResolutionRange>
    <scan:XResolutionRange><scan:Min>75</scan:Min><scan:Max>600</scan:Max><scan:Normal>300</scan:Normal><scan:Step>75</scan:Step></scan:XResolutionRange>
    <scan:YResolutionRange><scan:Min>75</scan:Min><scan:Max>600</scan:Max><scan:Normal>300</scan:Normal><scan:Step>75</scan:Step></scan:YResolutionRange>
   </scan:ResolutionRange></scan:SupportedResolutions>
  </scan:SettingProfile></scan:SettingProfiles>
 </scan:PlatenInputCaps></scan:Platen>
</scan:ScannerCapabilities>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/eSCL/ScannerCapabilities" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, capsXML)
	}))
	defer srv.Close()
	s, err := NewXML(srv.URL + "/eSCL")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Capabilities(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	want := Caps{
		Sources:     []string{"platen"},
		Resolutions: []int{75, 150, 300, 600},
		ColorModes:  []string{"color", "blackandwhite"},
		Intents:     []string{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("caps = %+v, want %+v", got, want)
	}
}

// intentCapsXML is a platen plus ADF simplex capabilities document whose
// SupportedIntents children are named child (scanners use Intent or
// SupportedIntent).
func intentCapsXML(child string) string {
	inputCaps := func(intents ...string) string {
		var b strings.Builder
		b.WriteString(`<scan:MinWidth>16</scan:MinWidth><scan:MaxWidth>2550</scan:MaxWidth><scan:MinHeight>16</scan:MinHeight><scan:MaxHeight>3508</scan:MaxHeight>
  <scan:SettingProfiles><scan:SettingProfile>
   <scan:ColorModes><scan:ColorMode>RGB24</scan:ColorMode></scan:ColorModes>
   <scan:SupportedResolutions><scan:DiscreteResolutions><scan:DiscreteResolution><scan:XResolution>300</scan:XResolution><scan:YResolution>300</scan:YResolution></scan:DiscreteResolution></scan:DiscreteResolutions></scan:SupportedResolutions>
  </scan:SettingProfile></scan:SettingProfiles>
  <scan:SupportedIntents>`)
		for _, it := range intents {
			b.WriteString("<scan:" + child + ">" + it + "</scan:" + child + ">")
		}
		b.WriteString("</scan:SupportedIntents>")
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<scan:ScannerCapabilities xmlns:scan="http://schemas.hp.com/imaging/escl/2011/05/03" xmlns:pwg="http://www.pwg.org/schemas/2010/12/sm">
 <pwg:Version>2.63</pwg:Version>
 <scan:Platen><scan:PlatenInputCaps>` + inputCaps("Photo", "Document") + `</scan:PlatenInputCaps></scan:Platen>
 <scan:Adf><scan:AdfSimplexInputCaps>` + inputCaps("TextAndGraphic", "Document") + `</scan:AdfSimplexInputCaps><scan:FeederCapacity>50</scan:FeederCapacity></scan:Adf>
</scan:ScannerCapabilities>`
}

// inputsOf reads the per-source inputs the way each client's Start does.
func inputsOf(t *testing.T, s Scanner) []input {
	t.Helper()
	switch s := s.(type) {
	case *mfpScanner:
		_, in, err := s.caps(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		return in
	case *xmlScanner:
		c, err := s.caps(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		return c.inputs()
	}
	t.Fatalf("unknown client %T", s)
	return nil
}

// Both clients read SupportedIntents per source under either child name, and
// Caps.Intents is their union in eSCL order.
func TestIntentsBothElementNames(t *testing.T) {
	for _, child := range []string{"Intent", "SupportedIntent"} {
		capsXML := intentCapsXML(child)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/eSCL/ScannerCapabilities" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/xml")
			_, _ = io.WriteString(w, capsXML)
		}))
		t.Cleanup(srv.Close)
		for name, newClient := range clients {
			t.Run(child+"/"+name, func(t *testing.T) {
				s, err := newClient(srv.URL + "/eSCL")
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.Capabilities(ctx(t))
				if err != nil {
					t.Fatal(err)
				}
				if want := []string{"document", "text-and-graphic", "photo"}; !reflect.DeepEqual(got.Intents, want) {
					t.Errorf("Intents = %v, want %v", got.Intents, want)
				}
				per := map[string][]string{}
				for _, in := range inputsOf(t, s) {
					per[in.source] = in.intents
				}
				want := map[string][]string{"platen": {"photo", "document"}, "adf": {"text-and-graphic", "document"}}
				if !reflect.DeepEqual(per, want) {
					t.Errorf("intents per source = %v, want %v", per, want)
				}
			})
		}
	}
}

// Both clients send a chosen intent, send none when unset, and refuse an
// intent the chosen input does not list before any job is created.
func TestConformanceIntent(t *testing.T) {
	for name, newClient := range clients {
		t.Run(name, func(t *testing.T) {
			url, last := virtualScanner(t, false)
			s, err := newClient(url)
			if err != nil {
				t.Fatal(err)
			}
			set := settings("platen", 75, "color")
			set.Intent = "photo"
			job, err := s.Start(ctx(t), set)
			if err != nil {
				t.Fatal(err)
			}
			if ss := last(); ss.Intent == nil || *ss.Intent != escl.Photo {
				t.Errorf("Intent sent = %v, want Photo", ss.Intent)
			}
			readPages(t, s, job)

			job, err = s.Start(ctx(t), settings("platen", 75, "color"))
			if err != nil {
				t.Fatal(err)
			}
			if ss := last(); ss.Intent != nil {
				t.Errorf("Intent sent without one set = %v, want none", *ss.Intent)
			}
			readPages(t, s, job)

			set.Intent = "preview" // valid, but the virtual scanner lists only document and photo
			if _, err := s.Start(ctx(t), set); err == nil || errors.Is(err, ErrInvalidSettings) ||
				!strings.Contains(err.Error(), "no preview intent for platen") {
				t.Errorf("unsupported intent: err = %v, want \"scanner has no preview intent for platen\"", err)
			}
			set.Intent = "poster"
			if _, err := s.Start(ctx(t), set); !errors.Is(err, ErrInvalidSettings) {
				t.Errorf("unknown intent: err = %v, want ErrInvalidSettings", err)
			}
		})
	}
}

func TestValidateIntent(t *testing.T) {
	set := settings("platen", 300, "color")
	for intent, ok := range map[string]bool{"": true, "photo": true, "business-card": true, "poster": false, "Photo": false} {
		set.Intent = intent
		err := Validate(set)
		if ok && err != nil {
			t.Errorf("Validate(intent %q) = %v, want nil", intent, err)
		}
		if !ok && (!errors.Is(err, ErrInvalidSettings) || !strings.Contains(err.Error(), "must be one of document, text-and-graphic, photo")) {
			t.Errorf("Validate(intent %q) = %v, want ErrInvalidSettings listing the intents", intent, err)
		}
	}
}
