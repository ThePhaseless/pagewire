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

	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/config/yamledit"
)

const valid = `apiVersion: printscan/v1
# the printer's name
name: Office   # shown in the UI

location: Desk
`

func check(b []byte) error {
	_, err := config.Parse(b)
	return err
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "printscan.yaml")
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
	_, err := s.Create([]byte("apiVersion: printscan/v1\n"))
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
	const invalid = "apiVersion: printscan/v1\n"
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
	other := "apiVersion: printscan/v1\nname: Other\n"
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
		contents = append(contents, "apiVersion: printscan/v1\nname: P\n# "+strings.Repeat("x", i*400)+"\n")
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
	_, err = s.Replace(etag, []byte("apiVersion: printscan/v1\nname: Other\n"))
	var el *config.ErrorList
	if err == nil || errors.As(err, &el) || errors.As(err, new(*ErrPreconditionFailed)) {
		t.Fatalf("err = %v, want a plain write error", err)
	}
	assertContent(t, path, valid)
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
