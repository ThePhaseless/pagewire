package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/config/yamledit"
)

const valid = `apiVersion: pagewire/v2
# the UI title
name: Office   # shown in the UI

scan:
  retentionDays: 7
`

func check(b []byte) error {
	_, err := config.Parse(b)
	return err
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pagewire.yaml")
	return New(path, check), path
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestReadMissing(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Read(); !errors.Is(err, ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

func TestCreate(t *testing.T) {
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, got, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != valid || got != sum([]byte(valid)) || etag != got {
		t.Fatalf("Read = %q, %q; Create etag %q", raw, got, etag)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", fi.Mode().Perm())
	}
	assertOnlyConfig(t, path)
}

func TestCreateTwice(t *testing.T) {
	s, path := newStore(t)
	if _, err := s.Create([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(valid, "Office", "Other", 1)
	if _, err := s.Create([]byte(other)); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	assertContent(t, path, valid)
}

func TestCreateInvalid(t *testing.T) {
	s, path := newStore(t)
	_, err := s.Create([]byte("apiVersion: pagewire/v2\n"))
	var el *config.ErrorList
	if !errors.As(err, &el) {
		t.Fatalf("err = %v, want *config.ErrorList", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file written despite validation failure: %v", err)
	}
}

func TestPatch(t *testing.T) {
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	raw, newEtag, err := s.Patch(etag, []byte(`{"name":"Home"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(valid, "name: Office   #", "name: Home     #", 1)
	if string(raw) != want || newEtag != sum([]byte(want)) {
		t.Fatalf("Patch = %q, %q", raw, newEtag)
	}
	assertContent(t, path, want)
	assertOnlyConfig(t, path)
}

func TestPatchStaleEtag(t *testing.T) {
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Patch("0000", []byte(`{"name":"Home"}`))
	var pf *ErrPreconditionFailed
	if !errors.As(err, &pf) || pf.CurrentEtag != etag {
		t.Fatalf("err = %#v, want ErrPreconditionFailed{%q}", err, etag)
	}
	assertContent(t, path, valid)
}

func TestPatchInvalidResult(t *testing.T) {
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Patch(etag, []byte(`{"name":null}`))
	if _, ok := err.(*config.ErrorList); !ok { //nolint:errorlint // the API relies on the list arriving unwrapped
		t.Fatalf("err = %#v, want an unwrapped *config.ErrorList", err)
	}
	assertContent(t, path, valid)
}

func TestNoOpPatchOnInvalidFile(t *testing.T) {
	s, path := newStore(t)
	const invalid = "apiVersion: pagewire/v2\n"
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	_, etag, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Patch(etag, []byte(`{}`)); !errors.As(err, new(*config.ErrorList)) {
		t.Fatalf("err = %v, want *config.ErrorList", err)
	}
	assertContent(t, path, invalid)
}

func TestPatchBadBody(t *testing.T) {
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Patch(etag, []byte(`[1]`)); !errors.Is(err, yamledit.ErrInvalidPatch) {
		t.Fatalf("err = %v, want yamledit.ErrInvalidPatch", err)
	}
	assertContent(t, path, valid)
}

func TestPatchUnparseableFile(t *testing.T) {
	s, path := newStore(t)
	if err := os.WriteFile(path, []byte("name: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, etag, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Patch(etag, []byte(`{"name":"x"}`))
	var el *config.ErrorList
	if !errors.As(err, &el) || el.Errors[0].Line == 0 {
		t.Fatalf("err = %#v, want the file's *config.ErrorList with a position", err)
	}
	assertContent(t, path, string(raw))
}

func TestMissingFile(t *testing.T) {
	s, _ := newStore(t)
	if _, _, err := s.Patch("x", []byte(`{}`)); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Patch err = %v, want ErrNotExist", err)
	}
	if _, err := s.Replace("x", []byte(valid)); !errors.Is(err, ErrNotExist) {
		t.Fatalf("Replace err = %v, want ErrNotExist", err)
	}
}

func TestReplace(t *testing.T) {
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	other := "apiVersion: pagewire/v2\nname: Other\n"
	if _, err := s.Replace("stale", []byte(other)); !errors.As(err, new(*ErrPreconditionFailed)) {
		t.Fatalf("err = %v, want ErrPreconditionFailed", err)
	}
	assertContent(t, path, valid)
	got, err := s.Replace(etag, []byte(other))
	if err != nil || got != sum([]byte(other)) {
		t.Fatalf("Replace = %q, %v", got, err)
	}
	assertContent(t, path, other)
}

func TestConcurrentPatches(t *testing.T) {
	for range 20 {
		s, _ := newStore(t)
		etag, err := s.Create([]byte(valid))
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, name := range []string{"A", "B"} {
			wg.Go(func() {
				_, _, errs[i] = s.Patch(etag, []byte(`{"name":"`+name+`"}`))
			})
		}
		wg.Wait()
		ok, failed := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.As(err, new(*ErrPreconditionFailed)):
				failed++
			default:
				t.Fatal(err)
			}
		}
		if ok != 1 || failed != 1 {
			t.Fatalf("ok=%d precondition-failed=%d, want 1 and 1", ok, failed)
		}
	}
}

// TestReadersSeeWholeFiles checks crash safety from the outside: while writes
// run, a reader only ever sees one complete version, never a partial file;
// a reader that opened the old file keeps reading it whole (the file is
// replaced, not rewritten in place); and no temporary file is left behind.
func TestReadersSeeWholeFiles(t *testing.T) {
	s, path := newStore(t)
	versions := map[string]bool{valid: true}
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	var contents []string
	for i := range 50 {
		contents = append(contents, "apiVersion: pagewire/v2\nname: P\n# "+strings.Repeat("x", i*400)+"\n")
	}
	for _, c := range contents {
		versions[c] = true
	}
	done := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		defer close(bad)
		for {
			select {
			case <-done:
				return
			default:
			}
			b, err := os.ReadFile(path) //nolint:gosec // test temp path
			if err != nil || !versions[string(b)] {
				bad <- string(b)
				return
			}
		}
	}()
	for _, c := range contents {
		if etag, err = s.Replace(etag, []byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	close(done)
	if b, ok := <-bad; ok {
		t.Fatalf("reader saw a partial file: %q", b)
	}
	if b, err := io.ReadAll(old); err != nil || string(b) != valid {
		t.Fatalf("old file descriptor reads %q, %v; want the original content", b, err)
	}
	assertOnlyConfig(t, path)
}

func TestUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s, path := newStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil { //nolint:gosec // the test needs a read-only directory
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) //nolint:gosec // restore for TempDir cleanup
	_, err = s.Replace(etag, []byte("apiVersion: pagewire/v2\nname: Other\n"))
	var el *config.ErrorList
	if err == nil || errors.As(err, &el) || errors.As(err, new(*ErrPreconditionFailed)) {
		t.Fatalf("err = %v, want a plain write error", err)
	}
	assertContent(t, path, valid)
}

// v1 is a pagewire/v1 file; normalized stores migrate it on every write.
const v1 = `apiVersion: pagewire/v1
name: Office # kept
print:
  maxUploadMiB: 20
  defaults:
    sides: two-sided-long-edge
`

func newNormalizingStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pagewire.yaml")
	validate := func(b []byte) error {
		_, err := config.Check(b, config.Known{})
		return err
	}
	migrate := func(b []byte) ([]byte, error) {
		out, _, err := config.Migrate(b)
		return out, err
	}
	return New(path, validate, WithNormalize(migrate)), path
}

// assertV2 checks that the file is the migrated v1 file and etag its etag.
func assertV2(t *testing.T, path, etag string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if etag != sum(b) {
		t.Errorf("etag %s is not the etag of the file on disk %s", etag, sum(b))
	}
	if config.IsV1(b) || !strings.Contains(string(b), "name: Office # kept") {
		t.Fatalf("file is not the migrated v1 file:\n%s", b)
	}
	got, err := config.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Printers) != 1 || got.Printers[0].Defaults.Sides != "two-sided-long-edge" || got.Print.MaxUploadMiB != 20 {
		t.Errorf("migrated config %+v", got)
	}
}

func TestNormalizeCreate(t *testing.T) {
	s, path := newNormalizingStore(t)
	etag, err := s.Create([]byte(v1))
	if err != nil {
		t.Fatal(err)
	}
	assertV2(t, path, etag)
}

func TestNormalizeReplace(t *testing.T) {
	s, path := newNormalizingStore(t)
	etag, err := s.Create([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if etag, err = s.Replace(etag, []byte(v1)); err != nil {
		t.Fatal(err)
	}
	assertV2(t, path, etag)
}

func TestNormalizePatch(t *testing.T) {
	for name, patch := range map[string]string{
		"rename": `{"name":"Home","printers":[{"id":"printer","name":"Home"}]}`,
		"no-op":  `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, path := newNormalizingStore(t)
			if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
				t.Fatal(err)
			}
			_, etag, err := s.Read()
			if err != nil {
				t.Fatal(err)
			}
			raw, etag, err := s.Patch(etag, []byte(patch))
			if err != nil {
				t.Fatal(err)
			}
			assertContent(t, path, string(raw))
			if name == "no-op" {
				assertV2(t, path, etag)
				return
			}
			b, _ := os.ReadFile(path) //nolint:gosec // test temp path
			got, err := config.Parse(b)
			if err != nil {
				t.Fatalf("%v\n%s", err, b)
			}
			if etag != sum(b) || got.Name != "Home" || len(got.Printers) != 1 || got.Printers[0].Name != "Home" ||
				got.Printers[0].Defaults.Sides != "" || len(got.Scanners) != 1 || got.Print.MaxUploadMiB != 20 {
				t.Errorf("patched v1 file:\n%s", b)
			}
		})
	}
}

func TestNormalizeError(t *testing.T) {
	s, path := newNormalizingStore(t)
	bad := "apiVersion: pagewire/v1\nname: Office\nscan:\n  retentionDays: soon\n"
	if _, err := s.Create([]byte(bad)); !errors.As(err, new(*config.ErrorList)) {
		t.Fatalf("err = %v, want the v1 file's *config.ErrorList", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file written despite a normalize error: %v", err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("file = %q, want %q", b, want)
	}
}

func assertOnlyConfig(t *testing.T, path string) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != filepath.Base(path) {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only %s", names, filepath.Base(path))
	}
}
