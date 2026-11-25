package api

import (
	"encoding/json"
	"testing"

	"github.com/ThePhaseless/printscan/internal/apitypes"
)

func TestGetEULA(t *testing.T) {
	t.Run("pending until reported", func(t *testing.T) {
		e := newEnv(t, func(d *Deps) { d.Setup = setupFake{s: emptySetup(t)} })
		rec := e.do("GET", "/api/v1/eulas/hplip-plugin", nil)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["state"] != apitypes.EULAPending || got["text"] != nil || got["textSha256"] != nil || got["version"] != "3.24.4" {
			t.Errorf("body %s", rec.Body)
		}
		for _, k := range []string{"text", "textSha256", "message", "title", "vendor", "summary"} {
			if _, ok := got[k]; !ok {
				t.Errorf("body lacks %q: %s", k, rec.Body)
			}
		}
	})
	t.Run("available", func(t *testing.T) {
		text := "licence\n"
		want := apitypes.EULA{ID: "hplip-plugin", Version: "3.24.4", State: apitypes.EULAAvailable, Text: &text, TextSHA256: &textSHA}
		e := newEnv(t, func(d *Deps) {
			d.Setup = setupFake{s: emptySetup(t), eulas: map[string]apitypes.EULA{"hplip-plugin": want}}
		})
		rec := e.do("GET", "/api/v1/eulas/hplip-plugin", nil)
		got := decode[apitypes.EULA](t, rec.Body.Bytes())
		if rec.Code != 200 || got.Text == nil || *got.Text != text || got.TextSHA256 == nil || *got.TextSHA256 != textSHA {
			t.Errorf("%d %s", rec.Code, rec.Body)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		e := newEnv(t, func(d *Deps) { d.Setup = setupFake{s: emptySetup(t)} })
		rec := e.do("GET", "/api/v1/eulas/no-such-eula", nil)
		p := decode[apitypes.Problem](t, rec.Body.Bytes())
		if rec.Code != 404 || p.Type != "not-found" || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%d %s %+v", rec.Code, rec.Header().Get("Content-Type"), p)
		}
	})
}
