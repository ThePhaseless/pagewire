package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const want = `# pagewire sane usb index v1
04a9	1701	pixma
04a9	1702	pixma
04a9	176e	pixma
04e8	3419	xerox_mfp
04e8	341b	xerox_mfp
04e8	341c	xerox_mfp
04e8	342e	xerox_mfp
`

func TestBuild(t *testing.T) {
	got, err := build("testdata/sane.d", dllConf(t, "pixma\nxerox_mfp\n"), []string{"testdata/desc"}, "testdata/libs")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// Only backends dll.conf or a file in dll.d enables are indexed, as SANE's dll
// backend loads only those (upstream comments out epson next to epson2).
func TestDLLConfEnables(t *testing.T) {
	dll := dllConf(t, "# sane-dll.conf\n#pixma\nnet\n")
	got, err := build("testdata/sane.d", dll, []string{"testdata/desc"}, "testdata/libs")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# pagewire sane usb index v1\n" {
		t.Errorf("disabled backends indexed:\n%s", got)
	}
	if err := os.Mkdir(filepath.Join(filepath.Dir(dll), "dll.d"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(dll), "dll.d", "extra"), "# snippet\npixma  # trailing comment\n")
	got, err = build("testdata/sane.d", dll, []string{"testdata/desc"}, "testdata/libs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "04a9\t176e\tpixma\n") || strings.Contains(string(got), "xerox_mfp") {
		t.Errorf("want pixma from dll.d only:\n%s", got)
	}
}

// The fixture libs dir has no net or utsushi library, so it cannot tell a
// banned backend from a missing one. Here both libraries exist.
func TestBannedBackendDroppedEvenWithLib(t *testing.T) {
	libs := t.TempDir()
	for _, b := range []string{"pixma", "xerox_mfp", "net", "utsushi"} {
		writeFile(t, filepath.Join(libs, "libsane-"+b+".so.1"), "")
	}
	got, err := build("testdata/sane.d", dllConf(t, "pixma\nxerox_mfp\nnet\nutsushi\n"), []string{"testdata/desc"}, libs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "\tnet\n") {
		t.Errorf("banned backend net indexed:\n%s", got)
	}
	if !strings.Contains(string(got), "04b8\t1129\tutsushi\n") {
		t.Errorf("utsushi row missing once its library exists:\n%s", got)
	}
}

func TestRunWritesFileWithRepeatedDesc(t *testing.T) {
	extra := t.TempDir()
	writeFile(t, filepath.Join(extra, "epson2.desc"), `:backend "epson2"
:mfg "Epson"
:model "Perfection"
:usbid "0x04b8" "0x0101"
:status :good
`)
	libs := t.TempDir()
	for _, b := range []string{"pixma", "xerox_mfp", "epson2"} {
		writeFile(t, filepath.Join(libs, "libsane-"+b+".so.1"), "")
	}
	out := filepath.Join(t.TempDir(), "sane-usb.tsv")
	dll := dllConf(t, "pixma\nxerox_mfp\nepson2\n")
	args := []string{"-conf", "testdata/sane.d", "-dll", dll, "-desc", "testdata/desc", "-desc", extra, "-libs", libs, "-o", out}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(out) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "04b8\t0101\tepson2\n") || !strings.Contains(string(first), "04a9\t176e\tpixma\n") {
		t.Errorf("rows from both -desc dirs expected:\n%s", first)
	}
	if err := run(args); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(out) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("two runs gave different output")
	}
}

func TestErrors(t *testing.T) {
	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "odd.desc"), ":backend \"odd\"\n:model \"X\"\n:usbid \"0xZZZZ\" \"0x0001\"\n")
	dll := dllConf(t, "pixma\nxerox_mfp\nodd\n")
	orphan := t.TempDir()
	writeFile(t, filepath.Join(orphan, "orphan.desc"), ":usbid \"0x0001\" \"0x0002\"\n")

	for name, args := range map[string][]string{
		"missing flags":   {"-conf", "testdata/sane.d"},
		"missing dll":     {"-conf", "testdata/sane.d", "-dll", "testdata/nope", "-desc", "testdata/desc", "-libs", "testdata/libs", "-o", filepath.Join(t.TempDir(), "o")},
		"missing libs":    {"-conf", "testdata/sane.d", "-dll", dll, "-desc", "testdata/desc", "-libs", "testdata/nope", "-o", filepath.Join(t.TempDir(), "o")},
		"missing conf":    {"-conf", "testdata/nope", "-dll", dll, "-desc", "testdata/desc", "-libs", "testdata/libs", "-o", filepath.Join(t.TempDir(), "o")},
		"missing desc":    {"-conf", "testdata/sane.d", "-dll", dll, "-desc", "testdata/nope", "-libs", "testdata/libs", "-o", filepath.Join(t.TempDir(), "o")},
		"bad usbid":       {"-conf", "testdata/sane.d", "-dll", dll, "-desc", bad, "-libs", "testdata/libs", "-o", filepath.Join(t.TempDir(), "o")},
		"id before model": {"-conf", "testdata/sane.d", "-dll", dll, "-desc", orphan, "-libs", "testdata/libs", "-o", filepath.Join(t.TempDir(), "o")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(args); err == nil {
				t.Error("want an error")
			}
		})
	}

	_, err := build("testdata/sane.d", dll, []string{bad}, "testdata/libs")
	if err == nil || !strings.Contains(err.Error(), "odd.desc:3") {
		t.Errorf("error should name file and line, got %v", err)
	}
}

// dllConf writes content as a dll.conf in a fresh directory and returns its path.
func dllConf(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dll.conf")
	writeFile(t, path, content)
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
