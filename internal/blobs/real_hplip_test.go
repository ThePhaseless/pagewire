package blobs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/printscan/internal/blobs"
	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/device"
)

// TestRealHPLIPPlugin checks the pinned HP plug-in against local copies of the
// real .run and .asc: the signature with the catalog key, the catalog sha256,
// and the licence text path. It is opt-in, as the files are HP's and never
// part of the repo:
//
//	PRINTSCAN_HPLIP_RUN=/path/hplip-3.24.4-plugin.run PRINTSCAN_HPLIP_ASC=/path/hplip-3.24.4-plugin.run.asc \
//	  go test ./internal/blobs -run TestRealHPLIPPlugin -v
func TestRealHPLIPPlugin(t *testing.T) {
	run, asc := os.Getenv("PRINTSCAN_HPLIP_RUN"), os.Getenv("PRINTSCAN_HPLIP_ASC")
	if run == "" || asc == "" {
		t.Skip("set PRINTSCAN_HPLIP_RUN and PRINTSCAN_HPLIP_ASC to local copies of HP's plug-in and signature")
	}
	b, ok := catalog.Default().Blob(id)
	if !ok {
		t.Fatal("catalog has no hplip-plugin blob")
	}
	e, ok := catalog.EULA(b.EULA)
	if !ok {
		t.Fatalf("catalog has no EULA %q", b.EULA)
	}
	keys, err := catalog.BlobKey(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(run) //nolint:gosec // path from the opt-in env var
	if err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(asc) //nolint:gosec // path from the opt-in env var
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(keys, bytes.NewReader(data), bytes.NewReader(sig), nil); err != nil {
		t.Fatalf("signature: %v", err)
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	t.Logf("archive sha256 %s", got)
	if got != b.Verify.SHA256 {
		t.Errorf("archive sha256 %s, catalog pins %q", got, b.Verify.SHA256)
	}
	text, err := blobs.ExtractFile(run, e.Text.Path, blobs.MaxTextBytes)
	if err != nil {
		t.Fatal(err)
	}
	textSHA := blobs.TextSHA256(text)
	t.Logf("licence %s: %d bytes, sha256 %s", e.Text.Path, len(text), textSHA)

	// The full install path with both checks, served from the local copies.
	v := newVendor(t, data, sig)
	b.URL = v.srv.URL + "/plugin.run"
	b.Verify.PGPSignatureURL = v.srv.URL + "/plugin.run.asc"
	for _, goarch := range []string{"amd64", "arm64"} {
		st := blobs.Fetch(context.Background(), b, []device.EULARef{{ID: e.ID, Version: e.Version, TextSHA256: textSHA}},
			filepath.Join(t.TempDir(), "blobs"), blobs.Options{Client: v.srv.Client(), Keys: keys, GOARCH: goarch})
		if st.State != device.BlobInstalled {
			t.Errorf("%s install: %+v", goarch, st)
		}
	}
}
