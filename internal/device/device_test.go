package device

import (
	"encoding/json"
	"reflect"
	"regexp"
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
		Mode:         ModeUSB,
		Printer:      "hplip",
		Scanner:      "airsane",
		SaneBackends: []string{"hpaio"},
		Blobs:        []BlobRef{{ID: "hplip-plugin", Version: "3.24.4", Optional: true}},
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
		"printer":        func(s *Spec) { s.Printer = "ps" },
		"blobs":          func(s *Spec) { s.Blobs = nil },
		"sane backends":  func(s *Spec) { s.SaneBackends = []string{"hpaio", "epson2"} },
		"name":           func(s *Spec) { s.Name = "Living room printer" },
		"location":       func(s *Spec) { s.Location = "Hall" },
		"mode":           func(s *Spec) { s.Mode = ModeProbe },
		"optional flip":  func(s *Spec) { s.Blobs[0].Optional = false },
		"blob version":   func(s *Spec) { s.Blobs[0].Version = "3.25.2" },
		"scanner":        func(s *Spec) { s.Scanner = "" },
		"backends order": func(s *Spec) { s.SaneBackends = []string{"epson2", "hpaio"} },
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
		a := Spec{Mode: ModeSimulated, Printer: "sim", Scanner: "sim"}
		b := Spec{Mode: ModeSimulated, Printer: "sim", Scanner: "sim", SaneBackends: []string{}, Blobs: []BlobRef{}, AcceptedEULAs: []EULARef{}}
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
		for _, s := range []Spec{
			{Mode: ModeProbe, Hash: "old"},
			{Mode: ModeUSB, SaneBackends: []string{}, Blobs: []BlobRef{{ID: "x", Version: "1"}}, Hash: "old"},
		} {
			before := s // deep copy that keeps the nil-ness of each slice
			if s.SaneBackends != nil {
				before.SaneBackends = append([]string{}, s.SaneBackends...)
			}
			if s.Blobs != nil {
				before.Blobs = append([]BlobRef{}, s.Blobs...)
			}
			SpecHash(&s)
			if !reflect.DeepEqual(s, before) {
				t.Errorf("SpecHash modified its input:\n got  %#v\n want %#v", s, before)
			}
			if (s.SaneBackends == nil) != (before.SaneBackends == nil) || (s.Blobs == nil) != (before.Blobs == nil) ||
				(s.AcceptedEULAs == nil) != (before.AcceptedEULAs == nil) {
				t.Error("SpecHash changed the nil-ness of a slice")
			}
		}
	})

	t.Run("nil spec", func(t *testing.T) {
		if got := SpecHash(nil); got != "" {
			t.Errorf("SpecHash(nil) = %q, want empty", got)
		}
	})
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

	if got := mustJSON(Decision{}); !strings.Contains(got, `"device":null`) {
		t.Errorf("Decision with nil Device: %s", got)
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
	if got := mustJSON(AgentReport{}); !strings.Contains(got, `"podUid"`) || !strings.Contains(got, `"appliedHash"`) || !strings.Contains(got, `"eulas"`) {
		t.Errorf("AgentReport JSON: %s", got)
	}
	if got := mustJSON(DesiredState{}); !strings.Contains(got, `"stageEulas"`) {
		t.Errorf("DesiredState JSON: %s", got)
	}
	if got := mustJSON(EULARef{ID: "a", Version: "1", TextSHA256: "ff"}); got != `{"id":"a","version":"1","textSha256":"ff"}` {
		t.Errorf("EULARef JSON: %s", got)
	}
	if got := mustJSON(EULAText{}); got != `{"id":"","version":"","textSha256":"","text":"","state":"","message":""}` {
		t.Errorf("EULAText JSON: %s", got)
	}
	if got := mustJSON(Spec{}); !strings.Contains(got, `"acceptedEulas"`) {
		t.Errorf("Spec JSON: %s", got)
	}
	if got := mustJSON(QueueResult{}); !strings.Contains(got, `"printerUri"`) || !strings.Contains(got, `"deviceUri"`) {
		t.Errorf("QueueResult JSON: %s", got)
	}
	if got := mustJSON(DNSSDService{}); !strings.Contains(got, `"txt"`) {
		t.Errorf("DNSSDService JSON: %s", got)
	}
	if got := mustJSON(Decision{}); !strings.Contains(got, `"requiredEulas"`) || !strings.Contains(got, `"optionalEulas"`) {
		t.Errorf("Decision JSON: %s", got)
	}
}
