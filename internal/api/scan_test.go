package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/scanning"
)

const scanBody = `{"source":"platen","resolution":300,"colorMode":"color","format":"pdf"}`

func TestScanner(t *testing.T) {
	e := newEnv(t)
	e.scans.caps = apitypes.ScannerCaps{Sources: []string{"platen"}, Resolutions: []int{300}}
	rec := e.do("GET", "/api/v1/scanner", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"colorModes":[]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.scans.capsErr = scanning.ErrNoScanner
	wantProblem(t, e.do("GET", "/api/v1/scanner", nil), 503, "scanner-unavailable")
	e.scans.capsErr = errors.New("connection refused")
	wantProblem(t, e.do("GET", "/api/v1/scanner", nil), 503, "scanner-unavailable")
}

func TestPostScan(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/api/v1/scans", strings.NewReader(scanBody))
	if rec.Code != 202 || decode[apitypes.Scan](t, rec.Body.Bytes()).State != "scanning" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(`{"source":`)), 400, "bad-request")
	wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(`{"source":"platen","x":1}`)), 400, "bad-request")
	wantProblem(t, e.do("POST", "/api/v1/scans", strings.NewReader(`{"source":"platen","resolution":5,"colorMode":"color","format":"pdf"}`)),
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
