package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

const scanBody = `{"scanner":"s1","source":"platen","resolution":300,"colorMode":"color","format":"pdf"}`

// samsungScanCaps is the Samsung's scanner as AirSane reports it: no duplex, no intents.
var samsungScanCaps = apitypes.ScannerCaps{
	Sources: []string{"platen", "adf"}, Resolutions: []int{75, 150, 300, 600}, ColorModes: []string{"color", "grayscale"}, ADF: true,
}

func TestScanner(t *testing.T) {
	e := newEnv(t)
	e.scans.caps = apitypes.ScannerCaps{Sources: []string{"platen"}, Resolutions: []int{300}}
	rec := e.do("GET", "/api/v1/scanners/s1/capabilities", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"colorModes":[]`) || !strings.Contains(rec.Body.String(), `"intents":[]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wantProblem(t, e.do("GET", "/api/v1/scanners/nope/capabilities", nil), 404, "scanner-not-found")
	e.scans.capsErr = scanning.ErrNoScanner
	wantProblem(t, e.do("GET", "/api/v1/scanners/s1/capabilities", nil), 503, "scanner-unavailable")
	e.scans.capsErr = errors.New("connection refused")
	wantProblem(t, e.do("GET", "/api/v1/scanners/s1/capabilities", nil), 503, "scanner-unavailable")
}

func TestPostScan(t *testing.T) {
	e := newEnv(t)
	e.scans.caps = samsungScanCaps
	rec := e.do("POST", "/api/v1/scans", strings.NewReader(scanBody))
	if sc := decode[apitypes.Scan](t, rec.Body.Bytes()); rec.Code != 202 || sc.State != "scanning" || sc.Scanner != "s1" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(`{"source":`)), 400, "bad-request")
	wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(`{"source":"platen","x":1}`)), 400, "bad-request")
	wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(`{"scanner":"s1","source":"platen","resolution":5,"colorMode":"color","format":"pdf"}`)),
		422, "invalid-settings")
	for err, want := range map[error]struct {
		code int
		typ  string
	}{
		scanning.ErrBusy: {409, "scanner-busy"},
		&scanning.StorageError{Err: os.ErrPermission}: {500, "storage-error"},
		scanning.ErrNoScanner:                         {503, "scanner-unavailable"},
		scanning.ErrStorageFull:                       {507, "storage-full"},
	} {
		e.scans.startErr = err
		p := wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(scanBody)), want.code, want.typ)
		if strings.Contains(p.Detail, "permission") {
			t.Errorf("storage error detail %q leaks the raw error", p.Detail)
		}
	}
}

func TestScanReads(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", "/api/v1/scans", nil); rec.Code != 200 || rec.Body.String() != "[]" {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
	id := "00000000000000aa"
	e.scans.scans = []apitypes.Scan{{ID: id, State: "completed", Pages: 1, Format: "pdf", CreatedAt: time.Now()}}
	if err := os.WriteFile(filepath.Join(e.scans.pageDir, id+"-1.jpg"), []byte("\xff\xd8\xffjpeg"), 0o600); err != nil {
		t.Fatal(err)
	}

	if rec := e.do("GET", "/api/v1/scans/"+id, nil); rec.Code != 200 {
		t.Fatalf("get: %d", rec.Code)
	}
	wantProblem(t, e.do("GET", "/api/v1/scans/00000000000000ff", nil), 404, "not-found")

	rec := e.do("GET", "/api/v1/scans/"+id+"/pages/1", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Body.String() != "\xff\xd8\xffjpeg" ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("page: %d %v", rec.Code, rec.Header())
	}
	wantProblem(t, e.do("GET", "/api/v1/scans/"+id+"/pages/2", nil), 404, "not-found")
	wantProblem(t, e.do("GET", "/api/v1/scans/"+id+"/pages/x", nil), 404, "not-found")

	rec = e.do("GET", "/api/v1/scans/"+id+"/download", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/pdf" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), `filename="scan.pdf"`) || !strings.HasPrefix(rec.Body.String(), "%PDF-") ||
		rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download: %d %v", rec.Code, rec.Header())
	}
	e.scans.dlErr = scanning.ErrNotFinished
	wantProblem(t, e.do("GET", "/api/v1/scans/"+id+"/download", nil), 409, "scan-not-finished")
	e.scans.dlErr = scanning.ErrNotFound
	wantProblem(t, e.do("GET", "/api/v1/scans/"+id+"/download", nil), 404, "not-found")
}

func TestDeleteScan(t *testing.T) {
	e := newEnv(t)
	id := "00000000000000aa"
	e.scans.scans = []apitypes.Scan{{ID: id, State: "completed", Format: "pdf", CreatedAt: time.Now()}}
	if rec := e.do("DELETE", "/api/v1/scans/"+id, nil); rec.Code != 204 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wantProblem(t, e.do("DELETE", "/api/v1/scans/00000000000000ff", nil), 404, "not-found")
}

func TestDeleteRunningScanCancels(t *testing.T) {
	e := newEnv(t)
	id := "00000000000000aa"
	e.scans.scans = []apitypes.Scan{{
		ID: id, State: "scanning", Format: "pdf", CreatedAt: time.Now(),
		Settings: apitypes.ScanSettings{Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"},
	}}
	e.scans.running = id
	rec := e.do("DELETE", "/api/v1/scans/"+id, nil)
	if rec.Code != 202 {
		t.Fatalf("DELETE running: %d %s", rec.Code, rec.Body)
	}
	validateDef(t, "Scan", rec.Body.Bytes())
	if sc := decode[apitypes.Scan](t, rec.Body.Bytes()); sc.ID != id || sc.State != "scanning" {
		t.Fatalf("202 body %+v, want the scan as it is now", sc)
	}
	if e.scans.canceled != 1 {
		t.Fatalf("Cancel called %d times", e.scans.canceled)
	}
	// Once it has ended (canceled), a second DELETE removes it.
	e.scans.running = ""
	if rec := e.do("DELETE", "/api/v1/scans/"+id, nil); rec.Code != 204 {
		t.Fatalf("DELETE after cancel: %d %s", rec.Code, rec.Body)
	}
	wantProblem(t, e.do("DELETE", "/api/v1/scans/00000000000000ff", nil), 404, "not-found")
}

func TestPostScanUnknownScanner(t *testing.T) {
	e := newEnv(t)
	e.scans.caps = samsungScanCaps
	for _, id := range []string{"nope", ""} {
		body := strings.Replace(scanBody, `"s1"`, `"`+id+`"`, 1)
		wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(body)), 404, "scanner-not-found")
	}
	if len(e.scans.List()) != 0 {
		t.Fatal("a scan started on an unknown scanner")
	}
}

func TestPostScanChecksCaps(t *testing.T) {
	e := newEnv(t)
	e.scans.caps = samsungScanCaps
	for _, tc := range []struct{ body, path string }{
		{`{"scanner":"s1","source":"platen","resolution":300,"colorMode":"color","format":"pdf","intent":"poster"}`, ""}, // enum
		{`{"scanner":"s1","source":"platen","resolution":300,"colorMode":"color","format":"pdf","intent":"photo"}`, "/intent"},
		{`{"scanner":"s1","source":"adf-duplex","resolution":300,"colorMode":"color","format":"pdf"}`, "/source"},
		{`{"scanner":"s1","source":"platen","resolution":1200,"colorMode":"color","format":"pdf"}`, "/resolution"},
		{`{"scanner":"s1","source":"platen","resolution":300,"colorMode":"blackandwhite","format":"pdf"}`, "/colorMode"},
	} {
		p := wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(tc.body)), 422, "invalid-settings")
		if tc.path != "" && (len(p.Errors) != 1 || p.Errors[0].Path != tc.path) {
			t.Errorf("%s: errors %+v, want one at %s", tc.body, p.Errors, tc.path)
		}
	}
	if len(e.scans.List()) != 0 {
		t.Fatal("an unsupported scan started")
	}
	// Supported: adf with grayscale at 600 dpi.
	if rec := e.do("POST", "/api/v1/scans", strings.NewReader(`{"scanner":"s1","source":"adf","resolution":600,"colorMode":"grayscale","format":"jpeg"}`)); rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPostScanCapsUnavailable(t *testing.T) {
	e := newEnv(t)
	e.scans.capsErr = errors.New("connection refused")
	body := `{"scanner":"s1","source":"adf-duplex","resolution":300,"colorMode":"color","format":"pdf","intent":"photo"}`
	if rec := e.do("POST", "/api/v1/scans", strings.NewReader(body)); rec.Code != 202 {
		t.Fatalf("caps unavailable: %d %s, want the scan started", rec.Code, rec.Body)
	}
}
