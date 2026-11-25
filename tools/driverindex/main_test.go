package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generate runs the CLI on the testdata dumps and returns the decompressed index.
func generate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "drivers.tsv.gz")
	if err := run(append(args, "-o", out)); err != nil {
		return "", err
	}
	b, err := os.ReadFile(out) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if zr.Name != "" || !zr.ModTime.IsZero() || zr.Comment != "" || zr.Extra != nil {
		t.Errorf("gzip header not empty: %+v", zr.Header)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), nil
}

func TestGenerate(t *testing.T) {
	got, err := generate(t, "-in", "ps=testdata/ps.txt", "-in", "legacy=testdata/legacy.txt")
	if err != nil {
		t.Fatal(err)
	}
	// ps.txt's generic PCL 5e driver needs hpijs, which no image ships, so it is left out.
	want := strings.Join([]string{
		"# pagewire driver index v1",
		"legacy\tbrother--hl-l2350dw--en\t0\tBrother\tHL-L2350DW series\t\tBrother HL-L2350DW series (en)",
		"legacy\tsamsung--scx-4300--2-0-0-en\t0\tSamsung\tSCX-4300\t\tSamsung SCX-4300, 2.0.0 (en)",
		"ps\tgeneric--pcl-printer--en\t0\tGeneric\t\tPCL\tGeneric PCL Printer (en)",
		"ps\tgeneric--postscript-printer--recommended-en\t1\tGeneric\tPostScript Printer\t\tGeneric PostScript Printer (recommended) (en)",
		"ps\tlexmark--e-230--en\t1\tLexmark International\tLexmark E230\tPCL 6 Emulation, PJL\tLexmark E230 (en)",
		"ps\tlexmark--ms-610-dn--en\t0\tLexmark\tMS610dn\tPCLXL,POSTSCRIPT\tLexmark MS610dn (en)",
	}, "\n") + "\n"
	if got != want {
		t.Errorf("index:\n%s\nwant:\n%s", got, want)
	}
}

func TestDeterministic(t *testing.T) {
	a, err := generate(t, "-in", "ps=testdata/ps.txt", "-in", "legacy=testdata/legacy.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := generate(t, "-in", "legacy=testdata/legacy.txt", "-in", "ps=testdata/ps.txt")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("two runs gave different output")
	}
}

func TestRecommended(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{`a--b--recommended-en "d" "MFG:A;MDL:B;"`, true},
		{`a--b--en "d" "MFG:A;MDL:B;DRV:Dx,R1,M0;"`, true},
		{`a--b--en "d" "MFG:A;MDL:B;DRV:Dx,R0,M0;"`, false},
		{`a--b--en "d" "MFG:A;MDL:R1;DRV:DR1,M0;"`, false},
		{`a--b--en "d" ""`, false},
	} {
		r, err := parseLine("f", tc.line)
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if r.recommended != tc.want {
			t.Errorf("%s: recommended %v, want %v", tc.line, r.recommended, tc.want)
		}
	}
}

func TestBadLines(t *testing.T) {
	for _, line := range []string{
		"total 8",
		`name "desc"`,
		`name "desc" devid`,
		`name desc" "devid"`,
		` "desc" "devid"`,
		`name"x" "desc" "devid"`,
		"",
	} {
		if _, err := parseLine("f", line); err == nil {
			t.Errorf("%q: no error", line)
		}
	}
	_, err := generate(t, "-in", "legacy=testdata/bad.txt")
	if err == nil || !strings.Contains(err.Error(), "testdata/bad.txt:2") {
		t.Errorf("error %v, want one naming testdata/bad.txt:2", err)
	}
}

func TestArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"no inputs":        {},
		"no family":        {"-in", "=testdata/ps.txt"},
		"no path":          {"-in", "ps="},
		"no equals":        {"-in", "testdata/ps.txt"},
		"duplicate family": {"-in", "ps=testdata/ps.txt", "-in", "ps=testdata/legacy.txt"},
		"missing file":     {"-in", "ps=testdata/missing.txt"},
	} {
		if _, err := generate(t, args...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestDuplicateDriver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dup.txt")
	line := `a--b--en "d" "MFG:A;MDL:B;"` + "\n"
	if err := os.WriteFile(p, []byte(line+line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := generate(t, "-in", "ps="+p); err == nil {
		t.Error("duplicate driver name: no error")
	}
}
