package api

import (
	"strings"
	"testing"
	"testing/fstest"
)

var uiFS = fstest.MapFS{
	"index.html":                       {Data: []byte("<!doctype html><title>printscan</title>")},
	"assets/index-qrhwxhtd.js":         {Data: []byte("console.log(1)")},
	"assets/index-qrhwxhtd.js.map":     {Data: []byte("{}")},
	"assets/files/figtree-latin.woff2": {Data: []byte("font")},
	"THIRD_PARTY_LICENSES.txt":         {Data: []byte("licences")},
}

func TestSPA(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = uiFS })
	for path, want := range map[string]struct{ body, cache string }{
		"/":                                 {"<!doctype", "no-cache"},
		"/print":                            {"<!doctype", "no-cache"},
		"/settings/deep/link":               {"<!doctype", "no-cache"},
		"/assets":                           {"<!doctype", "no-cache"},
		"/assets/index-qrhwxhtd.js":         {"console.log", "public, max-age=31536000, immutable"},
		"/assets/index-qrhwxhtd.js.map":     {"{}", "public, max-age=31536000, immutable"},
		"/assets/files/figtree-latin.woff2": {"font", "no-cache"},
		"/THIRD_PARTY_LICENSES.txt":         {"licences", "no-cache"},
	} {
		rec := e.do("GET", path, nil)
		if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), want.body) {
			t.Errorf("%s: %d %q", path, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Cache-Control"); got != want.cache {
			t.Errorf("%s: Cache-Control %q, want %q", path, got, want.cache)
		}
	}
	if rec := e.do("POST", "/", nil); rec.Code != 405 {
		t.Errorf("POST /: %d", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = uiFS })
	for _, path := range []string{"/", "/api/v1/status"} {
		h := e.do("GET", path, nil).Header()
		if h.Get("Content-Security-Policy") != "default-src 'self'; img-src 'self' blob: data:; frame-ancestors 'none'" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", path, h)
		}
	}
}

func TestUINotBuilt(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = fstest.MapFS{".keep": {}} })
	if rec := e.do("GET", "/", nil); rec.Code != 503 || !strings.Contains(rec.Body.String(), "UI not built") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUnknownAPIPath(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.UI = uiFS })
	wantProblem(t, e.do("GET", "/api/v1/nope", nil), 404, "not-found")
}

func TestHealth(t *testing.T) {
	ready := false
	e := newEnv(t, func(d *Deps) { d.Ready = func() bool { return ready } })
	if rec := e.do("GET", "/healthz", nil); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("healthz %d", rec.Code)
	}
	if rec := e.do("GET", "/readyz", nil); rec.Code != 503 {
		t.Fatalf("readyz before ready: %d", rec.Code)
	}
	ready = true
	if rec := e.do("GET", "/readyz", nil); rec.Code != 200 {
		t.Fatalf("readyz: %d", rec.Code)
	}
}
