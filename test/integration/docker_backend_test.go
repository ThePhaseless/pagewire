//go:build integration && docker

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/backend/docker"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

// dockerSimContainers are the device containers of simulated mode, in plan order.
var dockerSimContainers = []string{"probe", "dbus", "avahi", "agent", "sim-printer", "airsane"}

// dockerCommonCaps are the capabilities both Docker and Podman grant by
// default. Podman's compat inspect lists the dropped defaults in CapDrop
// instead of ALL, and its default set lacks Docker's MKNOD, NET_RAW and AUDIT_WRITE.
var dockerCommonCaps = []string{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "NET_BIND_SERVICE",
	"SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// TestDockerBackend drives the control plane from compose.docker.yaml
// (--backend=docker) in simulated mode, which needs no USB printer. See
// README.md "Docker backend".
func TestDockerBackend(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
	defer cancel()

	base := os.Getenv("PAGEWIRE_URL")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	eng, err := docker.NewClient("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	dockerWaitHealthz(ctx, t, base)
	code, body, _ := dockerHTTP(ctx, t, http.MethodPut, base+"/api/v1/config",
		map[string]string{"If-None-Match": "*", "Content-Type": "application/json"},
		`{"apiVersion":"pagewire/v1","name":"pagewire-it-docker","device":{"mode":"simulated"}}`)
	if code != http.StatusCreated {
		t.Fatalf("PUT config: %d %s (a config already exists? run compose down -v first)", code, body)
	}
	st := dockerWaitReady(ctx, t, base, 5*time.Minute)

	t.Run("print and scan", func(t *testing.T) {
		sample, err := os.ReadFile("../../internal/printing/testdata/sample.pdf")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("printer %s, scanner %s", st.Printer.URI, st.Scanner.URL)
		printAndWait(ctx, t, st.Printer.URI, sample)
		for _, c := range []struct {
			name string
			new  func(string) (scanning.Scanner, error)
		}{{"mfp", scanning.NewMFP}, {"xml", scanning.NewXML}} {
			t.Run(c.name, func(t *testing.T) {
				s, err := c.new(st.Scanner.URL)
				if err != nil {
					t.Fatal(err)
				}
				waitFor(ctx, t, 60*time.Second, "AirSane eSCL to accept connections", func() (bool, string) {
					if _, err := s.Capabilities(ctx); err != nil {
						return false, err.Error()
					}
					return true, ""
				})
				scanOnePage(ctx, t, s)
			})
		}
	})

	t.Run("hardening", func(t *testing.T) {
		owned := dockerOwned(ctx, t, eng)
		if names := dockerNames(owned); !slices.Equal(names, dockerSorted(dockerSimContainers)) {
			t.Fatalf("owned containers %q, want %q", names, dockerSorted(dockerSimContainers))
		}
		for _, c := range owned {
			ins, err := eng.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if msg := dockerHardeningErr(ins.Container.HostConfig); msg != "" {
				t.Errorf("%s: %s", c.Labels[docker.LabelContainer], msg)
			}
		}
	})

	t.Run("control-plane restart adopts the workload", func(t *testing.T) {
		before := dockerIDs(dockerOwned(ctx, t, eng))
		if _, err := eng.ContainerRestart(ctx, docker.DefaultContainer, client.ContainerRestartOptions{}); err != nil {
			t.Fatal(err)
		}
		dockerWaitHealthz(ctx, t, base)
		dockerWaitReady(ctx, t, base, 3*time.Minute)
		// A rollout after the first Apply would replace the IDs; give a late one time to show.
		time.Sleep(5 * time.Second)
		if after := dockerIDs(dockerOwned(ctx, t, eng)); !maps.Equal(before, after) {
			t.Fatalf("device containers changed across a control-plane restart:\nbefore %v\nafter  %v", before, after)
		}
	})

	t.Run("self-heal", func(t *testing.T) {
		old := dockerIDs(dockerOwned(ctx, t, eng))["sim-printer"]
		if _, err := eng.ContainerRemove(ctx, docker.Prefix+"sim-printer", client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			t.Fatal(err)
		}
		waitFor(ctx, t, 3*time.Minute, "a new sim-printer and every device container running", func() (bool, string) {
			owned := dockerOwned(ctx, t, eng)
			id := dockerIDs(owned)["sim-printer"]
			return id != "" && id != old && dockerAllRunning(owned), dockerState(owned)
		})
		dockerWaitReady(ctx, t, base, 3*time.Minute)
	})

	t.Run("spec change rolls the workload", func(t *testing.T) {
		oldHash := dockerHashes(dockerOwned(ctx, t, eng))
		code, body, hdr := dockerHTTP(ctx, t, http.MethodGet, base+"/api/v1/config", nil, "")
		if code != http.StatusOK {
			t.Fatalf("GET config: %d %s", code, body)
		}
		code, body, _ = dockerHTTP(ctx, t, http.MethodPatch, base+"/api/v1/config",
			map[string]string{"If-Match": hdr.Get("ETag"), "Content-Type": "application/merge-patch+json"},
			`{"name":"pagewire-it-docker-2"}`)
		if code != http.StatusOK {
			t.Fatalf("PATCH config: %d %s", code, body)
		}
		waitFor(ctx, t, 5*time.Minute, "every device container on a new workload hash and running", func() (bool, string) {
			owned := dockerOwned(ctx, t, eng)
			hashes := dockerHashes(owned)
			ok := len(owned) == len(dockerSimContainers) && !slices.Contains(hashes, "") && dockerAllRunning(owned)
			for _, h := range hashes {
				ok = ok && !slices.Contains(oldHash, h)
			}
			return ok, fmt.Sprintf("hashes %q (old %q); %s", hashes, oldHash, dockerState(owned))
		})
		dockerWaitReady(ctx, t, base, 3*time.Minute)
	})
}

// dockerHardeningErr checks one device container's host config, normalised for
// Podman's compat inspect, and returns what is wrong or "".
func dockerHardeningErr(hc *container.HostConfig) string {
	if hc == nil {
		return "no host config"
	}
	var errs []string
	if !hc.ReadonlyRootfs {
		errs = append(errs, "root file system is writable")
	}
	if !hc.NetworkMode.IsHost() {
		errs = append(errs, fmt.Sprintf("network mode %q, want host", hc.NetworkMode))
	}
	if hc.Privileged {
		errs = append(errs, "privileged")
	}
	if len(hc.CapAdd) > 0 {
		errs = append(errs, fmt.Sprintf("CapAdd %q", hc.CapAdd))
	}
	drop := map[string]bool{}
	for _, c := range hc.CapDrop {
		drop[strings.TrimPrefix(strings.ToUpper(c), "CAP_")] = true
	}
	if !drop["ALL"] {
		for _, c := range dockerCommonCaps {
			if !drop[c] {
				errs = append(errs, fmt.Sprintf("CapDrop %q keeps %s", hc.CapDrop, c))
				break
			}
		}
	}
	if !slices.ContainsFunc(hc.SecurityOpt, func(o string) bool {
		return o == "no-new-privileges" || o == "no-new-privileges:true" || o == "no-new-privileges=true"
	}) {
		errs = append(errs, fmt.Sprintf("SecurityOpt %q lacks no-new-privileges", hc.SecurityOpt))
	}
	return strings.Join(errs, "; ")
}

// dockerOwned lists every container the backend owns, exited ones included
// (the probe runs to completion).
func dockerOwned(ctx context.Context, t *testing.T, eng *client.Client) []container.Summary {
	t.Helper()
	res, err := eng.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", docker.LabelManagedBy+"="+docker.ManagedBy),
	})
	if err != nil {
		t.Fatalf("list device containers: %v", err)
	}
	return res.Items
}

func dockerSorted(s []string) []string { return slices.Sorted(slices.Values(s)) }

// dockerNames returns the workload names (the container label), sorted.
func dockerNames(cs []container.Summary) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Labels[docker.LabelContainer])
	}
	return dockerSorted(out)
}

// dockerIDs maps workload names to container IDs.
func dockerIDs(cs []container.Summary) map[string]string {
	out := map[string]string{}
	for _, c := range cs {
		out[c.Labels[docker.LabelContainer]] = c.ID
	}
	return out
}

func dockerHashes(cs []container.Summary) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Labels[docker.LabelWorkloadHash])
	}
	return out
}

// dockerAllRunning: all simulated-mode containers exist and every one but a
// run-to-completion init is running.
func dockerAllRunning(cs []container.Summary) bool {
	if !slices.Equal(dockerNames(cs), dockerSorted(dockerSimContainers)) {
		return false
	}
	for _, c := range cs {
		if c.Labels[docker.LabelInit] != "true" && c.State != container.StateRunning {
			return false
		}
	}
	return true
}

func dockerState(cs []container.Summary) string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Labels[docker.LabelContainer]+" "+string(c.State))
	}
	slices.Sort(out)
	return strings.Join(out, ", ")
}

// dockerTry sends one request to the control plane and returns the status,
// the body and the response headers.
func dockerTry(ctx context.Context, method, url string, hdr map[string]string, body string) (int, string, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body)) //nolint:gosec // G704: the test's own control plane
	if err != nil {
		return 0, "", nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the test's own control plane
	if err != nil {
		return 0, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), resp.Header, err
}

// dockerHTTP is dockerTry that fails the test on a transport error.
func dockerHTTP(ctx context.Context, t *testing.T, method, url string, hdr map[string]string, body string) (int, string, http.Header) {
	t.Helper()
	code, b, h, err := dockerTry(ctx, method, url, hdr, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return code, b, h
}

func dockerWaitHealthz(ctx context.Context, t *testing.T, base string) {
	t.Helper()
	waitFor(ctx, t, 2*time.Minute, base+"/healthz", func() (bool, string) {
		code, body, _, err := dockerTry(ctx, http.MethodGet, base+"/healthz", nil, "")
		if err != nil {
			return false, err.Error()
		}
		return code == http.StatusOK, fmt.Sprintf("%d %s", code, body)
	})
}

// dockerWaitReady waits for device phase ready with the printer and the
// scanner available, and returns that status.
func dockerWaitReady(ctx context.Context, t *testing.T, base string, d time.Duration) apitypes.Status {
	t.Helper()
	var st apitypes.Status
	waitFor(ctx, t, d, "device ready with printer and scanner available", func() (bool, string) {
		code, body, _, err := dockerTry(ctx, http.MethodGet, base+"/api/v1/status", nil, "")
		if err != nil {
			return false, err.Error()
		}
		if code != http.StatusOK {
			return false, fmt.Sprintf("status %d %s", code, body)
		}
		st = apitypes.Status{}
		if err := json.Unmarshal([]byte(body), &st); err != nil {
			return false, err.Error()
		}
		return st.Device.Phase == "ready" && st.Printer.Available && st.Scanner.Available,
			fmt.Sprintf("device %s %q, printer %v %s, scanner %v %s", st.Device.Phase, st.Device.Message,
				st.Printer.Available, st.Printer.URI, st.Scanner.Available, st.Scanner.URL)
	})
	return st
}
