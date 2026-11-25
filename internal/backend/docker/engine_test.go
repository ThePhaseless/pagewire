package docker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// TestNewClientTCPPodman lists containers over tcp:// (the socket-proxy path)
// from an engine that reports API 1.41, Podman's compat version.
func TestNewClientTCPPodman(t *testing.T) {
	for _, k := range []string{"DOCKER_HOST", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
		t.Setenv(k, "")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", "1.41")
		switch {
		case r.URL.Path == "/_ping":
			_, _ = w.Write([]byte("OK"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1.41/containers/json":
			if !strings.Contains(r.URL.Query().Get("filters"), LabelManagedBy) {
				http.Error(w, "missing label filter", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"Id":"abc","Names":["/pagewire-device-agent"],"State":"running"}]`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r, err := c.ContainerList(t.Context(), client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", LabelManagedBy+"="+ManagedBy),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Items) != 1 || r.Items[0].ID != "abc" || r.Items[0].State != "running" {
		t.Errorf("items %+v", r.Items)
	}
}

func TestPlainRemote(t *testing.T) {
	for _, c := range []struct {
		host string
		tls  bool
		want bool
	}{
		{"unix:///var/run/docker.sock", false, false},
		{"tcp://127.0.0.1:2375", false, false},
		{"tcp://[::1]:2375", false, false},
		{"tcp://localhost:2375", false, false},
		{"tcp://192.0.2.1:2375", false, true},
		{"tcp://docker.example:2375", false, true},
		{"tcp://192.0.2.1:2376", true, false},
	} {
		if got := plainRemote(c.host, c.tls); got != c.want {
			t.Errorf("plainRemote(%q, %v) = %v, want %v", c.host, c.tls, got, c.want)
		}
	}
}

func TestNewClientWarnsPlainRemote(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://192.0.2.1:2375")
	t.Setenv("DOCKER_CERT_PATH", "")
	logs := captureLogs(t)
	c, err := NewClient("")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if !strings.Contains(logs.String(), "plain TCP") {
		t.Errorf("no warning for a plain remote host; logs: %s", logs)
	}
	logs2 := captureLogs(t)
	c, err = NewClient("tcp://127.0.0.1:2375")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if logs2.String() != "" {
		t.Errorf("warned for loopback: %s", logs2)
	}
}
