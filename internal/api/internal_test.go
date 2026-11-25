package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ThePhaseless/printscan/internal/device"
)

type tokenSource struct {
	token string
	err   error
	calls *int
}

func (s tokenSource) AgentToken(context.Context) (string, error) {
	if s.calls != nil {
		*s.calls++
	}
	return s.token, s.err
}

func agentEnv(t *testing.T, tokens device.AgentTokenSource) (*testEnv, *[]device.AgentReport) {
	t.Helper()
	var got []device.AgentReport
	e := newEnv(t, func(d *Deps) {
		d.Agent = Agent{
			Tokens: tokens,
			Report: func(_ context.Context, r device.AgentReport) (device.DesiredState, error) {
				got = append(got, r)
				return device.DesiredState{Hash: "h"}, nil
			},
		}
	})
	return e, &got
}

func report(e *testEnv, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(e.t.Context(), "POST", device.AgentReportPath, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.internal.ServeHTTP(rec, req)
	return rec
}

func TestAgentReportAuth(t *testing.T) {
	body := `{"podUid":"p1","devicePresent":true}`
	for name, tc := range map[string]struct {
		tokens device.AgentTokenSource
		auth   string
	}{
		"no token source":    {nil, "Bearer secret"},
		"token source error": {tokenSource{err: errors.New("no secret")}, "Bearer secret"},
		"empty token":        {tokenSource{}, "Bearer "},
		"wrong token":        {tokenSource{token: "secret"}, "Bearer wrong"},
		"no header":          {tokenSource{token: "secret"}, ""},
		"wrong scheme":       {tokenSource{token: "secret"}, "Basic secret"},
	} {
		t.Run(name, func(t *testing.T) {
			e, got := agentEnv(t, tc.tokens)
			wantProblem(t, report(e, tc.auth, body), 401, "unauthorized")
			if len(*got) != 0 {
				t.Fatal("report reached the controller")
			}
		})
	}
}

func TestAgentHeaderCheckedBeforeToken(t *testing.T) {
	calls := 0
	e, _ := agentEnv(t, tokenSource{token: "secret", calls: &calls})
	for _, auth := range []string{"", "Bearer", "Basic secret", "Bearer "} {
		wantProblem(t, report(e, auth, `{}`), 401, "unauthorized")
	}
	if calls != 0 {
		t.Fatalf("token fetched %d times for requests without a bearer token", calls)
	}
	wantProblem(t, report(e, "Bearer wrong", `{}`), 401, "unauthorized")
	if calls != 1 {
		t.Fatalf("token fetched %d times, want 1", calls)
	}
}

func TestAgentReport(t *testing.T) {
	e, got := agentEnv(t, tokenSource{token: "secret"})
	rec := report(e, "Bearer secret", `{"podUid":"p1","devicePresent":true}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// DesiredState goes through encodeJSON: Browse is [] even when nil.
	if !strings.Contains(rec.Body.String(), `"browse":[]`) || !strings.Contains(rec.Body.String(), `"hash":"h"`) {
		t.Fatalf("body %s", rec.Body)
	}
	if len(*got) != 1 || (*got)[0].PodUID != "p1" || !(*got)[0].DevicePresent {
		t.Fatalf("reports %+v", *got)
	}
	wantProblem(t, report(e, "Bearer secret", `{"podUid":`), 400, "bad-request")
}

func TestAgentEndpointNotPublic(t *testing.T) {
	e, _ := agentEnv(t, tokenSource{token: "secret"})
	rec := e.do("POST", device.AgentReportPath, strings.NewReader(`{}`), "Authorization", "Bearer secret")
	if rec.Code == 200 {
		t.Fatal("agent endpoint served on the public handler")
	}
}
