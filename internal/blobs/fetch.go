// Package blobs fetches proprietary vendor blobs at runtime, only for blobs
// whose EULA the user accepted, verifies them and installs them atomically
// into the blob volume. Nothing proprietary is ever shipped.
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
	"os"
	"path/filepath"
	"runtime"
	"strings"

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

const (
	versionFile     = ".printscan-version"
	defaultMaxBytes = 64 << 20
	maxSigBytes     = 64 << 10
)

// Fetch installs blob b into dir/<b.ID> and reports its state. A disabled blob
// or one whose EULA {id, version} is not in accepted makes no network request.
// accepted refs match on ID and Version only. An install whose
// dir/<id>/.printscan-version already holds the version is a no-op.
func Fetch(ctx context.Context, b catalog.Blob, accepted []device.BlobRef, dir string, opt Options) device.BlobStatus {
	st := device.BlobStatus{ID: b.ID}
	eula, eulaOK := catalog.EULA(b.EULA)
	st.Version = eula.Version
	set := func(state, msg string) device.BlobStatus {
		st.State, st.Message = state, msg
		return st
	}
	switch {
	case !b.Enabled:
		if b.DisabledMessage != "" {
			return set(device.BlobDisabled, b.DisabledMessage)
		}
		return set(device.BlobDisabled, b.ID+" is not yet supported")
	case !eulaOK:
		return set(device.BlobFailed, fmt.Sprintf("unknown EULA %q", b.EULA))
	case !isAccepted(accepted, b.ID, eula.Version):
		return set(device.BlobMissing, fmt.Sprintf("EULA %s %s not accepted", b.EULA, eula.Version))
	}
	final := filepath.Join(dir, b.ID)
	if v, err := os.ReadFile(filepath.Join(final, versionFile)); err == nil && strings.TrimSpace(string(v)) == eula.Version { //nolint:gosec // path built from the catalog id
		return set(device.BlobInstalled, "")
	}
	if err := install(ctx, b, eula.Version, dir, opt); err != nil {
		return set(device.BlobFailed, err.Error())
	}
	return set(device.BlobInstalled, "")
}

func isAccepted(accepted []device.BlobRef, id, version string) bool {
	for _, a := range accepted {
		if a.ID == id && a.Version == version {
			return true
		}
	}
	return false
}

func install(ctx context.Context, b catalog.Blob, version, dir string, opt Options) error {
	if b.Verify.SHA256 == "" {
		if b.Verify.PGPSignatureURL == "" {
			return errors.New("no verification configured")
		}
		if len(opt.Keys) == 0 {
			return errors.New("no verification key")
		}
	}
	client := opt.Client
	if client == nil {
		client = http.DefaultClient
	}
	goarch := opt.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	maxBytes := b.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	tmp := filepath.Join(dir, ".tmp", b.ID)
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil { //nolint:gosec // the blob volume is read by other uids
		return err
	}
	defer func() { _ = os.RemoveAll(filepath.Join(dir, ".tmp")) }()

	download := filepath.Join(tmp, "download")
	if err := get(ctx, client, b.URL, download, maxBytes); err != nil {
		return err
	}
	if err := verify(ctx, client, b.Verify, download, opt.Keys); err != nil {
		return err
	}

	staged := filepath.Join(dir, b.ID+".new")
	if err := os.RemoveAll(staged); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := os.MkdirAll(staged, 0o755); err != nil { //nolint:gosec // read by the driver and scanner containers
		return err
	}
	switch b.Format {
	case "hplip-plugin":
		if err := installHPLIP(download, filepath.Join(tmp, "payload"), staged, goarch, version, maxBytes*extractRatio); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown blob format %q", b.Format)
	}
	if err := os.WriteFile(filepath.Join(staged, versionFile), []byte(version+"\n"), 0o644); err != nil { //nolint:gosec // world-readable by design
		return err
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
	f, err := os.Create(path) //nolint:gosec // path inside the blob dir's .tmp
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

func verify(ctx context.Context, client *http.Client, v catalog.Verify, path string, keys openpgp.EntityList) error {
	f, err := os.Open(path) //nolint:gosec // path inside the blob dir's .tmp
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
		return nil
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
