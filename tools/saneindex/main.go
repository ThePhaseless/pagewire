// Command saneindex builds internal/catalog/sane-usb.tsv, the USB vendor and
// product IDs each installed SANE backend claims. It reads the backends'
// sane.d/*.conf "usb 0xVVVV 0xPPPP" lines and the sane-backends source .desc
// files (pixma, epson2 and others hard-code their IDs), and keeps a row only
// when libsane-<backend>.so.1 is installed and the image's dll.conf (or a file
// in dll.d beside it) enables the backend, as SANE's dll backend does. It runs
// at image build time (images/device/Dockerfile, target sane-index) and uses
// only the standard library.
//
//	saneindex -conf /etc/sane.d -dll /etc/sane.d/dll.conf -desc descriptions -desc descriptions-external -libs /usr/lib/x86_64-linux-gnu/sane -o sane-usb.tsv
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const header = "# pagewire sane usb index v1"

// banned backends are network or meta backends; AirSane reaches them anyway.
var banned = map[string]bool{"net": true, "escl": true, "airscan": true, "test": true, "dll": true}

var (
	libRe     = regexp.MustCompile(`^libsane-(.+)\.so\.1$`)
	confUSBRe = regexp.MustCompile(`(?i)^\s*usb\s+0x([0-9a-f]{1,4})\s+0x([0-9a-f]{1,4})(?:\s|$)`)
	keywordRe = regexp.MustCompile(`^:(\w+)\s*(.*)$`)
	quotedRe  = regexp.MustCompile(`^"([^"]*)"`)
	statusRe  = regexp.MustCompile(`^:(\w+)`)
	// Quotes are optional: the 1.4.0 .desc files hold an unquoted pair and a stray trailing quote.
	descUSBRe = regexp.MustCompile(`(?i)^"?0x([0-9a-f]{1,4})"?\s+"?0x([0-9a-f]{1,4})"?(?:[\s;"]|$)`)
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "saneindex:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fl := flag.NewFlagSet("saneindex", flag.ContinueOnError)
	conf := fl.String("conf", "", "sane.d directory with the backends' *.conf files")
	dll := fl.String("dll", "", "the image's dll.conf; it and the files in dll.d beside it enable backends")
	var descs []string
	fl.Func("desc", "directory of sane-backends *.desc files (repeatable)", func(s string) error {
		descs = append(descs, s)
		return nil
	})
	libs := fl.String("libs", "", "directory holding the installed libsane-<backend>.so.1 files")
	out := fl.String("o", "", "output TSV file")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *conf == "" || *dll == "" || len(descs) == 0 || *libs == "" || *out == "" {
		return errors.New("-conf, -dll, -desc, -libs and -o are required")
	}
	idx, err := build(*conf, *dll, descs, *libs)
	if err != nil {
		return err
	}
	return os.WriteFile(*out, idx, 0o644) //nolint:gosec // committed file, world-readable
}

// build returns the index: the header, then sorted unique "vvvv\tpppp\tbackend" rows.
func build(confDir, dllConf string, descDirs []string, libsDir string) ([]byte, error) {
	installed, err := readLibs(libsDir)
	if err != nil {
		return nil, err
	}
	enabled, err := readDLL(dllConf)
	if err != nil {
		return nil, err
	}
	rows := map[string]bool{}
	add := func(vid, pid, backend string) {
		if installed[backend] && enabled[backend] && !banned[backend] {
			rows[vid+"\t"+pid+"\t"+backend] = true
		}
	}
	if err := readConfs(confDir, add); err != nil {
		return nil, err
	}
	for _, d := range descDirs {
		if err := readDescs(d, add); err != nil {
			return nil, err
		}
	}
	lines := make([]string, 0, len(rows))
	for r := range rows {
		lines = append(lines, r)
	}
	slices.Sort(lines) // fixed-width hex, so this sorts by vendor, product, backend
	var b bytes.Buffer
	b.WriteString(header + "\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.Bytes(), nil
}

func readLibs(dir string) (map[string]bool, error) {
	// Names only: the real .so.1 entries are symlinks, the fixtures empty files.
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	libs := map[string]bool{}
	for _, e := range ents {
		if m := libRe.FindStringSubmatch(e.Name()); m != nil {
			libs[m[1]] = true
		}
	}
	return libs, nil
}

// readDLL returns the backends dll.conf and the files in dll.d beside it
// enable: one name per line, "#" starts a comment. Upstream comments out
// superseded backends (epson, next to epson2); hpaio is enabled from dll.d.
func readDLL(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a build input named on the command line
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	dir := filepath.Join(filepath.Dir(path), "dll.d")
	if err := eachFile(dir, "", func(_ string, more []string) error {
		lines = append(lines, more...)
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, l := range lines {
		l, _, _ = strings.Cut(l, "#")
		if f := strings.Fields(l); len(f) > 0 {
			enabled[f[0]] = true
		}
	}
	return enabled, nil
}

// readConfs reads <backend>.conf files; dll.d and other subdirectories are skipped.
func readConfs(dir string, add func(vid, pid, backend string)) error {
	return eachFile(dir, ".conf", func(name string, lines []string) error {
		backend := strings.TrimSuffix(name, ".conf")
		for _, l := range lines {
			if m := confUSBRe.FindStringSubmatch(l); m != nil {
				add(hexID(m[1]), hexID(m[2]), backend)
			}
		}
		return nil
	})
}

// readDescs reads .desc files. A :model block's :status may follow its :usbid,
// so IDs are held until the block ends.
func readDescs(dir string, add func(vid, pid, backend string)) error {
	return eachFile(dir, ".desc", func(name string, lines []string) error {
		var backend, status string
		var ids [][2]string
		flush := func() {
			if status != "unsupported" {
				for _, id := range ids {
					add(id[0], id[1], backend)
				}
			}
			ids, status = nil, ""
		}
		inModel := false
		for i, raw := range lines {
			m := keywordRe.FindStringSubmatch(strings.TrimSpace(raw))
			if m == nil {
				continue
			}
			key, val := m[1], m[2]
			switch key {
			case "backend":
				flush()
				inModel = false
				q := quotedRe.FindStringSubmatch(val)
				if q == nil {
					return fmt.Errorf("%s/%s:%d: bad :backend %q", dir, name, i+1, val)
				}
				backend = q[1]
			case "mfg", "devicetype":
				flush()
				inModel = false
			case "model":
				flush()
				inModel = true
			case "status":
				if s := statusRe.FindStringSubmatch(val); s != nil {
					status = s[1]
				}
			case "usbid":
				if !inModel || backend == "" {
					return fmt.Errorf("%s/%s:%d: :usbid outside a :model block", dir, name, i+1)
				}
				if strings.HasPrefix(strings.ToLower(val), `"ignore"`) {
					continue
				}
				u := descUSBRe.FindStringSubmatch(val)
				if u == nil {
					return fmt.Errorf("%s/%s:%d: bad :usbid %q", dir, name, i+1, val)
				}
				ids = append(ids, [2]string{hexID(u[1]), hexID(u[2])})
			}
		}
		flush()
		return nil
	})
}

// eachFile calls fn with the lines of every regular *ext file directly in dir.
func eachFile(dir, ext string, fn func(name string, lines []string) error) error {
	fsys := os.DirFS(dir)
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		data, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return err
		}
		if err := fn(e.Name(), strings.Split(string(data), "\n")); err != nil {
			return err
		}
	}
	return nil
}

// hexID zero-pads a 1-4 digit hex ID (the real lexmark.conf has "0x43d").
func hexID(s string) string {
	n, _ := strconv.ParseUint(s, 16, 16) // the regexes allow only 1-4 hex digits
	return fmt.Sprintf("%04x", n)
}
