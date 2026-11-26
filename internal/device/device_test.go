package device

import (
	"encoding/json"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestParseDeviceID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "hp laserjet 1020",
			in:   "MFG:Hewlett-Packard;MDL:HP LaserJet 1020;CMD:ACL;CLS:PRINTER;DES:HP LaserJet 1020;FWVER:20050309;",
			want: map[string]string{"MFG": "Hewlett-Packard", "MDL": "HP LaserJet 1020", "CMD": "ACL", "CLS": "PRINTER", "DES": "HP LaserJet 1020", "FWVER": "20050309"},
		},
		{
			name: "brother with comma-separated command set",
			in:   "MFG:Brother;CMD:PJL,PCL,PCLXL,URF;MDL:HL-L2350DW series;CLS:PRINTER;CID:Brother Laser Type1;URF:W8,CP1,IS4-1,MT1-3-4-5-8,OB10,PQ4,RS300-600-1200,V1.4,DM1;",
			want: map[string]string{"MFG": "Brother", "CMD": "PJL,PCL,PCLXL,URF", "MDL": "HL-L2350DW series", "CLS": "PRINTER", "CID": "Brother Laser Type1", "URF": "W8,CP1,IS4-1,MT1-3-4-5-8,OB10,PQ4,RS300-600-1200,V1.4,DM1"},
		},
		{
			name: "epson long key aliases",
			in:   "MANUFACTURER:EPSON;COMMAND SET:ESC/P-R,ESCPL2,BDC,D4,END4,GENEP;MODEL:XP-2100 Series;CLASS:PRINTER;",
			want: map[string]string{"MFG": "EPSON", "CMD": "ESC/P-R,ESCPL2,BDC,D4,END4,GENEP", "MDL": "XP-2100 Series", "CLASS": "PRINTER"},
		},
		{
			name: "lower-case keys, spaces and no trailing semicolon",
			in:   " mfg: Canon ; mdl:MG3600 series;cmd:BJL,BJRaster3,BSCCe,NCCe,IVEC,IVECPLI",
			want: map[string]string{"MFG": "Canon", "MDL": "MG3600 series", "CMD": "BJL,BJRaster3,BSCCe,NCCe,IVEC,IVECPLI"},
		},
		{
			name: "short key wins over a later alias",
			in:   "MFG:HP;MANUFACTURER:Hewlett-Packard;",
			want: map[string]string{"MFG": "HP"},
		},
		{name: "empty", in: "", want: map[string]string{}},
		{name: "garbage without colon", in: "garbage;;", want: map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseDeviceID(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseDeviceID(%q)\n got  %v\n want %v", tt.in, got, tt.want)
			}
		})
	}
}

func baseSpec() Spec {
	return Spec{
		Mode: ModeUSB,
		Printers: []PrinterSpec{
			{ID: "office", Family: "hplip", USB: USBKey{VendorID: "03f0", ProductID: "2b17", Serial: "S1"}},
			{ID: "laser", Family: "legacy", USB: USBKey{VendorID: "04e8", ProductID: "342e"}},
		},
		Scanners: []ScannerSpec{
			{ID: "office-scanner", Family: "airsane", SaneBackends: []string{"hpaio"}, USB: USBKey{VendorID: "03f0", ProductID: "2b17", Serial: "S1"}},
		},
		Blobs: []BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
	}
}

func TestSpecHash(t *testing.T) {
	base := baseSpec()
	h := SpecHash(&base)

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(h) {
		t.Fatalf("hash %q is not 64 lowercase hex characters", h)
	}
	same := baseSpec()
	if SpecHash(&same) != h {
		t.Error("equal specs hash differently")
	}

	changes := map[string]func(*Spec){
		"node":           func(s *Spec) { s.Node = "n1" },
		"printer family": func(s *Spec) { s.Printers[0].Family = "ps" },
		"printer usb":    func(s *Spec) { s.Printers[1].USB.Serial = "X" },
		"printer port":   func(s *Spec) { s.Printers[0].Port = 8631 },
		"printer name":   func(s *Spec) { s.Printers[0].Name = "Living room printer" },
		"location":       func(s *Spec) { s.Printers[0].Location = "Hall" },
		"printer order":  func(s *Spec) { s.Printers[0], s.Printers[1] = s.Printers[1], s.Printers[0] },
		"no printers":    func(s *Spec) { s.Printers = nil },
		"no scanners":    func(s *Spec) { s.Scanners = nil },
		"blobs":          func(s *Spec) { s.Blobs = nil },
		"sane backends":  func(s *Spec) { s.Scanners[0].SaneBackends = []string{"hpaio", "epson2"} },
		"backends order": func(s *Spec) { s.Scanners[0].SaneBackends = []string{"epson2", "hpaio"} },
		"mode":           func(s *Spec) { s.Mode = ModeProbe },
		"optional flip":  func(s *Spec) { s.Blobs[0].Optional = false },
		"blob version":   func(s *Spec) { s.Blobs[0].Version = "3.25.2" },
		"accepted EULA": func(s *Spec) {
			s.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: strings.Repeat("a", 64)}}
		},
	}
	for name, change := range changes {
		t.Run("changes on "+name, func(t *testing.T) {
			s := baseSpec()
			change(&s)
			if SpecHash(&s) == h {
				t.Errorf("changing %s did not change the hash", name)
			}
		})
	}

	t.Run("Hash field is ignored", func(t *testing.T) {
		s := baseSpec()
		s.Hash = "something else"
		if SpecHash(&s) != h {
			t.Error("the Hash field changed the hash")
		}
	})

	t.Run("nil and empty slices hash alike", func(t *testing.T) {
		a := Spec{Mode: ModeSimulated, Scanners: []ScannerSpec{{ID: "s", Family: "sim"}}}
		b := Spec{
			Mode: ModeSimulated, Printers: []PrinterSpec{}, Scanners: []ScannerSpec{{ID: "s", Family: "sim", SaneBackends: []string{}}},
			Blobs: []BlobRef{}, AcceptedEULAs: []EULARef{},
		}
		if SpecHash(&a) != SpecHash(&b) {
			t.Error("nil and empty slices hash differently")
		}
	})

	t.Run("accepted text sha changes the hash", func(t *testing.T) {
		a, b := baseSpec(), baseSpec()
		a.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: strings.Repeat("a", 64)}}
		b.AcceptedEULAs = []EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: strings.Repeat("b", 64)}}
		if SpecHash(&a) == SpecHash(&b) {
			t.Error("a different accepted text sha256 hashes alike")
		}
	})

	t.Run("input unchanged", func(t *testing.T) {
		s := Spec{Mode: ModeUSB, Scanners: []ScannerSpec{{ID: "a"}, {ID: "b", SaneBackends: []string{}}}, Hash: "old"}
		SpecHash(&s)
		if s.Hash != "old" || s.Printers != nil || s.Blobs != nil || s.AcceptedEULAs != nil {
			t.Errorf("SpecHash modified its input: %#v", s)
		}
		if s.Scanners[0].SaneBackends != nil || s.Scanners[1].SaneBackends == nil {
			t.Error("SpecHash changed the nil-ness of a nested slice")
		}
	})

	t.Run("nil spec", func(t *testing.T) {
		if got := SpecHash(nil); got != "" {
			t.Errorf("SpecHash(nil) = %q, want empty", got)
		}
	})
}

func TestUSBDeviceNodeAndKey(t *testing.T) {
	d := USBDevice{VendorID: "04e8", ProductID: "342e", Serial: "S9", Bus: 1, Address: 5}
	if got := d.Node(); got != "001/005" {
		t.Errorf("Node() = %q", got)
	}
	if got := d.Key(); got != (USBKey{VendorID: "04e8", ProductID: "342e", Serial: "S9"}) {
		t.Errorf("Key() = %+v", got)
	}
	if got := d.Key().String(); got != "04e8:342e:S9" {
		t.Errorf("Key().String() = %q", got)
	}
}

func TestPAPPLSystemURI(t *testing.T) {
	if got := PAPPLSystemURI(8005); got != "ipp://127.0.0.1:8005/ipp/system" {
		t.Errorf("PAPPLSystemURI(8005) = %q", got)
	}
}

// hpCaps reports every print kind.
func hpCaps() PrinterCaps {
	return PrinterCaps{
		Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaDefault: "iso_a4_210x297mm",
		MediaSources: []string{"auto", "tray-1"}, MediaSourceDefault: "auto",
		MediaTypes: []string{"stationery"}, MediaTypeDefault: "stationery",
		Sides: []string{"one-sided", "two-sided-long-edge"}, SidesDefault: "one-sided",
		ColorModes: []string{"color", "monochrome"}, ColorModeDefault: "color",
		Qualities: []string{"normal", "high"}, QualityDefault: "normal",
		Resolutions: []int{300, 600}, ResolutionDefault: 600,
		ContentOptimize: []string{"auto", "photo"}, ContentOptimizeDefault: "auto",
		Orientations: []string{"portrait"}, OrientationDefault: "portrait",
		NumberUp: []int{1, 2}, NumberUpDefault: 1,
		PrintScalings: []string{"auto", "fit"}, PrintScalingDefault: "auto",
		OutputBins: []string{"face-down"}, OutputBinDefault: "face-down",
		Finishings: []string{"none"}, FinishingsDefault: "none",
	}
}

func TestFitPrint(t *testing.T) {
	samsung := PrinterCaps{
		Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaDefault: "na_letter_8.5x11in",
		Sides: []string{"one-sided"}, SidesDefault: "one-sided",
		ColorModes: []string{"monochrome"}, ColorModeDefault: "monochrome",
		Qualities: []string{"draft", "normal", "high"}, QualityDefault: "normal",
		Resolutions: []int{300, 600}, ResolutionDefault: 600,
	}
	tests := []struct {
		name    string
		want    PrintSettings
		caps    PrinterCaps
		fitted  PrintSettings
		dropped []Dropped
	}{
		{name: "empty wants", caps: samsung},
		{name: "empty wants, no caps"},
		{
			name: "samsung colorMode auto gives monochrome", caps: samsung,
			want:    PrintSettings{ColorMode: "auto", Quality: "high"},
			fitted:  PrintSettings{Quality: "high"},
			dropped: []Dropped{{Option: OptionColorMode, Value: "auto", Using: "monochrome"}},
		},
		{
			name: "empty supported list drops with no using", caps: samsung,
			want:    PrintSettings{MediaSource: "tray-1"},
			dropped: []Dropped{{Option: OptionMediaSource, Value: "tray-1", Using: ""}},
		},
		{
			name: "unsupported value uses the default", caps: samsung,
			want:    PrintSettings{Resolution: 1200, Media: "iso_a4_210x297mm"},
			fitted:  PrintSettings{Media: "iso_a4_210x297mm"},
			dropped: []Dropped{{Option: OptionResolution, Value: "1200", Using: "600"}},
		},
		{
			name: "int default not reported gives empty using", caps: PrinterCaps{NumberUp: []int{1}},
			want:    PrintSettings{NumberUp: 4},
			dropped: []Dropped{{Option: OptionNumberUp, Value: "4", Using: ""}},
		},
		{
			name: "every kind supported", caps: hpCaps(),
			want: PrintSettings{
				Media: "na_letter_8.5x11in", MediaSource: "tray-1", MediaType: "stationery", Sides: "two-sided-long-edge",
				ColorMode: "monochrome", Quality: "high", Resolution: 300, ContentOptimize: "photo", Orientation: "portrait",
				NumberUp: 2, PrintScaling: "fit", OutputBin: "face-down", Finishings: "none",
			},
			fitted: PrintSettings{
				Media: "na_letter_8.5x11in", MediaSource: "tray-1", MediaType: "stationery", Sides: "two-sided-long-edge",
				ColorMode: "monochrome", Quality: "high", Resolution: 300, ContentOptimize: "photo", Orientation: "portrait",
				NumberUp: 2, PrintScaling: "fit", OutputBin: "face-down", Finishings: "none",
			},
		},
		{
			name: "every kind unsupported, in table order", caps: hpCaps(),
			want: PrintSettings{
				Media: "iso_a5_148x210mm", MediaSource: "manual", MediaType: "labels", Sides: "two-sided-short-edge",
				ColorMode: "auto", Quality: "draft", Resolution: 1200, ContentOptimize: "text", Orientation: "landscape",
				NumberUp: 4, PrintScaling: "none", OutputBin: "top", Finishings: "staple",
			},
			dropped: []Dropped{
				{OptionMedia, "iso_a5_148x210mm", "iso_a4_210x297mm"},
				{OptionMediaSource, "manual", "auto"},
				{OptionMediaType, "labels", "stationery"},
				{OptionSides, "two-sided-short-edge", "one-sided"},
				{OptionColorMode, "auto", "color"},
				{OptionQuality, "draft", "normal"},
				{OptionResolution, "1200", "600"},
				{OptionContentOptimize, "text", "auto"},
				{OptionOrientation, "landscape", "portrait"},
				{OptionNumberUp, "4", "1"},
				{OptionPrintScaling, "none", "auto"},
				{OptionOutputBin, "top", "face-down"},
				{OptionFinishings, "staple", "none"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fitted, dropped := FitPrint(tt.want, tt.caps)
			if fitted != tt.fitted {
				t.Errorf("fitted\n got  %+v\n want %+v", fitted, tt.fitted)
			}
			if !reflect.DeepEqual(dropped, tt.dropped) {
				t.Errorf("dropped\n got  %+v\n want %+v", dropped, tt.dropped)
			}
		})
	}
}

func TestOptionKinds(t *testing.T) {
	want := []string{
		"media", "mediaSource", "mediaType", "sides", "colorMode", "quality", "resolution", "contentOptimize",
		"orientation", "numberUp", "printScaling", "outputBin", "finishings", "copies", "source", "intent", "format", "other",
	}
	if !reflect.DeepEqual(OptionKinds, want) {
		t.Errorf("OptionKinds = %v", OptionKinds)
	}
}

func TestJSONNames(t *testing.T) {
	mustJSON := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if got := mustJSON(PrinterDecision{}); !strings.Contains(got, `"device":null`) {
		t.Errorf("PrinterDecision with nil Device: %s", got)
	}
	if got := mustJSON(ScannerDecision{}); !strings.Contains(got, `"device":null`) || !strings.Contains(got, `"saneBackends"`) {
		t.Errorf("ScannerDecision JSON: %s", got)
	}
	got := mustJSON(USBDevice{VendorID: "03f0", DeviceID: "MFG:HP;"})
	for _, key := range []string{`"vendorId":"03f0"`, `"deviceId":"MFG:HP;"`, `"productId"`, `"interfaces"`} {
		if !strings.Contains(got, key) {
			t.Errorf("USBDevice JSON %s lacks %s", got, key)
		}
	}
	if got := mustJSON(BlobRef{ID: "a", Version: "1", Optional: true}); got != `{"id":"a","version":"1","optional":true}` {
		t.Errorf("BlobRef JSON: %s", got)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(mustJSON(AgentReport{})), &report); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(report))
	if want := []string{"appliedHash", "attached", "blobs", "desiredVersion", "eulas", "instance", "inventory", "podUid", "probeNodes", "queues", "seq", "services", "wait", "workload"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("AgentReport keys %v, want %v", keys, want)
	}
	if report["attached"] != nil || report["probeNodes"] != nil {
		t.Errorf("nil Attached and ProbeNodes must encode as null: %v", report)
	}
	if got := mustJSON(DesiredState{}); !strings.Contains(got, `"stageEulas"`) || !strings.Contains(got, `"queues"`) || !strings.Contains(got, `"version":""`) {
		t.Errorf("DesiredState JSON: %s", got)
	}
	got = mustJSON(QueueSpec{})
	for _, key := range []string{`"id"`, `"systemUri"`, `"device":{`, `"defaults":{}`} {
		if !strings.Contains(got, key) {
			t.Errorf("QueueSpec JSON %s lacks %s", got, key)
		}
	}
	if got := mustJSON(EULARef{ID: "a", Version: "1", TextSHA256: "ff"}); got != `{"id":"a","version":"1","textSha256":"ff"}` {
		t.Errorf("EULARef JSON: %s", got)
	}
	if got := mustJSON(EULAText{}); got != `{"id":"","version":"","textSha256":"","text":"","state":"","message":""}` {
		t.Errorf("EULAText JSON: %s", got)
	}
	if got := mustJSON(Spec{}); !strings.Contains(got, `"acceptedEulas"`) || !strings.Contains(got, `"node"`) || !strings.Contains(got, `"printers"`) {
		t.Errorf("Spec JSON: %s", got)
	}
	if got := mustJSON(QueueResult{}); !strings.Contains(got, `"id"`) || !strings.Contains(got, `"printerUri"`) ||
		!strings.Contains(got, `"deviceUri"`) || !strings.Contains(got, `"warnings"`) || !strings.Contains(got, `"unreachable"`) {
		t.Errorf("QueueResult JSON: %s", got)
	}
	if got := mustJSON(DNSSDService{}); !strings.Contains(got, `"txt"`) {
		t.Errorf("DNSSDService JSON: %s", got)
	}
	if got := mustJSON(PrinterDecision{}); !strings.Contains(got, `"family"`) || !strings.Contains(got, `"requiredEulas"`) || !strings.Contains(got, `"optionalEulas"`) ||
		!strings.Contains(got, `"driver":""`) || !strings.Contains(got, `"driverDescription":""`) {
		t.Errorf("PrinterDecision JSON: %s", got)
	}
}

func TestDesiredStateVersion(t *testing.T) {
	base := func() DesiredState {
		return DesiredState{
			Hash:       "h",
			Queues:     []QueueSpec{{ID: "office", Name: "Office", Schemes: []string{"hp", "usb"}, Device: USBDevice{VendorID: "03f0", ProductID: "2b17"}}},
			Browse:     BrowseTypes,
			StageEULAs: []EULARef{{ID: "hplip-plugin", Version: "3.24.4"}},
		}
	}
	ds := base()
	v := DesiredStateVersion(ds)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(v) {
		t.Fatalf("version %q is not 64 lowercase hex characters", v)
	}
	ds.Version = v
	if DesiredStateVersion(ds) != v {
		t.Error("the Version field changes the version")
	}
	if ds.Queues[0].Schemes == nil || ds.Queues[0].Device.Interfaces != nil {
		t.Error("DesiredStateVersion modified its argument's queues")
	}

	empty := DesiredState{Queues: []QueueSpec{{ID: "q", Schemes: []string{}, Device: USBDevice{Interfaces: []USBInterface{}}}}, Browse: []string{}, StageEULAs: []EULARef{}}
	nils := DesiredState{Queues: []QueueSpec{{ID: "q"}}}
	if DesiredStateVersion(empty) != DesiredStateVersion(nils) {
		t.Error("nil and empty slices version differently")
	}
	if DesiredStateVersion(DesiredState{Queues: []QueueSpec{}}) != DesiredStateVersion(DesiredState{}) {
		t.Error("nil and empty queues version differently")
	}

	changes := map[string]func(*DesiredState){
		"queue name":   func(ds *DesiredState) { ds.Queues[0].Name = "Hall" },
		"queue added":  func(ds *DesiredState) { ds.Queues = append(ds.Queues, QueueSpec{ID: "laser"}) },
		"no queues":    func(ds *DesiredState) { ds.Queues = nil },
		"queue device": func(ds *DesiredState) { ds.Queues[0].Device.Serial = "S1" },
		"hash":         func(ds *DesiredState) { ds.Hash = "other" },
		"stage EULAs":  func(ds *DesiredState) { ds.StageEULAs = nil },
	}
	for name, change := range changes {
		t.Run("changes on "+name, func(t *testing.T) {
			ds := base()
			change(&ds)
			if DesiredStateVersion(ds) == v {
				t.Errorf("changing %s did not change the version", name)
			}
		})
	}
}
