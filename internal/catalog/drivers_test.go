package catalog

import (
	"reflect"
	"strings"
	"testing"
)

// testIndex is a small synthetic driver index: the header, then family, name,
// recommended, mfg, mdl, cmd, description.
const testIndex = `# pagewire driver index v1
ghostscript	hp--laserjet-1020--foo-recommended-en	1	Hewlett-Packard	HP LaserJet 1020		HP LaserJet 1020, foo (recommended) (en)
ghostscript	hp--laserjet-1020--bar-en	0	Hewlett-Packard	HP LaserJet 1020		HP LaserJet 1020, bar (en)
ghostscript	lexmark--ms-610-dn--postscript-en	0	Lexmark	Lexmark MS610dn		Lexmark MS610dn, Postscript (en)
hplip	hp--laserjet-1020--requires-proprietary-plugin-en	0	HP	HP LaserJet 1020		HP LaserJet 1020 (en)
hplip	generic--postscript-printer--postscript-en	0			PS	Generic PostScript Printer (en)
legacy	samsung--scx-4300--2-0-0-en	0	Samsung	SCX-4300		Samsung SCX-4300, 2.0.0 (en)
ps	generic--postscript-printer--en	0	Generic	PostScript Printer		Generic PostScript Printer (en)
ps	generic--postscript-printer--recommended-en	1	Generic	PostScript Printer		Generic PostScript Printer (recommended) (en)
ps	lexmark--ms-610-dn--en	0	Lexmark	Lexmark MS610dn		Lexmark MS610dn (en)
ps	zed--x-1--en	0	Alpha	X1		Zed X1 (en)
ps	tie--x-1--en	0	Alpha	X1		Tie X1 (en)
`

func parseTestIndex(t *testing.T, src string) *DriverIndex {
	t.Helper()
	x, err := ParseDriverIndex(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	return x
}

var testPriority = []string{"hplip", "legacy", "ps", "gutenprint", "ghostscript"}

func TestDriverIndexMatch(t *testing.T) {
	x := parseTestIndex(t, testIndex)
	for _, tc := range []struct {
		name, mfg, mdl, family string
		priority               []string
		want                   string // "" means no match
	}{
		// HP, Hewlett-Packard and HEWLETT-PACKARD are one vendor; hplip outranks ghostscript.
		{"alias learned from data", "HEWLETT-PACKARD", "HP LaserJet 1020", "", testPriority, "hp--laserjet-1020--requires-proprietary-plugin-en"},
		{"alias HP", "HP", "LaserJet 1020", "", testPriority, "hp--laserjet-1020--requires-proprietary-plugin-en"},
		{"family restricted, recommended first", "Hewlett-Packard", "HP LaserJet 1020", "ghostscript", testPriority, "hp--laserjet-1020--foo-recommended-en"},
		{"family priority beats recommended", "Hewlett-Packard", "HP LaserJet 1020", "", []string{"ghostscript", "hplip"}, "hp--laserjet-1020--foo-recommended-en"},
		{"series and make prefix stripped", "Samsung", "SCX-4300 Series", "", testPriority, "samsung--scx-4300--2-0-0-en"},
		{"series is a whole word", "Samsung", "SCX-4300series", "", testPriority, ""},
		{"vendor from the first word", "Lexmark International", "Lexmark MS610dn", "", testPriority, "lexmark--ms-610-dn--en"},
		{"recommended within a family", "Generic", "PostScript Printer", "", testPriority, "generic--postscript-printer--recommended-en"},
		{"rows without MDL are not matched", "Generic", "PostScript Printer", "hplip", testPriority, ""},
		{"family without the model", "Samsung", "SCX-4300", "ps", testPriority, ""},
		{"unknown model", "Zebra", "GK420d", "", testPriority, ""},
		{"name breaks ties", "Alpha", "X1", "", testPriority, "tie--x-1--en"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := x.Match(tc.mfg, tc.mdl, tc.priority, tc.family)
			if ok != (tc.want != "") || d.Name != tc.want {
				t.Errorf("Match = %q, %v, want %q", d.Name, ok, tc.want)
			}
		})
	}
}

// The majority of a MFG's rows decides its vendor; a tie goes to the smaller key.
func TestDriverIndexAliasVotes(t *testing.T) {
	x := parseTestIndex(t, `# pagewire driver index v1
ps	aaa--m-1--en	0	Acme Corp	M1		d
ps	bbb--m-2--en	0	Acme Corp	M2		d
ps	ccc--n-1--en	0	Zeta	N1		d
ps	ccc--n-2--en	0	Zeta	N2		d
ps	ddd--n-3--en	0	Zeta	N3		d
`)
	for mfg, want := range map[string]string{"Acme Corp": "aaa", "ZETA": "ccc", "ddd": "ddd", "Other Inc": "otherinc"} {
		if got := x.vendor(mfg); got != want {
			t.Errorf("vendor(%q) = %q, want %q", mfg, got, want)
		}
	}
	// Acme Corp's rows all resolve to aaa, so a device can reach bbb's model too.
	if d, ok := x.Match("Acme Corp", "M2", nil, ""); !ok || d.Name != "bbb--m-2--en" {
		t.Errorf("Match(Acme Corp, M2) = %+v, %v", d, ok)
	}
}

func TestDriverIndexLookup(t *testing.T) {
	x := parseTestIndex(t, testIndex)
	d, ok := x.Lookup("ps", "generic--postscript-printer--en")
	if !ok || d.Description != "Generic PostScript Printer (en)" || d.CMD != "" || d.Recommended {
		t.Errorf("Lookup = %+v, %v", d, ok)
	}
	if _, ok := x.Lookup("ghostscript", "generic--postscript-printer--en"); ok {
		t.Error("Lookup found a name in the wrong family")
	}
	// Rows without MDL still count for names.
	if _, ok := x.Lookup("hplip", "generic--postscript-printer--postscript-en"); !ok {
		t.Error("Lookup missed a row without MDL")
	}
	if _, ok := x.Lookup("ps", "nope"); ok {
		t.Error("Lookup found nope")
	}
	var nilIndex *DriverIndex
	if _, ok := nilIndex.Match("HP", "x", nil, ""); ok {
		t.Error("nil index matched")
	}
	if _, ok := nilIndex.Lookup("ps", "x"); ok {
		t.Error("nil index has drivers")
	}
}

func TestParseDriverIndexErrors(t *testing.T) {
	for name, src := range map[string]string{
		"no header":       "ps\ta--b\t0\tA\tB\t\td\n",
		"short row":       "# pagewire driver index v1\nps\ta--b\t0\tA\n",
		"bad recommended": "# pagewire driver index v1\nps\ta--b\tyes\tA\tB\t\td\n",
	} {
		if _, err := ParseDriverIndex(strings.NewReader(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseSaneIndex(t *testing.T) {
	s, err := ParseSaneIndex(strings.NewReader("# pagewire sane usb index v1\n04b8\t0806\tepson2\n04b8\t0806\tepson\n04E8\t342E\txerox_mfp\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := SaneIndex{"04b8:0806": {"epson", "epson2"}, "04e8:342e": {"xerox_mfp"}}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("got %v", s)
	}
	for name, src := range map[string]string{
		"no header": "04b8\t0806\tepson\n",
		"short row": "# pagewire sane usb index v1\n04b8\t0806\n",
		"bad id":    "# pagewire sane usb index v1\n4b8\t0806\tepson\n",
	} {
		if _, err := ParseSaneIndex(strings.NewReader(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
