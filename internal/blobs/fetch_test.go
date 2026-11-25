package blobs_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/dsa" //nolint:staticcheck // HP signs its plug-in with DSA; the test builds such a key.
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/ThePhaseless/printscan/internal/blobs"
	"github.com/ThePhaseless/printscan/internal/blobs/blobtest"
	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/device"
)

const id = "hplip-plugin"

// version is the catalog EULA version, which Fetch matches accepted refs on.
func version(t *testing.T) string {
	t.Helper()
	b, ok := catalog.Default().Blob(id)
	if !ok {
		t.Fatal("catalog has no hplip-plugin blob")
	}
	e, ok := catalog.EULA(b.EULA)
	if !ok {
		t.Fatalf("catalog has no EULA %q", b.EULA)
	}
	return e.Version
}

// vendor serves files from an httptest server and counts requests.
type vendor struct {
	srv   *httptest.Server
	files map[string][]byte
	hits  atomic.Int64
}

func newVendor(t *testing.T, archive, sig []byte) *vendor {
	t.Helper()
	v := &vendor{files: map[string][]byte{"/plugin.run": archive, "/plugin.run.asc": sig}}
	v.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.hits.Add(1)
		b, ok := v.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(v.srv.Close)
	return v
}

// blob is the real catalog entry, enabled and pointed at the test vendor.
func (v *vendor) blob(t *testing.T) catalog.Blob {
	t.Helper()
	b, _ := catalog.Default().Blob(id)
	b.Enabled = true
	b.URL = v.srv.URL + "/plugin.run"
	b.Verify = catalog.Verify{PGPSignatureURL: v.srv.URL + "/plugin.run.asc"}
	return b
}

type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// noNetwork is a client whose transport fails the test when called.
func noNetwork(t *testing.T) *http.Client {
	return &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected request to %s", r.URL)
		return nil, errors.New("no network in this test")
	})}
}

// licenseSHA is the text sha256 of blobtest.License, the value acceptance records.
var licenseSHA = blobs.TextSHA256([]byte(blobtest.License))

func accepted(t *testing.T) []device.EULARef {
	return []device.EULARef{{ID: id, Version: version(t), TextSHA256: licenseSHA}}
}

type fixture struct {
	key *openpgp.Entity
	v   *vendor
	dir string
}

// setup serves Archive(goarch) signed with a fresh key.
func setup(t *testing.T, goarch string) fixture {
	t.Helper()
	key := blobtest.NewKey()
	a := blobtest.Archive(goarch)
	return fixture{key: key, v: newVendor(t, a, blobtest.Sign(key, a)), dir: filepath.Join(t.TempDir(), "blobs")}
}

func (f fixture) fetch(t *testing.T, b catalog.Blob, acc []device.EULARef, goarch string) device.BlobStatus {
	t.Helper()
	return blobs.Fetch(context.Background(), b, acc, f.dir, blobs.Options{
		Client: f.v.srv.Client(), Keys: openpgp.EntityList{f.key}, GOARCH: goarch,
	})
}

func wantState(t *testing.T, st device.BlobStatus, state, msgPart string) {
	t.Helper()
	if st.State != state || !strings.Contains(st.Message, msgPart) {
		t.Fatalf("status = %+v, want state %q with message containing %q", st, state, msgPart)
	}
	if st.ID != id {
		t.Errorf("status id = %q, want %q", st.ID, id)
	}
}

func TestNotAccepted(t *testing.T) {
	f := setup(t, "arm64")
	b := f.v.blob(t)
	for name, acc := range map[string][]device.EULARef{
		"none":          nil,
		"wrong version": {{ID: id, Version: "0.0.1", TextSHA256: licenseSHA}},
		"other EULA":    {{ID: "other", Version: version(t), TextSHA256: licenseSHA}},
	} {
		t.Run(name, func(t *testing.T) {
			st := blobs.Fetch(context.Background(), b, acc, f.dir, blobs.Options{
				Client: noNetwork(t), Keys: openpgp.EntityList{f.key}, GOARCH: "arm64",
			})
			wantState(t, st, device.BlobMissing, "not accepted")
			if st.Version != version(t) {
				t.Errorf("version = %q, want %q", st.Version, version(t))
			}
		})
	}
}

// TestAcceptedMatchesEULA uses a blob whose id differs from its EULA's, so
// matching accepted refs on the blob id would fail it.
func TestAcceptedMatchesEULA(t *testing.T) {
	f := setup(t, "arm64")
	b := f.v.blob(t)
	b.ID = "renamed-blob"
	opt := blobs.Options{Client: f.v.srv.Client(), Keys: openpgp.EntityList{f.key}, GOARCH: "arm64"}
	st := blobs.Fetch(context.Background(), b, []device.EULARef{{ID: "renamed-blob", Version: version(t), TextSHA256: licenseSHA}}, f.dir, opt)
	if st.State != device.BlobMissing {
		t.Errorf("accepted ref named after the blob id: %+v, want missing", st)
	}
	st = blobs.Fetch(context.Background(), b, accepted(t), f.dir, opt)
	if st.State != device.BlobInstalled || st.ID != "renamed-blob" {
		t.Errorf("accepted ref named after the EULA: %+v, want installed", st)
	}
}

func TestSuccessLayout(t *testing.T) {
	for goarch, arch := range map[string]string{"arm64": "arm64", "amd64": "x86_64"} {
		t.Run(goarch, func(t *testing.T) {
			f := setup(t, goarch)
			wantState(t, f.fetch(t, f.v.blob(t), accepted(t), goarch), device.BlobInstalled, "")
			root := filepath.Join(f.dir, id)
			for _, p := range []string{
				"prnt/plugins/lj-" + arch + ".so",
				"scan/plugins/bb_soapht-" + arch + ".so",
				"fax/plugins/fax_marvell-" + arch + ".so",
				"data/firmware/hp_laserjet_1018.fw.gz",
			} {
				fi, err := os.Lstat(filepath.Join(root, p))
				if err != nil {
					t.Fatal(err)
				}
				if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o755 {
					t.Errorf("%s: mode %v, want regular 0755", p, fi.Mode())
				}
			}
			for link, target := range map[string]string{
				"prnt/plugins/lj.so":         "lj-" + arch + ".so",
				"scan/plugins/bb_soapht.so":  "bb_soapht-" + arch + ".so",
				"fax/plugins/fax_marvell.so": "fax_marvell-" + arch + ".so",
			} {
				got, err := os.Readlink(filepath.Join(root, link))
				if err != nil || got != target {
					t.Errorf("%s -> %q (%v), want %q", link, got, err, target)
				}
			}
			state, err := os.ReadFile(filepath.Join(root, "hplip.state")) //nolint:gosec // test temp dir
			if err != nil {
				t.Fatal(err)
			}
			if want := "[plugin]\ninstalled = 1\neula = 1\nversion = " + version(t) + "\n\n"; string(state) != want {
				t.Errorf("hplip.state = %q, want %q", state, want)
			}
			ver, err := os.ReadFile(filepath.Join(root, ".printscan-version")) //nolint:gosec // test temp dir
			if err != nil || strings.TrimSpace(string(ver)) != version(t) {
				t.Errorf(".printscan-version = %q (%v)", ver, err)
			}
			// The udev rule targets $RULESDIR, which no image links to the blob volume.
			_ = filepath.WalkDir(f.dir, func(p string, _ os.DirEntry, _ error) error {
				if strings.HasSuffix(p, ".rules") {
					t.Errorf("udev rule installed at %s", p)
				}
				return nil
			})
			// Driver and scanner containers read the tree as another uid.
			_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err != nil {
					t.Error(err)
					return nil
				}
				fi, _ := d.Info()
				want := os.FileMode(0o004)
				if d.IsDir() {
					want = 0o005
				}
				if d.Type()&os.ModeSymlink == 0 && fi.Mode().Perm()&want != want {
					t.Errorf("%s: mode %v is not world-readable", p, fi.Mode())
				}
				return nil
			})
			if txt, err := os.ReadFile(filepath.Join(root, ".printscan-text-sha256")); err != nil || strings.TrimSpace(string(txt)) != licenseSHA { //nolint:gosec // test temp dir
				t.Errorf(".printscan-text-sha256 = %q (%v), want %s", txt, err, licenseSHA)
			}
			// The verified archive stays staged for the next start; nothing else.
			entries, _ := os.ReadDir(f.dir)
			for _, e := range entries {
				if e.Name() != id && e.Name() != ".staging" {
					t.Errorf("leftover %s in blob dir", e.Name())
				}
			}
			staged, _ := os.ReadDir(filepath.Join(f.dir, ".staging", id, version(t)))
			if len(staged) != 1 || staged[0].Name() != "plugin.run" {
				t.Errorf("staging dir holds %v, want only plugin.run", staged)
			}
		})
	}
}

// TestMakeselfHeaderVariants covers the header shapes of makeself releases:
// 2.1 and 2.2 write offset=`head -n N "$0" ...` (payload after N lines), 2.4
// and later write skip="N" near the top with the same meaning, and 2.0 used
// skip="N" with tail +N (payload from line N). The payload has no newline
// byte, so reading one line too far would hit EOF.
func TestMakeselfHeaderVariants(t *testing.T) {
	payload := noNewlinePayload(t)
	header := func(lines ...string) []byte { return []byte(strings.Join(lines, "\n") + "\n") }
	for name, run := range map[string][]byte{
		"2.1 offset head -n": blobtest.Makeself(payload),
		"2.4 skip=": append(header("#!/bin/sh", "# Makeself 2.4.5", `label="x"`, `skip="6"`,
			`offset=`+"`"+`head -n "$skip" "$0" | wc -c | sed "s/ //g"`+"`", "exit 0"), payload...),
		"2.0 skip= tail +N": append(header("#!/bin/sh", `skip="4"`, `tail +$skip "$0" | tar xzf -`), payload...),
	} {
		t.Run(name, func(t *testing.T) {
			key := blobtest.NewKey()
			v := newVendor(t, run, blobtest.Sign(key, run))
			st := blobs.Fetch(context.Background(), v.blob(t), accepted(t), filepath.Join(t.TempDir(), "blobs"), blobs.Options{
				Client: v.srv.Client(), Keys: openpgp.EntityList{key}, GOARCH: "arm64",
			})
			wantState(t, st, device.BlobInstalled, "")
		})
	}
}

// noNewlinePayload builds a minimal valid plug-in payload whose gzip bytes
// contain no '\n', retrying with different file contents until one does.
func noNewlinePayload(t *testing.T) []byte {
	t.Helper()
	for i := 0; i < 10000; i++ {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, f := range []struct{ name, body string }{
			{"./plugin.spec", "[products]\np=lj\n[lj]\nsrc=lj-$ARCH.so\ntrg=$HOMEDIR/prnt/plugins/lj-$ARCH.so\n"},
			{"./lj-arm64.so", fmt.Sprint("lib ", i)},
			{"./license.txt", blobtest.License},
		} {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: 0o644, Size: int64(len(f.body))})
			_, _ = tw.Write([]byte(f.body))
		}
		_ = tw.Close()
		_ = gz.Close()
		if bytes.IndexByte(buf.Bytes(), '\n') < 0 {
			return buf.Bytes()
		}
	}
	t.Fatal("no newline-free payload found")
	return nil
}

func TestIdempotent(t *testing.T) {
	f := setup(t, "arm64")
	b := f.v.blob(t)
	wantState(t, f.fetch(t, b, accepted(t), "arm64"), device.BlobInstalled, "")
	hits := f.v.hits.Load()
	st := blobs.Fetch(context.Background(), b, accepted(t), f.dir, blobs.Options{
		Client: noNetwork(t), Keys: openpgp.EntityList{f.key}, GOARCH: "arm64",
	})
	wantState(t, st, device.BlobInstalled, "")
	if f.v.hits.Load() != hits {
		t.Errorf("re-run made %d requests", f.v.hits.Load()-hits)
	}
}

func TestReplacesOtherVersion(t *testing.T) {
	f := setup(t, "arm64")
	old := filepath.Join(f.dir, id)
	if err := os.MkdirAll(filepath.Join(old, "prnt/plugins"), 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(old, ".printscan-version"), []byte("1.0.0\n"), 0o600)
	_ = os.WriteFile(filepath.Join(old, "prnt/plugins/stale.so"), nil, 0o600)
	wantState(t, f.fetch(t, f.v.blob(t), accepted(t), "arm64"), device.BlobInstalled, "")
	if _, err := os.Stat(filepath.Join(old, "prnt/plugins/stale.so")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale file survived the upgrade: %v", err)
	}
}

func TestBadSignature(t *testing.T) {
	a := blobtest.Archive("arm64")
	v := newVendor(t, a, blobtest.Sign(blobtest.NewKey(), a))
	dir := filepath.Join(t.TempDir(), "blobs")
	st := blobs.Fetch(context.Background(), v.blob(t), accepted(t), dir, blobs.Options{
		Client: v.srv.Client(), Keys: openpgp.EntityList{blobtest.NewKey()}, GOARCH: "arm64",
	})
	wantState(t, st, device.BlobFailed, "signature")
	if _, err := os.Stat(filepath.Join(dir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob installed despite a bad signature: %v", err)
	}
}

func TestNoVerificationKey(t *testing.T) {
	f := setup(t, "arm64")
	st := blobs.Fetch(context.Background(), f.v.blob(t), accepted(t), f.dir, blobs.Options{
		Client: noNetwork(t), GOARCH: "arm64",
	})
	wantState(t, st, device.BlobFailed, "no verification key")
}

func TestSHA256(t *testing.T) {
	f := setup(t, "arm64")
	sum := sha256.Sum256(f.v.files["/plugin.run"])
	b := f.v.blob(t)
	opt := blobs.Options{Client: f.v.srv.Client(), GOARCH: "arm64"}
	b.Verify = catalog.Verify{SHA256: strings.Repeat("0", 64)}
	wantState(t, blobs.Fetch(context.Background(), b, accepted(t), f.dir, opt), device.BlobFailed, "sha256")
	b.Verify.SHA256 = strings.ToUpper(hex.EncodeToString(sum[:]))
	wantState(t, blobs.Fetch(context.Background(), b, accepted(t), f.dir, opt), device.BlobInstalled, "")
}

func TestOversize(t *testing.T) {
	f := setup(t, "arm64")
	b := f.v.blob(t)
	b.MaxBytes = int64(len(f.v.files["/plugin.run"]) - 1)
	wantState(t, f.fetch(t, b, accepted(t), "arm64"), device.BlobFailed, "limit")
	if _, err := os.Stat(filepath.Join(f.dir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("oversize blob installed: %v", err)
	}
}

func TestDownloadError(t *testing.T) {
	f := setup(t, "arm64")
	delete(f.v.files, "/plugin.run")
	wantState(t, f.fetch(t, f.v.blob(t), accepted(t), "arm64"), device.BlobFailed, "404")
}

func TestNoBuildForArch(t *testing.T) {
	for _, goarch := range []string{"arm64", "riscv64"} {
		t.Run(goarch, func(t *testing.T) {
			f := setup(t, "amd64")
			wantState(t, f.fetch(t, f.v.blob(t), accepted(t), goarch), device.BlobFailed, "HP plug-in has no "+goarch+" build")
			if _, err := os.Stat(filepath.Join(f.dir, id)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("blob installed without an %s build: %v", goarch, err)
			}
		})
	}
}

// malicious builds a signed .run whose payload holds a valid plugin.spec plus
// the given hostile entry.
func malicious(t *testing.T, key *openpgp.Entity, evil tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	write := func(h tar.Header, body string) {
		h.Size = int64(len(body))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		h.Mode = 0o644
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			_, _ = tw.Write([]byte(body))
		}
	}
	write(tar.Header{Typeflag: tar.TypeReg, Name: "./plugin.spec"}, blobtest.Spec)
	write(tar.Header{Typeflag: tar.TypeReg, Name: "./lj-arm64.so"}, "lib")
	write(tar.Header{Typeflag: tar.TypeReg, Name: "./license.txt"}, blobtest.License)
	write(evil, "evil")
	_ = tw.Close()
	_ = gz.Close()
	return blobtest.Makeself(buf.Bytes())
}

func TestUnsafeArchiveEntries(t *testing.T) {
	for name, h := range map[string]tar.Header{
		"dot-dot":          {Typeflag: tar.TypeReg, Name: "../evil.so"},
		"nested dot-dot":   {Typeflag: tar.TypeReg, Name: "./a/../../evil.so"},
		"absolute":         {Typeflag: tar.TypeReg, Name: "/tmp/printscan-evil.so"},
		"escaping link":    {Typeflag: tar.TypeSymlink, Name: "./bb_soapht-arm64.so", Linkname: "../../evil.so"},
		"absolute link":    {Typeflag: tar.TypeSymlink, Name: "./bb_soapht-arm64.so", Linkname: "/etc/passwd"},
		"hard link":        {Typeflag: tar.TypeLink, Name: "./bb_soapht-arm64.so", Linkname: "/etc/passwd"},
		"character device": {Typeflag: tar.TypeChar, Name: "./dev"},
	} {
		t.Run(name, func(t *testing.T) {
			key := blobtest.NewKey()
			a := malicious(t, key, h)
			v := newVendor(t, a, blobtest.Sign(key, a))
			base := t.TempDir()
			dir := filepath.Join(base, "blobs")
			st := blobs.Fetch(context.Background(), v.blob(t), accepted(t), dir, blobs.Options{
				Client: v.srv.Client(), Keys: openpgp.EntityList{key}, GOARCH: "arm64",
			})
			wantState(t, st, device.BlobFailed, "unsafe")
			entries, _ := os.ReadDir(base)
			if len(entries) != 1 || entries[0].Name() != "blobs" {
				t.Errorf("files outside the blob dir: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(dir, id)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("hostile archive installed: %v", err)
			}
			if _, err := os.Lstat("/tmp/printscan-evil.so"); err == nil {
				t.Error("absolute entry written to /tmp")
			}
		})
	}
}

func TestDisabled(t *testing.T) {
	f := setup(t, "arm64")
	b := f.v.blob(t)
	b.Enabled = false
	opt := blobs.Options{Client: noNetwork(t), Keys: openpgp.EntityList{f.key}, GOARCH: "arm64"}
	b.DisabledMessage = "HP plug-in not yet supported"
	wantState(t, blobs.Fetch(context.Background(), b, accepted(t), f.dir, opt), device.BlobDisabled, "HP plug-in not yet supported")
	b.DisabledMessage = ""
	wantState(t, blobs.Fetch(context.Background(), b, accepted(t), f.dir, opt), device.BlobDisabled, id+" is not yet supported")
}

// TestRealCatalogEntry runs the embedded entry as shipped. Disabled, it must
// report its message without a request; enabled, the real download is out of
// scope for unit tests.
func TestRealCatalogEntry(t *testing.T) {
	b, ok := catalog.Default().Blob(id)
	if !ok {
		t.Fatal("no hplip-plugin blob")
	}
	if b.Enabled {
		t.Skip("hplip-plugin is enabled; the real download is not exercised in unit tests")
	}
	st := blobs.Fetch(context.Background(), b, accepted(t), t.TempDir(), blobs.Options{Client: noNetwork(t)})
	wantState(t, st, device.BlobDisabled, "HP plug-in not yet supported")
}

func TestInMemoryTransport(t *testing.T) {
	key := blobtest.NewKey()
	a := blobtest.Archive("arm64")
	b, _ := catalog.Default().Blob(id)
	b.Enabled = true
	b.URL = "https://vendor.invalid/plugin.run"
	b.Verify = catalog.Verify{PGPSignatureURL: "https://vendor.invalid/plugin.run.asc"}
	client := &http.Client{Transport: blobtest.Transport(map[string][]byte{b.URL: a, b.Verify.PGPSignatureURL: blobtest.Sign(key, a)})}
	st := blobs.Fetch(context.Background(), b, accepted(t), t.TempDir(), blobs.Options{
		Client: client, Keys: openpgp.EntityList{key}, GOARCH: "arm64",
	})
	wantState(t, st, device.BlobInstalled, "")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://vendor.invalid/missing", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown URL: status %d, want 404", resp.StatusCode)
	}
}

// TestDSASHA1Signature pins the signature policy to what HP uses: a DSA key
// and a SHA-1 detached signature, both of which go-crypto's openpgp/v2 rejects by default.
func TestDSASHA1Signature(t *testing.T) {
	key, sign := dsaSigner(t)
	a := blobtest.Archive("arm64")
	v := newVendor(t, a, sign(a))
	st := blobs.Fetch(context.Background(), v.blob(t), accepted(t), filepath.Join(t.TempDir(), "blobs"), blobs.Options{
		Client: v.srv.Client(), Keys: openpgp.EntityList{key}, GOARCH: "arm64",
	})
	wantState(t, st, device.BlobInstalled, "")
}

// dsaSigner builds a DSA-1024 entity like HP's key and a function making
// armored SHA-1 detached signatures with it.
func dsaSigner(t *testing.T) (*openpgp.Entity, func([]byte) []byte) {
	t.Helper()
	var priv dsa.PrivateKey                                                                      //nolint:staticcheck // see the import comment
	if err := dsa.GenerateParameters(&priv.Parameters, rand.Reader, dsa.L1024N160); err != nil { //nolint:staticcheck // as above
		t.Fatal(err)
	}
	if err := dsa.GenerateKey(&priv, rand.Reader); err != nil { //nolint:staticcheck // as above
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Hour)
	pk := packet.NewDSAPrivateKey(now, &priv)
	uid := packet.NewUserId("synthetic DSA vendor", "", "dsa@printscan.invalid")
	primary := true
	self := &packet.Signature{
		Version: 4, SigType: packet.SigTypePositiveCert, PubKeyAlgo: packet.PubKeyAlgoDSA, Hash: crypto.SHA256,
		CreationTime: now, IssuerKeyId: &pk.KeyId, IsPrimaryId: &primary,
		FlagsValid: true, FlagSign: true, FlagCertify: true,
	}
	if err := self.SignUserId(uid.Id, &pk.PublicKey, pk, nil); err != nil {
		t.Fatal(err)
	}
	e := &openpgp.Entity{
		PrimaryKey: &pk.PublicKey, PrivateKey: pk,
		Identities: map[string]*openpgp.Identity{uid.Id: {Name: uid.Id, UserId: uid, SelfSignature: self, Signatures: []*packet.Signature{self}}},
	}
	return e, func(data []byte) []byte {
		sig := &packet.Signature{
			Version: 4, SigType: packet.SigTypeBinary, PubKeyAlgo: packet.PubKeyAlgoDSA, Hash: crypto.SHA1,
			CreationTime: time.Now(), IssuerKeyId: &pk.KeyId,
		}
		h, err := sig.PrepareSign(nil)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(data)
		noNotation := false // salt notations need a hash SHA-1 lacks a salt size for
		if err := sig.Sign(h, pk, &packet.Config{NonDeterministicSignaturesViaNotation: &noNotation}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		w, err := armor.Encode(&out, "PGP SIGNATURE", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := sig.Serialize(w); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		return out.Bytes()
	}
}
