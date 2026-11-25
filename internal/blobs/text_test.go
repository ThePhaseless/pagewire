package blobs_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/blobs/blobtest"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// runWith builds a makeself archive whose payload holds the given tar entries.
func runWith(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := strings.Repeat("x", int(h.Size))
		if h.Typeflag != tar.TypeReg {
			h.Size, body = 0, ""
		}
		h.Mode = 0o644
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return blobtest.Makeself(buf.Bytes())
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plugin.run")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractFile(t *testing.T) {
	run := writeTemp(t, blobtest.Archive("arm64"))
	for _, name := range []string{"license.txt", "./license.txt"} {
		got, err := blobs.ExtractFile(run, name, blobs.MaxTextBytes)
		if err != nil || string(got) != blobtest.License {
			t.Errorf("ExtractFile(%q) = %q, %v", name, got, err)
		}
	}
	for name, tc := range map[string]struct {
		run  []byte
		file string
		max  int64
		want string
	}{
		"missing":    {blobtest.Archive("arm64"), "nope.txt", blobs.MaxTextBytes, "no nope.txt"},
		"oversize":   {blobtest.Archive("arm64"), "license.txt", 4, "limit"},
		"unsafe":     {blobtest.Archive("arm64"), "../license.txt", blobs.MaxTextBytes, "unsafe"},
		"symlink":    {runWith(t, tar.Header{Typeflag: tar.TypeSymlink, Name: "./license.txt", Linkname: "/etc/passwd"}), "license.txt", blobs.MaxTextBytes, "not a regular file"},
		"not gzip":   {blobtest.Makeself([]byte("plain")), "license.txt", blobs.MaxTextBytes, "gzip"},
		"over limit": {runWith(t, tar.Header{Typeflag: tar.TypeReg, Name: "./license.txt", Size: blobs.MaxTextBytes + 1}), "license.txt", blobs.MaxTextBytes, "limit"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := blobs.ExtractFile(writeTemp(t, tc.run), tc.file, tc.max)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func eula(t *testing.T) catalog.EULAInfo {
	t.Helper()
	e, ok := catalog.EULA(id)
	if !ok {
		t.Fatal("no hplip-plugin EULA")
	}
	return e
}

func TestText(t *testing.T) {
	f := setup(t, "arm64")
	opt := blobs.Options{Client: f.v.srv.Client(), Keys: openpgp.EntityList{f.key}}
	text, err := blobs.Text(context.Background(), f.v.blob(t), eula(t), f.dir, opt)
	if err != nil || string(text) != blobtest.License {
		t.Fatalf("Text = %q, %v", text, err)
	}
	staged := filepath.Join(f.dir, ".staging", id, version(t), "plugin.run")
	if b, err := os.ReadFile(staged); err != nil || !bytes.Equal(b, f.v.files["/plugin.run"]) { //nolint:gosec // test temp dir
		t.Fatalf("staged archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging installed the blob: %v", err)
	}

	// A restart re-verifies the staged archive: only the signature is fetched.
	hits := f.v.hits.Load()
	if _, err := blobs.Text(context.Background(), f.v.blob(t), eula(t), f.dir, opt); err != nil {
		t.Fatal(err)
	}
	if n := f.v.hits.Load() - hits; n != 1 {
		t.Errorf("re-staging made %d requests, want 1 (the signature)", n)
	}

	// A tampered staged archive is replaced by a fresh verified download.
	if err := os.WriteFile(staged, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if text, err := blobs.Text(context.Background(), f.v.blob(t), eula(t), f.dir, opt); err != nil || string(text) != blobtest.License {
		t.Fatalf("after tampering: %q, %v", text, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(staged))
	if len(entries) != 1 {
		t.Errorf("staging dir holds %v, want only the archive", entries)
	}
}

func TestTextVerificationFailureStagesNothing(t *testing.T) {
	a := blobtest.Archive("arm64")
	v := newVendor(t, a, blobtest.Sign(blobtest.NewKey(), a))
	dir := t.TempDir()
	_, err := blobs.Text(context.Background(), v.blob(t), eula(t), dir, blobs.Options{
		Client: v.srv.Client(), Keys: openpgp.EntityList{blobtest.NewKey()},
	})
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("err = %v, want a signature failure", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".staging", id, version(t)))
	if len(entries) != 0 {
		t.Errorf("unverified download left behind: %v", entries)
	}
}

// TestBothChecksRequired: with a sha256 and a signature configured, passing
// one of them is not enough.
func TestBothChecksRequired(t *testing.T) {
	f := setup(t, "arm64")
	sum := sha256.Sum256(f.v.files["/plugin.run"])
	good := hex.EncodeToString(sum[:])
	for name, tc := range map[string]struct {
		sha  string
		keys openpgp.EntityList
		want string
	}{
		"good sha256, bad signature": {good, openpgp.EntityList{blobtest.NewKey()}, "signature"},
		"bad sha256, good signature": {strings.Repeat("0", 64), openpgp.EntityList{f.key}, "sha256"},
		"good sha256, no key":        {good, nil, "no verification key"},
	} {
		t.Run(name, func(t *testing.T) {
			b := f.v.blob(t)
			b.Verify.SHA256 = tc.sha
			st := blobs.Fetch(context.Background(), b, accepted(t), filepath.Join(t.TempDir(), "blobs"), blobs.Options{
				Client: f.v.srv.Client(), Keys: tc.keys, GOARCH: "arm64",
			})
			wantState(t, st, device.BlobFailed, tc.want)
		})
	}
	b := f.v.blob(t)
	b.Verify.SHA256 = good
	wantState(t, f.fetch(t, b, accepted(t), "arm64"), device.BlobInstalled, "")
}

func TestNoVerificationConfigured(t *testing.T) {
	f := setup(t, "arm64")
	b := f.v.blob(t)
	b.Verify = catalog.Verify{}
	wantState(t, f.fetch(t, b, accepted(t), "arm64"), device.BlobFailed, "no verification configured")
}

func TestTextMismatchFails(t *testing.T) {
	for name, sha := range map[string]string{"other text": strings.Repeat("a", 64), "empty": ""} {
		t.Run(name, func(t *testing.T) {
			f := setup(t, "arm64")
			st := f.fetch(t, f.v.blob(t), []device.EULARef{{ID: id, Version: version(t), TextSHA256: sha}}, "arm64")
			wantState(t, st, device.BlobFailed, blobs.MsgTextChanged)
			if _, err := os.Stat(filepath.Join(f.dir, id)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("installed despite a text mismatch: %v", err)
			}
		})
	}
}

// TestMarkerRecheck: an install with the right version but a missing or
// different text-sha marker is re-checked: installed again on a match,
// failed (and removed) on a mismatch.
func TestMarkerRecheck(t *testing.T) {
	for name, marker := range map[string]*string{"missing marker": nil, "different marker": ptr(strings.Repeat("b", 64) + "\n")} {
		t.Run(name, func(t *testing.T) {
			prepare := func(t *testing.T) fixture {
				f := setup(t, "arm64")
				wantState(t, f.fetch(t, f.v.blob(t), accepted(t), "arm64"), device.BlobInstalled, "")
				p := filepath.Join(f.dir, id, ".pagewire-text-sha256")
				if marker == nil {
					_ = os.Remove(p)
				} else if err := os.WriteFile(p, []byte(*marker), 0o600); err != nil {
					t.Fatal(err)
				}
				return f
			}
			t.Run("match installs", func(t *testing.T) {
				f := prepare(t)
				hits := f.v.hits.Load()
				wantState(t, f.fetch(t, f.v.blob(t), accepted(t), "arm64"), device.BlobInstalled, "")
				if f.v.hits.Load() == hits {
					t.Error("no re-check: the install was trusted without its text marker")
				}
				got, _ := os.ReadFile(filepath.Join(f.dir, id, ".pagewire-text-sha256")) //nolint:gosec // test temp dir
				if strings.TrimSpace(string(got)) != licenseSHA {
					t.Errorf("marker = %q after the re-check", got)
				}
			})
			t.Run("mismatch fails", func(t *testing.T) {
				f := prepare(t)
				other := []device.EULARef{{ID: id, Version: version(t), TextSHA256: strings.Repeat("c", 64)}}
				wantState(t, f.fetch(t, f.v.blob(t), other, "arm64"), device.BlobFailed, blobs.MsgTextChanged)
				if _, err := os.Stat(filepath.Join(f.dir, id)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("old install kept after a text mismatch: %v", err)
				}
			})
		})
	}
}

func ptr[T any](v T) *T { return &v }

func TestReusesStagedArchive(t *testing.T) {
	f := setup(t, "arm64")
	opt := blobs.Options{Client: f.v.srv.Client(), Keys: openpgp.EntityList{f.key}, GOARCH: "arm64"}
	if _, err := blobs.Text(context.Background(), f.v.blob(t), eula(t), f.dir, opt); err != nil {
		t.Fatal(err)
	}
	delete(f.v.files, "/plugin.run") // a second download would now 404
	wantState(t, f.fetch(t, f.v.blob(t), accepted(t), "arm64"), device.BlobInstalled, "")
}

func TestCleansOldStagingVersions(t *testing.T) {
	f := setup(t, "arm64")
	old := filepath.Join(f.dir, ".staging", id, "1.0.0")
	if err := os.MkdirAll(old, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(old, "plugin.run"), []byte("old"), 0o600)
	wantState(t, f.fetch(t, f.v.blob(t), accepted(t), "arm64"), device.BlobInstalled, "")
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old staging version kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".staging", id, version(t), "plugin.run")); err != nil {
		t.Errorf("current staging removed: %v", err)
	}
}
