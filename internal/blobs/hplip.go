package blobs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The HP plug-in is a makeself .run archive. hplip 3.24.4 runs it with sh and
// then installer/pluginhandler.py copies files per plugin.spec; this file does
// the same in Go without a shell.

const (
	extractRatio    = 10    // decompressed payload may be at most this many times maxBytes
	maxEntries      = 10000 // tar entries
	maxHeaderBytes  = 1 << 20
	hplipHome       = "/usr/share/hplip" // hplip.conf [dirs] home
	errNoArchFormat = "HP plug-in has no %s build"
)

// linkedDirs are the paths under hplip's home that the driver and device images
// link into the blob volume (decision 14). Targets elsewhere are skipped.
var linkedDirs = []string{"prnt/plugins", "scan/plugins", "fax/plugins", "data/firmware"}

// HPLIPArch maps a GOARCH to the $ARCH name pluginhandler.py derives from
// platform.machine() and the bitness.
func HPLIPArch(goarch string) (string, bool) {
	arch, ok := map[string]string{"amd64": "x86_64", "386": "x86_32", "arm64": "arm64", "arm": "arm32"}[goarch]
	return arch, ok
}

func installHPLIP(run, payloadDir, out, goarch, version string, maxExtract int64) error {
	if err := extractMakeself(run, payloadDir, maxExtract); err != nil {
		return err
	}
	src, err := os.OpenRoot(payloadDir)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenRoot(out)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	return copyPlugin(src, dst, goarch, version)
}

var (
	offsetLine = regexp.MustCompile(`^offset=` + "`" + `head -n ([0-9]+) `)
	skipLine   = regexp.MustCompile(`^skip="?([0-9]+)"?\s*$`)
)

// payloadOffset finds where the gzip payload starts. makeself 2.1 and 2.2
// write an offset= line running head -n N on the script, and 2.4 and later
// write skip="N" near the top; both mean the payload follows N lines. makeself
// 2.0 wrote skip="N" for tail +N, so the payload starts at line N. Both
// readings are tried and the one that lands on gzip magic wins.
func payloadOffset(f *os.File) (int64, error) {
	r := bufio.NewReader(io.LimitReader(f, maxHeaderBytes))
	var lineEnds []int64 // byte offset after each line
	var pos int64
	n := -1
	for n < 0 || len(lineEnds) < n {
		line, err := r.ReadString('\n')
		if err != nil {
			if n < 0 {
				return 0, errors.New("HP plug-in: no makeself payload offset in the header")
			}
			break // with skip="N" from makeself 2.0, line N is already payload
		}
		pos += int64(len(line))
		lineEnds = append(lineEnds, pos)
		if n >= 0 {
			continue
		}
		t := strings.TrimSpace(line)
		if m := offsetLine.FindStringSubmatch(t); m != nil {
			n, _ = strconv.Atoi(m[1])
		} else if m := skipLine.FindStringSubmatch(t); m != nil {
			n, _ = strconv.Atoi(m[1])
		}
	}
	for _, lines := range []int{n, n - 1} {
		if lines < 1 || lines > len(lineEnds) {
			continue
		}
		off := lineEnds[lines-1]
		magic := make([]byte, 2)
		if _, err := f.ReadAt(magic, off); err == nil && bytes.Equal(magic, []byte{0x1f, 0x8b}) {
			return off, nil
		}
	}
	return 0, errors.New("HP plug-in: makeself payload is not gzip")
}

// extractMakeself unpacks the tar.gz payload of run into dir. It rejects
// absolute paths, `..`, links leaving dir, hard links and special files, and
// stops at maxBytes of content or maxEntries entries. All writes go through
// os.Root, so a crafted entry cannot reach outside dir either way.
func extractMakeself(run, dir string, maxBytes int64) error {
	f, err := os.Open(run) //nolint:gosec // path inside the blob dir's .tmp
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	off, err := payloadOffset(f)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(io.NewSectionReader(f, off, 1<<62))
	if err != nil {
		return fmt.Errorf("HP plug-in payload: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // temporary, same tree as the install
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	tr := tar.NewReader(gz)
	budget := maxBytes
	for entries := 0; ; entries++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("HP plug-in payload: %w", err)
		}
		if entries >= maxEntries {
			return fmt.Errorf("HP plug-in payload has more than %d entries", maxEntries)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := path.Clean(h.Name)
		if path.IsAbs(h.Name) || !filepath.IsLocal(name) {
			return fmt.Errorf("HP plug-in payload: unsafe path %q", h.Name)
		}
		if name == "." {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, 0o755)
		case tar.TypeReg:
			if h.Size > budget {
				return fmt.Errorf("HP plug-in payload is over the %d byte extraction limit", maxBytes)
			}
			budget -= h.Size
			err = writeEntry(root, name, tr)
		case tar.TypeSymlink:
			target := h.Linkname
			if path.IsAbs(target) || !filepath.IsLocal(path.Join(path.Dir(name), target)) { //nolint:gosec // G305: this is the traversal check
				return fmt.Errorf("HP plug-in payload: unsafe link %q -> %q", h.Name, h.Linkname)
			}
			if err = root.MkdirAll(path.Dir(name), 0o755); err == nil {
				err = root.Symlink(target, name)
			}
		default:
			return fmt.Errorf("HP plug-in payload: unsafe entry type %q for %q", h.Typeflag, h.Name)
		}
		if err != nil {
			return fmt.Errorf("HP plug-in payload: %w", err)
		}
	}
}

func writeEntry(root *os.Root, name string, r io.Reader) error {
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	w, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// copyPlugin copies payload files into dst per plugin.spec, as
// pluginhandler.py's copyFiles does, then writes hplip.state.
func copyPlugin(src, dst *os.Root, goarch, version string) error {
	raw, err := src.ReadFile("plugin.spec")
	if err != nil {
		return fmt.Errorf("HP plug-in: %w", err)
	}
	spec := parseINI(string(raw))
	arch, ok := HPLIPArch(goarch)
	if !ok {
		return fmt.Errorf(errNoArchFormat, goarch)
	}
	vars := specVars(arch, goarch)

	type entry struct{ src, trg, link string }
	var entries []entry
	archEntries, archFound := 0, 0
	products := spec["products"]
	for _, product := range slices.Sorted(maps.Keys(products)) {
		vars["PRODUCT"] = product
		vars["MODEL"] = strings.NewReplacer("hp-", "", "hp_", "").Replace(product)
		for _, s := range strings.Split(products[product], ",") {
			s = strings.TrimSpace(s)
			sec, ok := spec[s]
			if !ok {
				return fmt.Errorf("HP plug-in: plugin.spec has no section [%s]", s)
			}
			if sec["src"] == "" || sec["trg"] == "" {
				return fmt.Errorf("HP plug-in: plugin.spec section [%s] needs src and trg", s)
			}
			var e entry
			for _, f := range []struct {
				in  string
				out *string
			}{{sec["src"], &e.src}, {sec["trg"], &e.trg}, {sec["link"], &e.link}} {
				if *f.out, err = substitute(f.in, vars); err != nil {
					return fmt.Errorf("HP plug-in: plugin.spec [%s]: %w", s, err)
				}
			}
			e.src = path.Base(e.src)
			if slices.Contains(entries, e) {
				continue
			}
			entries = append(entries, e)
			usesArch := strings.Contains(sec["src"], "$ARCH") || strings.Contains(sec["src"], "${ARCH}")
			if usesArch {
				archEntries++
			}
			fi, err := src.Stat(e.src)
			if err != nil || !fi.Mode().IsRegular() {
				continue // pluginhandler.py skips missing sources too
			}
			if usesArch {
				archFound++
			}
		}
	}
	if archEntries > 0 && archFound == 0 {
		return fmt.Errorf(errNoArchFormat, goarch)
	}

	for _, d := range linkedDirs {
		if err := dst.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	copied := 0
	for _, e := range entries {
		trg, ok := linkedPath(e.trg)
		if !ok {
			continue
		}
		if fi, err := src.Stat(e.src); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := copyFile(src, dst, e.src, trg); err != nil {
			return fmt.Errorf("HP plug-in: %w", err)
		}
		copied++
		if link, ok := linkedPath(e.link); ok {
			rel, err := filepath.Rel(path.Dir(link), trg)
			if err != nil {
				return err
			}
			_ = dst.Remove(link)
			if err := dst.Symlink(rel, link); err != nil {
				return fmt.Errorf("HP plug-in: %w", err)
			}
		}
	}
	if copied == 0 {
		return errors.New("HP plug-in: plugin.spec installs no files")
	}
	// The format ConfigParser writes and hplip's C get_key_value reads.
	state := "[plugin]\ninstalled = 1\neula = 1\nversion = " + version + "\n\n"
	return dst.WriteFile("hplip.state", []byte(state), 0o644)
}

// linkedPath maps an absolute hplip target to its path in the blob tree, or
// false when the images do not link that location to the blob volume.
func linkedPath(trg string) (string, bool) {
	rel, ok := strings.CutPrefix(path.Clean(trg), hplipHome+"/")
	if !ok || !filepath.IsLocal(rel) {
		return "", false
	}
	for _, d := range linkedDirs {
		if strings.HasPrefix(rel, d+"/") {
			return rel, true
		}
	}
	return "", false
}

func copyFile(src, dst *os.Root, from, to string) error {
	r, err := src.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	if err := dst.MkdirAll(path.Dir(to), 0o755); err != nil {
		return err
	}
	_ = dst.Remove(to)
	w, err := dst.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, r)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// pluginhandler.py sets rwxr-xr-x regardless of the umask.
	return dst.Chmod(to, 0o755)
}

// specVars are the locals of pluginhandler.py's __getPluginFilesList that
// plugin.spec values may name, with Ubuntu's hplip.conf directories.
func specVars(arch, goarch string) map[string]string {
	bitness, libdir, sanelibdir := "64", "/usr/lib64", "/usr/lib64/sane"
	if goarch == "386" || goarch == "arm" {
		bitness, libdir, sanelibdir = "32", "/usr/lib", "/usr/lib/sane"
	}
	return map[string]string{
		"ARCH": arch, "BITNESS": bitness, "ENDIAN": "1", // utils.LITTLE_ENDIAN
		"HOMEDIR": hplipHome, "PPDDIR": "/usr/share/ppd/hplip/HP", "DRVDIR": "/usr/share/cups/drv/hp",
		"DOCDIR": "/usr/share/doc/hplip", "CUPSBACKENDDIR": "/usr/lib/cups/backend",
		"CUPSFILTERDIR": "/usr/lib/cups/filter", "RULESDIR": "/etc/udev/rules.d", "BIN": "/usr/bin",
		"LIBDIR": libdir, "SANELIBDIR": sanelibdir,
	}
}

var templateVar = regexp.MustCompile(`\$(?:\$|([_a-zA-Z][_a-zA-Z0-9]*)|\{([_a-zA-Z][_a-zA-Z0-9]*)\})`)

// substitute expands $NAME, ${NAME} and $$ like Python's Template.substitute,
// failing on an unknown name as it does.
func substitute(s string, vars map[string]string) (string, error) {
	var missing string
	out := templateVar.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		name := strings.Trim(m, "${}")
		v, ok := vars[name]
		if !ok && missing == "" {
			missing = name
		}
		return v
	})
	if missing != "" {
		return "", fmt.Errorf("unknown variable $%s", missing)
	}
	return out, nil
}

// parseINI reads the ConfigParser subset plugin.spec uses: [section] headers,
// key=value or key: value, # and ; comment lines. Keys are lower-cased like
// ConfigParser's optionxform; section names keep their case.
func parseINI(s string) map[string]map[string]string {
	out := map[string]map[string]string{}
	var cur map[string]string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || t[0] == '#' || t[0] == ';':
		case t[0] == '[' && strings.HasSuffix(t, "]"):
			name := t[1 : len(t)-1]
			if out[name] == nil {
				out[name] = map[string]string{}
			}
			cur = out[name]
		case cur != nil:
			i := strings.IndexAny(t, "=:")
			if i < 0 {
				continue
			}
			cur[strings.ToLower(strings.TrimSpace(t[:i]))] = strings.TrimSpace(t[i+1:])
		}
	}
	return out
}
