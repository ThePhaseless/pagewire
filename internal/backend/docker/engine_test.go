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
			_, _ = w.Write([]byte(`[{"Id":"abc","Names":["/printscan-device-agent"],"State":"running"}]`))
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
