// Package blobs fetches proprietary vendor blobs at runtime. It stages and
// verifies a vendor archive so its own licence text can be shown before
// acceptance, and installs the blob atomically into the blob volume only for
// an accepted EULA whose recorded text sha256 matches. Nothing proprietary is
// ever shipped.
package blobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/device"
)

// Options carries what Fetch needs beyond the catalog entry.
type Options struct {
	Client *http.Client       // nil: http.DefaultClient
	Keys   openpgp.EntityList // verification keys for PGP-verified blobs
	GOARCH string             // "": runtime.GOARCH
}

// MaxTextBytes caps a EULA text extracted from a blob; a longer text fails.
const MaxTextBytes = 256 << 10

const (
	versionFile     = ".printscan-version"
	textSHAFile     = ".printscan-text-sha256"
	stagingDir      = ".staging"
	defaultMaxBytes = 64 << 20
	maxSigBytes     = 64 << 10

	// MsgTextChanged is the failed message when the archive's licence text is
	// not the one the user accepted.
	MsgTextChanged = "the licence text differs from the one accepted; review and accept it again in Settings"
)

// Fetch installs blob b into dir/<b.ID> and reports its state. A disabled blob
// or one whose EULA {id, version} is not in accepted makes no network request.
// accepted refs match b.EULA and the catalog version. An install is a no-op
// when dir/<id>/.printscan-version holds the version and
// dir/<id>/.printscan-text-sha256 holds the accepted text sha256; otherwise the
// archive is staged (a staged copy is reused after re-verifying it), and it is
// installed only when the sha256 of its licence text equals the accepted one.
func Fetch(ctx context.Context, b catalog.Blob, accepted []device.EULARef, dir string, opt Options) device.BlobStatus {
	st := device.BlobStatus{ID: b.ID}
	eula, eulaOK := catalog.EULA(b.EULA)
	st.Version = eula.Version
	set := func(state, msg string) device.BlobStatus {
		st.State, st.Message = state, msg
		return st
	}
	i := slices.IndexFunc(accepted, func(a device.EULARef) bool { return a.ID == b.EULA && a.Version == eula.Version })
	switch {
	case !b.Enabled:
		if b.DisabledMessage != "" {
			return set(device.BlobDisabled, b.DisabledMessage)
		}
		return set(device.BlobDisabled, b.ID+" is not yet supported")
	case !eulaOK:
		return set(device.BlobFailed, fmt.Sprintf("unknown EULA %q", b.EULA))
	case i < 0:
		return set(device.BlobMissing, fmt.Sprintf("EULA %s %s not accepted", b.EULA, eula.Version))
	}
	want := accepted[i].TextSHA256
	final := filepath.Join(dir, b.ID)
	if marker(final, versionFile) == eula.Version && want != "" && marker(final, textSHAFile) == want {
		return set(device.BlobInstalled, "")
	}
	if err := install(ctx, b, eula, want, dir, opt); err != nil {
		return set(device.BlobFailed, err.Error())
	}
	cleanStaging(dir, b.ID, eula.Version)
	return set(device.BlobInstalled, "")
}

// marker reads one install marker file, "" when it is missing.
func marker(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // path built from the catalog id
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Text returns the licence text of EULA e from the verified archive of blob
// b, staging the archive in dir first. b is the blob named by e.Text.Blob.
func Text(ctx context.Context, b catalog.Blob, e catalog.EULAInfo, dir string, opt Options) ([]byte, error) {
	run, err := Stage(ctx, b, e.Version, dir, opt)
	if err != nil {
		return nil, err
	}
	return readText(b, run, e.Text.Path)
}

func readText(b catalog.Blob, run, name string) ([]byte, error) {
	if b.Format != "hplip-plugin" {
		return nil, fmt.Errorf("unknown blob format %q", b.Format)
	}
	text, err := ExtractFile(run, name, MaxTextBytes)
	if err != nil {
		return nil, err
	}
	// The UI shows the text as JSON; invalid UTF-8 would not show as hashed.
	if !utf8.Valid(text) {
		return nil, fmt.Errorf("licence text %s is not UTF-8", name)
	}
	return text, nil
}

// TextSHA256 is the lowercase hex sha256 of a licence text, the value a
// config's acceptedEulas entry records.
func TextSHA256(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])
}

// Stage returns the path of the verified archive of blob b at
// dir/.staging/<id>/<version>/<basename of the URL>. An archive already there
// is re-verified and reused; otherwise, or when it fails the checks, it is
// downloaded to a temporary name beside it, verified and renamed into place,
// so a reader never sees a partial or unverified file.
func Stage(ctx context.Context, b catalog.Blob, version, dir string, opt Options) (string, error) {
	switch v := b.Verify; {
	case v.SHA256 == "" && v.PGPSignatureURL == "":
		return "", errors.New("no verification configured")
	case v.PGPSignatureURL != "" && len(opt.Keys) == 0:
		return "", errors.New("no verification key")
	}
	client := opt.Client
	if client == nil {
		client = http.DefaultClient
	}
	maxBytes := b.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	sdir := filepath.Join(dir, stagingDir, b.ID, version)
	final := filepath.Join(sdir, archiveName(b.URL))
	if _, err := os.Stat(final); err == nil && verify(ctx, client, b.Verify, final, opt.Keys) == nil {
		return final, nil
	}
	if err := os.MkdirAll(sdir, 0o755); err != nil { //nolint:gosec // the blob volume is read by other uids
		return "", err
	}
	// shortcut: a process killed mid-download leaves its .download-* file until
	// the next install of another version cleans the version directory.
	tmp, err := os.CreateTemp(sdir, ".download-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(name) }() // fails harmlessly after the rename
	if err := get(ctx, client, b.URL, name, maxBytes); err != nil {
		return "", err
	}
	if err := verify(ctx, client, b.Verify, name, opt.Keys); err != nil {
		return "", err
	}
	if err := os.Rename(name, final); err != nil {
		return "", err
	}
	return final, nil
}

func archiveName(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		if base := path.Base(u.Path); base != "." && base != "/" && filepath.IsLocal(base) {
			return base
		}
	}
	return "archive"
}

// cleanStaging removes the staged archives of every version but keep.
func cleanStaging(dir, id, keep string) {
	entries, _ := os.ReadDir(filepath.Join(dir, stagingDir, id))
	for _, e := range entries {
		if e.Name() != keep {
			_ = os.RemoveAll(filepath.Join(dir, stagingDir, id, e.Name()))
		}
	}
}

func install(ctx context.Context, b catalog.Blob, eula catalog.EULAInfo, wantText, dir string, opt Options) error {
	goarch := opt.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	maxBytes := b.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	run, err := Stage(ctx, b, eula.Version, dir, opt)
	if err != nil {
		return err
	}
	text, err := readText(b, run, eula.Text.Path)
	if err != nil {
		return err
	}
	if TextSHA256(text) != wantText {
		// An older install must not stay in use without a matching acceptance.
		_ = os.RemoveAll(filepath.Join(dir, b.ID))
		return errors.New(MsgTextChanged)
	}

	tmp := filepath.Join(dir, ".tmp", b.ID)
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil { //nolint:gosec // the blob volume is read by other uids
		return err
	}
	defer func() { _ = os.RemoveAll(filepath.Join(dir, ".tmp")) }()

	staged := filepath.Join(dir, b.ID+".new")
	if err := os.RemoveAll(staged); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := os.MkdirAll(staged, 0o755); err != nil { //nolint:gosec // read by the driver and scanner containers
		return err
	}
	if err := installHPLIP(run, filepath.Join(tmp, "payload"), staged, goarch, eula.Version, maxBytes*extractRatio); err != nil {
		return err
	}
	for name, v := range map[string]string{versionFile: eula.Version, textSHAFile: wantText} {
		if err := os.WriteFile(filepath.Join(staged, name), []byte(v+"\n"), 0o644); err != nil { //nolint:gosec // world-readable by design
			return err
		}
	}
	return swap(staged, filepath.Join(dir, b.ID))
}

// swap replaces final with staged. Linux cannot rename over a non-empty
// directory, so the old tree moves aside first and comes back on failure.
func swap(staged, final string) error {
	old := final + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	hadOld := true
	if err := os.Rename(final, old); errors.Is(err, os.ErrNotExist) {
		hadOld = false
	} else if err != nil {
		return err
	}
	if err := os.Rename(staged, final); err != nil {
		if hadOld {
			_ = os.Rename(old, final)
		}
		return err
	}
	return os.RemoveAll(old)
}

// get downloads url into path, failing once the body exceeds maxBytes.
func get(ctx context.Context, client *http.Client, url, path string, maxBytes int64) error {
	body, err := open(ctx, client, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	f, err := os.Create(path) //nolint:gosec // path inside the blob dir
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(body, maxBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxBytes {
		return fmt.Errorf("download %s is over the %d byte limit", url, maxBytes)
	}
	return nil
}

func open(ctx context.Context, client *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// verify checks every configured check on the file at path: the sha256 and
// the detached PGP signature. When both are set, both must pass.
func verify(ctx context.Context, client *http.Client, v catalog.Verify, path string, keys openpgp.EntityList) error {
	if v.SHA256 == "" && v.PGPSignatureURL == "" {
		return errors.New("no verification configured")
	}
	f, err := os.Open(path) //nolint:gosec // path inside the blob dir
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if v.SHA256 != "" {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, v.SHA256) {
			return fmt.Errorf("sha256 mismatch: got %s, want %s", got, strings.ToLower(v.SHA256))
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
	if v.PGPSignatureURL == "" {
		return nil
	}
	if len(keys) == 0 {
		return errors.New("no verification key")
	}
	body, err := open(ctx, client, v.PGPSignatureURL)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	sig, err := io.ReadAll(io.LimitReader(body, maxSigBytes+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", v.PGPSignatureURL, err)
	}
	if len(sig) > maxSigBytes {
		return fmt.Errorf("signature %s is over the %d byte limit", v.PGPSignatureURL, maxSigBytes)
	}
	// HP signs with DSA-1024 over SHA-1. The v1 openpgp API accepts both (only
	// openpgp/v2 applies the Reject* policies); TestDSASHA1Signature pins that.
	if _, err := openpgp.CheckArmoredDetachedSignature(keys, f, bytes.NewReader(sig), nil); err != nil {
		return fmt.Errorf("signature check failed: %w", err)
	}
	return nil
}
