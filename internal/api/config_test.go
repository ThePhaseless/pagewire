package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/config/store"
)

const validFile = `# Living room MFP
apiVersion: pagewire/v2
name: Living room printer

# where it stands
printers:
  - id: p1
    name: Living room printer
    mode: simulated
    location: Hall # line comment
`

const mergePatch = "application/merge-patch+json"

func TestGetConfigMissing(t *testing.T) {
	e := newEnv(t)
	wantProblem(t, e.do("GET", "/api/v1/config", nil), 404, "config-missing")
	wantProblem(t, e.do("GET", "/api/v1/config/raw", nil), 404, "config-missing")
}

func TestGetConfigApplied(t *testing.T) {
	e := newEnv(t)
	e.writeFile(validFile)
	rec := e.do("GET", "/api/v1/config", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("ETag"); got != quote(e.etag()) {
		t.Fatalf("ETag %q", got)
	}
	resp := decode[apitypes.ConfigResponse](t, rec.Body.Bytes())
	if resp.Etag != e.etag() || resp.State != "applied" || resp.Config == nil || len(resp.Config.Printers) != 1 || resp.Config.Printers[0].Location != "Hall" || resp.Errors == nil {
		t.Fatalf("response %+v", resp)
	}
}

func TestGetConfigRaw(t *testing.T) {
	e := newEnv(t)
	e.writeFile(validFile)
	rec := e.do("GET", "/api/v1/config/raw", nil)
	if rec.Code != 200 || rec.Body.String() != validFile {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h := rec.Header()
	if !strings.HasPrefix(h.Get("Content-Type"), "text/yaml") || h.Get("ETag") != quote(e.etag()) ||
		!strings.Contains(h.Get("Content-Disposition"), "pagewire.yaml") {
		t.Fatalf("headers %v", h)
	}
}

func TestPutConfigPreconditions(t *testing.T) {
	e := newEnv(t)
	body := `{"apiVersion":"pagewire/v2","name":"Office"}`
	wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(body)), 428, "precondition-required")
	wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-None-Match", `"abc"`), 400, "bad-request")
	// Replace while the file is missing: the 412's re-read finds no file.
	wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-Match", `"abc"`), 404, "config-missing")

	rec := e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-None-Match", "*")
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	created := decode[apitypes.ConfigResponse](t, rec.Body.Bytes())
	if created.State != "applied" || created.Config.Name != "Office" || rec.Header().Get("ETag") != quote(created.Etag) {
		t.Fatalf("created %+v", created)
	}
	raw, _ := os.ReadFile(e.path)
	if !strings.HasPrefix(string(raw), yamlHeader) || !strings.Contains(string(raw), "name: Office") {
		t.Fatalf("file:\n%s", raw)
	}

	// A second create: 412 with the file on disk.
	rec = e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v2","name":"Other"}`), "If-None-Match", "*")
	if rec.Code != 412 {
		t.Fatalf("double create: %d", rec.Code)
	}
	if r := decode[apitypes.ConfigResponse](t, rec.Body.Bytes()); r.Config == nil || r.Config.Name != "Office" || r.Etag != created.Etag {
		t.Fatalf("412 body %+v", r)
	}

	// Replace with the current ETag (quoted, as the UI sends it), then a stale one.
	rec = e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v2","name":"Office 2"}`), "If-Match", quote(created.Etag))
	if rec.Code != 200 {
		t.Fatalf("replace: %d %s", rec.Code, rec.Body)
	}
	rec = e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-Match", quote(created.Etag))
	if rec.Code != 412 {
		t.Fatalf("stale replace: %d", rec.Code)
	}
	if r := decode[apitypes.ConfigResponse](t, rec.Body.Bytes()); r.State != "applied" || r.Config.Name != "Office 2" {
		t.Fatalf("412 body %+v", r)
	}
}

func TestPutConfigBadBodies(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{`not json`, `[1,2]`, `name: yaml`, `"x"`} {
		wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-None-Match", "*"), 400, "bad-request")
	}
	p := wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v2","name":"x","bogus":true}`),
		"If-None-Match", "*"), 422, "invalid-config")
	if len(p.Errors) == 0 || p.Errors[0].Line == 0 || p.Errors[0].Column == 0 {
		t.Fatalf("422 without position: %+v", p.Errors)
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an invalid PUT wrote the file")
	}
}

func TestPutConfigRoundTripsTrickyStrings(t *testing.T) {
	for _, loc := range []string{"2", "null", "~", "yes", "0x1F"} {
		e := newEnv(t)
		body := `{"apiVersion":"pagewire/v2","name":"x","printers":[{"id":"p1","name":"P","location":"` + loc + `"}]}`
		if rec := e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-None-Match", "*"); rec.Code != 201 {
			t.Fatalf("location %q: %d %s", loc, rec.Code, rec.Body)
		}
		if got := decode[apitypes.ConfigResponse](t, e.do("GET", "/api/v1/config", nil).Body.Bytes()).Config; got == nil || got.Printers[0].Location != loc {
			t.Fatalf("location %q came back as %+v", loc, got)
		}
	}
	e := newEnv(t)
	body := `{"apiVersion":"pagewire/v2","name":"true",` +
		`"printers":[{"id":"p1","name":"P","location":"Room: 1 # top","mode":"usb","usb":{"vendorId":"03f0","productId":"2b17","serial":"0e21"}}],` +
		`"licenses":{"acceptedEulas":[{"id":"hplip-plugin","version":"3.24.4","textSha256":"` + textSHA + `"}]}}`
	if rec := e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-None-Match", "*"); rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := decode[apitypes.ConfigResponse](t, e.do("GET", "/api/v1/config", nil).Body.Bytes()).Config
	want := config.Default("true")
	want.Printers = []config.Printer{{
		ID: "p1", Name: "P", Location: "Room: 1 # top", Enabled: true, Mode: config.ModeUSB, Driver: config.Auto,
		USB: &config.USB{VendorID: "03f0", ProductID: "2b17", Serial: "0e21"},
	}}
	want.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
	if got == nil || !reflect.DeepEqual(*got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// TestPatchWizardFile patches the file a wizard PUT writes, the way Settings does.
func TestPatchWizardFile(t *testing.T) {
	e := newEnv(t)
	body, err := json.Marshal(config.Default("Office"))
	if err != nil {
		t.Fatal(err)
	}
	if rec := e.do("PUT", "/api/v1/config", bytes.NewReader(body), "If-None-Match", "*"); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	want := config.Default("Office")
	for _, step := range []struct {
		patch string
		apply func(*config.Config)
	}{
		{`{"licenses":{"acceptedEulas":[{"id":"hplip-plugin","version":"3.24.4","textSha256":"` + textSHA + `"}]}}`, func(c *config.Config) {
			c.Licenses.AcceptedEULAs = []config.EULARef{{ID: "hplip-plugin", Version: "3.24.4", TextSHA256: textSHA}}
		}},
		{`{"printers":[{"id":"p1","name":"Office","defaults":{"media":"na_letter_8.5x11in"}}]}`, func(c *config.Config) {
			c.Printers = []config.Printer{{
				ID: "p1", Name: "Office", Enabled: true, Mode: config.ModeUSB, Driver: config.Auto,
				Defaults: config.PrintDefaults{Media: "na_letter_8.5x11in"},
			}}
		}},
		{`{"scanners":[{"id":"s1","name":"Office scanner","usb":{"vendorId":"03f0","productId":"2b17","serial":"0e21"}}]}`, func(c *config.Config) {
			c.Scanners = []config.Scanner{{
				ID: "s1", Name: "Office scanner", Enabled: true, Mode: config.ModeUSB, Backend: config.BackendAuto,
				USB: &config.USB{VendorID: "03f0", ProductID: "2b17", Serial: "0e21"},
			}}
		}},
		{`{"scanners":[]}`, func(c *config.Config) { c.Scanners = []config.Scanner{} }},
	} {
		rec := e.do("PATCH", "/api/v1/config", strings.NewReader(step.patch), "If-Match", quote(e.etag()), "Content-Type", mergePatch)
		if rec.Code != 200 {
			t.Fatalf("PATCH %s: %d %s", step.patch, rec.Code, rec.Body)
		}
		step.apply(&want)
		got := decode[apitypes.ConfigResponse](t, e.do("GET", "/api/v1/config", nil).Body.Bytes()).Config
		if got == nil || !reflect.DeepEqual(*got, want) {
			t.Fatalf("after %s:\ngot  %+v\nwant %+v", step.patch, got, want)
		}
	}
}

func TestPatchConfig(t *testing.T) {
	e := newEnv(t)
	wantProblem(t, e.do("PATCH", "/api/v1/config", strings.NewReader(`{}`), "Content-Type", mergePatch), 428, "precondition-required")
	wantProblem(t, e.do("PATCH", "/api/v1/config", strings.NewReader(`{}`), "If-Match", `"x"`, "Content-Type", "application/json"),
		415, "unsupported-media-type")
	wantProblem(t, e.do("PATCH", "/api/v1/config", strings.NewReader(`{}`), "If-Match", `"x"`, "Content-Type", mergePatch), 404, "config-missing")

	e.writeFile(validFile)
	etag := e.etag()
	wantProblem(t, e.do("PATCH", "/api/v1/config", strings.NewReader(`[1]`), "If-Match", quote(etag), "Content-Type", mergePatch),
		400, "bad-request")
	p := wantProblem(t, e.do("PATCH", "/api/v1/config", strings.NewReader(`{"print":{"maxUploadMiB":9999}}`),
		"If-Match", quote(etag), "Content-Type", mergePatch), 422, "invalid-config")
	if len(p.Errors) != 1 || p.Errors[0].Path != "/print/maxUploadMiB" || p.Errors[0].Line == 0 {
		t.Fatalf("422 errors %+v", p.Errors)
	}

	rec := e.do("PATCH", "/api/v1/config", strings.NewReader(`{"name":"Kitchen"}`), "If-Match", quote(etag), "Content-Type", mergePatch)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decode[apitypes.ConfigResponse](t, rec.Body.Bytes())
	if resp.Config.Name != "Kitchen" || resp.Etag == etag || rec.Header().Get("ETag") != quote(resp.Etag) {
		t.Fatalf("response %+v", resp)
	}
	raw, _ := os.ReadFile(e.path)
	for _, keep := range []string{"# Living room MFP", "# where it stands", "# line comment", "\n\n"} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("lost %q:\n%s", keep, raw)
		}
	}
}

func TestPreconditionFailedBodies(t *testing.T) {
	e := newEnv(t)
	e.writeFile(validFile)
	stale := e.etag()
	e.writeFile(strings.Replace(validFile, "Hall", "Attic", 1))

	// Stale ETag against a valid file: the file's config, state applied.
	rec := e.do("PATCH", "/api/v1/config", strings.NewReader(`{"name":"x"}`), "If-Match", quote(stale), "Content-Type", mergePatch)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decode[apitypes.ConfigResponse](t, rec.Body.Bytes())
	if r.State != "applied" || r.Config == nil || r.Config.Printers[0].Location != "Attic" || r.Etag != e.etag() || len(r.Errors) != 0 {
		t.Fatalf("412 valid body %+v", r)
	}
	validateDef(t, "ConfigResponse", rec.Body.Bytes())

	// Against an invalid file: config null, the errors with positions, lastGood.
	good := config.Default("Last good")
	e.mu.Lock()
	e.lastGood = &good
	e.mu.Unlock()
	e.writeFile("apiVersion: pagewire/v1\nname: x\nbogus: 1\n")
	rec = e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v2","name":"x"}`), "If-Match", quote(stale))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r = decode[apitypes.ConfigResponse](t, rec.Body.Bytes())
	if r.State != "invalid" || r.Config != nil || r.LastGood == nil || r.LastGood.Name != "Last good" ||
		len(r.Errors) == 0 || r.Errors[0].Line != 3 || r.Errors[0].Column == 0 {
		t.Fatalf("412 invalid body %+v", r)
	}
	validateDef(t, "ConfigResponse", rec.Body.Bytes())
}

// failingStore has no file and fails every write.
type failingStore struct{}

var errReadOnly = errors.New("read-only file system")

func (failingStore) Read() ([]byte, string, error)                { return nil, "", store.ErrNotExist }
func (failingStore) Create([]byte) (string, error)                { return "", errReadOnly }
func (failingStore) Replace(string, []byte) (string, error)       { return "", errReadOnly }
func (failingStore) Patch(string, []byte) ([]byte, string, error) { return nil, "", errReadOnly }

func TestConfigStorageError(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.Store = failingStore{} })
	p := wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v2","name":"x"}`), "If-None-Match", "*"),
		500, "storage-error")
	// The raw error goes to the log, not to the client.
	if strings.Contains(p.Detail, "read-only") || p.Detail == "" {
		t.Fatalf("detail %q, want a generic message", p.Detail)
	}
	wantProblem(t, e.do("PATCH", "/api/v1/config", strings.NewReader(`{"name":"y"}`), "If-Match", `"x"`, "Content-Type", mergePatch),
		500, "storage-error")
}

func TestCrossOriginWriteRefused(t *testing.T) {
	e := newEnv(t)
	rec := e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v2","name":"x"}`),
		"If-None-Match", "*", "Sec-Fetch-Site", "cross-site")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin PUT: %d", rec.Code)
	}
	rec = e.do("POST", "/api/v1/scans", strings.NewReader(`{}`), "Origin", "https://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", rec.Code)
	}
	if _, err := os.Stat(e.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cross-origin PUT wrote the file")
	}
}

// TestPutV1ConfigStoresV2: a v1 body is migrated before it is written, so the
// returned ETag is that of the v2 file and the next PATCH needs no reload.
func TestPutV1ConfigStoresV2(t *testing.T) {
	e := newEnv(t)
	body := `{"apiVersion":"pagewire/v1","name":"Office","location":"Hall","device":{"mode":"simulated"},` +
		`"print":{"defaults":{"colorMode":"monochrome"}}}`
	rec := e.do("PUT", "/api/v1/config", strings.NewReader(body), "If-None-Match", "*")
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decode[apitypes.ConfigResponse](t, rec.Body.Bytes())
	raw, err := os.ReadFile(e.path)
	if err != nil {
		t.Fatal(err)
	}
	if !config.IsV1([]byte(body)) || config.IsV1(raw) || !strings.Contains(string(raw), "apiVersion: pagewire/v2") {
		t.Fatalf("file on disk is not v2:\n%s", raw)
	}
	if resp.Etag != store.Etag(raw) || rec.Header().Get("ETag") != quote(store.Etag(raw)) {
		t.Fatalf("etag %s, want the sha256 of the v2 file %s", resp.Etag, store.Etag(raw))
	}
	if c := resp.Config; c == nil || len(c.Printers) != 1 || c.Printers[0].Location != "Hall" || c.Printers[0].Defaults.ColorMode != "monochrome" {
		t.Fatalf("config %+v", resp.Config)
	}
	rec = e.do("PATCH", "/api/v1/config", strings.NewReader(`{"name":"Kitchen"}`), "If-Match", quote(resp.Etag), "Content-Type", mergePatch)
	if rec.Code != 200 {
		t.Fatalf("PATCH after a v1 PUT: %d %s", rec.Code, rec.Body)
	}
	// Replace with a v1 body too.
	cur := decode[apitypes.ConfigResponse](t, rec.Body.Bytes()).Etag
	rec = e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v1","name":"Office 2"}`), "If-Match", quote(cur))
	raw, _ = os.ReadFile(e.path)
	if rec.Code != 200 || config.IsV1(raw) || decode[apitypes.ConfigResponse](t, rec.Body.Bytes()).Etag != store.Etag(raw) {
		t.Fatalf("v1 replace: %d %s\n%s", rec.Code, rec.Body, raw)
	}
	// A v1 body that is not valid v1 answers 422 with its errors.
	wantProblem(t, e.do("PUT", "/api/v1/config", strings.NewReader(`{"apiVersion":"pagewire/v1","name":"x","device":{"mode":"bogus"}}`),
		"If-Match", quote(store.Etag(raw))), 422, "invalid-config")
}

// TestGetConfigEmptyDefaults: entries without defaults keys answer with no
// empty-string keys, so the generated client schema parses them.
func TestGetConfigEmptyDefaults(t *testing.T) {
	e := newEnv(t)
	e.writeFile("apiVersion: pagewire/v2\nname: Home\nprinters:\n  - id: p1\n    name: P\n    mode: simulated\n" +
		"scanners:\n  - id: s1\n    name: S\n    mode: simulated\n    enabled: false\n")
	rec := e.do("GET", "/api/v1/config", nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	validateDef(t, "ConfigResponse", rec.Body.Bytes())
	if b := rec.Body.String(); hasEmptyKey(b) || strings.Count(b, `"defaults":{}`) != 2 || !strings.Contains(b, `"enabled":false`) {
		t.Fatalf("body %s", b)
	}
}

// hasEmptyKey reports a key other than location (never omitted) holding "".
func hasEmptyKey(body string) bool {
	return strings.Contains(strings.ReplaceAll(body, `"location":""`, ""), `:""`)
}
