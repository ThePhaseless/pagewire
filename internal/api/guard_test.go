package api

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ThePhaseless/pagewire/internal/device"
)

func TestAllowedHosts(t *testing.T) {
	e := newEnv(t, func(d *Deps) {
		d.UI = uiFS
		d.AllowedHosts = []string{"localhost", "127.0.0.1", "[::1]", " Pagewire.Example "}
		d.Agent = Agent{Tokens: tokenSource{token: "secret"}, Report: func(_ context.Context, _ device.AgentReport) (device.DesiredState, error) {
			return device.DesiredState{}, nil
		}}
	})
	get := func(path, host string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), "GET", path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		e.public.ServeHTTP(rec, req)
		return rec
	}
	for _, host := range []string{"localhost", "LOCALHOST:1", "127.0.0.1:8080", "[::1]:8080", "[::1]", "pagewire.example:443"} {
		if rec := get("/api/v1/status", host); rec.Code != 200 {
			t.Errorf("Host %q: %d %s", host, rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"/api/v1/status", "/api/v1/events", "/", "/settings"} {
		rec := get(path, "evil.example")
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s from evil.example: %d, want 421", path, rec.Code)
			continue
		}
		wantProblem(t, rec, 421, "misdirected-request")
	}
	for _, host := range []string{"evil.example:8080", "localhost.evil.example", "127.0.0.2"} {
		if rec := get("/api/v1/status", host); rec.Code != 421 {
			t.Errorf("Host %q: %d, want 421", host, rec.Code)
		}
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := get(path, "evil.example"); rec.Code != 200 {
			t.Errorf("%s from evil.example: %d, want 200", path, rec.Code)
		}
	}
	// The internal listener has no Host check.
	req := httptest.NewRequestWithContext(t.Context(), "POST", device.AgentReportPath, strings.NewReader(`{}`))
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	e.internal.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("internal report with a foreign Host: %d %s", rec.Code, rec.Body)
	}
}

func TestNoAllowedHostsChecksNothing(t *testing.T) {
	e := newEnv(t)
	req := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/status", nil)
	req.Host = "anything.example"
	rec := httptest.NewRecorder()
	e.public.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestProxySecret(t *testing.T) {
	for name, header := range map[string]string{"default header": "", "custom header": "X-Auth-Secret"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(d *Deps) {
				d.UI = uiFS
				d.ProxySecret = "s3cret"
				d.ProxySecretHeader = header
			})
			if header == "" {
				header = "X-Pagewire-Proxy-Secret"
			}
			for _, path := range []string{"/api/v1/status", "/api/v1/events", "/api/v1/nope"} {
				wantProblem(t, e.do("GET", path, nil), 401, "unauthorized")
				wantProblem(t, e.do("GET", path, nil, header, "wrong"), 401, "unauthorized")
				wantProblem(t, e.do("GET", path, nil, header, "s3cret2"), 401, "unauthorized")
			}
			if rec := e.do("GET", "/api/v1/status", nil, header, "s3cret"); rec.Code != 200 {
				t.Fatalf("with the secret: %d %s", rec.Code, rec.Body)
			}
			for _, path := range []string{"/healthz", "/readyz", "/", "/print"} {
				if rec := e.do("GET", path, nil); rec.Code != 200 {
					t.Errorf("%s without the secret: %d", path, rec.Code)
				}
			}
		})
	}
}

// TestReadDeadlines sends a request whose body never arrives and expects the
// handler to give up after its read deadline.
func TestReadDeadlines(t *testing.T) {
	e := newEnv(t, func(d *Deps) { d.readTimeout, d.printReadTimeout = 100*time.Millisecond, 200*time.Millisecond })
	srv := httptest.NewServer(e.public)
	t.Cleanup(srv.Close)
	for _, req := range []string{
		"PUT /api/v1/config HTTP/1.1\r\nHost: x\r\nIf-None-Match: *\r\nContent-Length: 100\r\n\r\n{",
		"PATCH /api/v1/config HTTP/1.1\r\nHost: x\r\nIf-Match: *\r\nContent-Type: application/merge-patch+json\r\nContent-Length: 100\r\n\r\n{",
		"POST /api/v1/scans HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n{",
		"POST /api/v1/printers/p1/print HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 1000\r\n\r\n--b\r\n",
	} {
		start := time.Now()
		var d net.Dialer
		conn, err := d.DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		line, _, _ := strings.Cut(req, "\r\n")
		if err != nil {
			t.Errorf("%s: no response before the client deadline: %v", line, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 || time.Since(start) > 3*time.Second {
			t.Errorf("%s: %d after %v", line, resp.StatusCode, time.Since(start))
		}
	}
	if defaultReadTimeout != 30*time.Second || defaultPrintReadTimeout != 10*time.Minute {
		t.Errorf("read timeouts %v and %v, want 30 s and 10 min", defaultReadTimeout, defaultPrintReadTimeout)
	}
}
