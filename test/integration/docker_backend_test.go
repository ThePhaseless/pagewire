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
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/backend/docker"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

// dockerSimContainers are the device containers of simulated mode, in plan order.
var dockerSimContainers = []string{"probe", "dbus", "avahi", "agent", "sim-printer-0", "airsane"}

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
		`{"apiVersion":"pagewire/v2","name":"pagewire-it-docker",`+
			`"printers":[{"id":"sim","name":"pagewire-it-docker","mode":"simulated"}],`+
			`"scanners":[{"id":"sim-scan","name":"pagewire-it-docker scanner","mode":"simulated"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("PUT config: %d %s (a config already exists? run compose down -v first)", code, body)
	}
	st := dockerWaitReady(ctx, t, base, 5*time.Minute)

	t.Run("print and scan", func(t *testing.T) {
		sample, err := os.ReadFile("../../internal/printing/testdata/sample.pdf")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("printer %s, scanner %s", st.Printers[0].URI, st.Scanners[0].URL)
		printAndWait(ctx, t, st.Printers[0].URI, sample)
		for _, c := range []struct {
			name string
			new  func(string) (scanning.Scanner, error)
		}{{"mfp", scanning.NewMFP}, {"xml", scanning.NewXML}} {
			t.Run(c.name, func(t *testing.T) {
				s, err := c.new(st.Scanners[0].URL)
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
		old := dockerIDs(dockerOwned(ctx, t, eng))["sim-printer-0"]
		reacted := dockerReaction(ctx, t, eng, docker.Prefix+"sim-printer-0")
		filtered := dockerFiltered(ctx, t, eng, docker.Prefix+"sim-printer-0")
		removed := time.Now()
		if _, err := eng.ContainerRemove(ctx, docker.Prefix+"sim-printer-0", client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			t.Fatal(err)
		}
		// The backend's own stream filters on the server by type, label and
		// event; this engine must pass the removed container's die or destroy
		// through them, or the backend never hears of it.
		select {
		case ev := <-filtered:
			if ev.err != nil {
				t.Fatalf("Engine events with the backend's filters: %v", ev.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no die or destroy of sim-printer-0 within 10 s on a stream with docker.EventFilters: this engine's server-side event filter drops it")
		}
		// The fallback re-check runs every 30 s, so a rollout step this fast
		// comes from an event: the backend's stream, or the agent reporting
		// the vanished printer service (either makes the controller re-apply).
		// The rollout itself may take longer: it stops each container with a
		// 10 s timeout, and AirSane was seen to wait out that timeout in runs
		// that scanned first.
		select {
		case ev := <-reacted:
			if ev.err != nil {
				t.Fatalf("Engine events: %v", ev.err)
			}
			t.Logf("backend reacted %s after the remove: %s %s", time.Since(removed).Round(time.Millisecond), ev.Action, ev.Actor.Attributes["name"])
		case <-time.After(10 * time.Second):
			t.Fatalf("no rollout step within 10 s of removing sim-printer-0; state: %s", dockerState(dockerOwned(ctx, t, eng)))
		}
		waitFor(ctx, t, 3*time.Minute, "a new sim-printer and every device container running", func() (bool, string) {
			owned := dockerOwned(ctx, t, eng)
			id := dockerIDs(owned)["sim-printer-0"]
			return id != "" && id != old && dockerAllRunning(owned), dockerState(owned)
		})
		t.Logf("new sim-printer-0 and every device container running %s after the remove", time.Since(removed).Round(time.Millisecond))
		st := dockerWaitReady(ctx, t, base, 3*time.Minute)
		for _, r := range st.Reconcile {
			if r.Item == "backend-events" && !r.OK {
				t.Errorf("reconcile item backend-events failed: %s", r.Message)
			}
		}
	})

	t.Run("spec change rolls the workload", func(t *testing.T) {
		oldHash := dockerHashes(dockerOwned(ctx, t, eng))
		code, body, hdr := dockerHTTP(ctx, t, http.MethodGet, base+"/api/v1/config", nil, "")
		if code != http.StatusOK {
			t.Fatalf("GET config: %d %s", code, body)
		}
		code, body, _ = dockerHTTP(ctx, t, http.MethodPatch, base+"/api/v1/config",
			map[string]string{"If-Match": hdr.Get("ETag"), "Content-Type": "application/merge-patch+json"},
			`{"printers":[{"id":"sim","name":"pagewire-it-docker-2","mode":"simulated"}]}`) // the simulated printer's name is in the spec
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

// dockerReaction follows the Engine's events for the backend's containers and
// delivers the first rollout step that is not the removal of gone (its own
// kill, die and destroy): another device container stopped, or one created.
// A stream error is delivered instead. The stream replays from now, so an
// event that comes before the request is connected is not missed.
func dockerReaction(ctx context.Context, t *testing.T, eng *client.Client, gone string) <-chan dockerEvent {
	t.Helper()
	ectx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	now := time.Now()
	r := eng.Events(ectx, client.EventsListOptions{Since: fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond()), Filters: make(client.Filters).
		Add("type", "container").Add("label", docker.LabelManagedBy+"="+docker.ManagedBy)})
	out := make(chan dockerEvent, 1)
	go func() {
		for {
			select {
			case <-ectx.Done():
				return
			case err := <-r.Err:
				out <- dockerEvent{err: fmt.Errorf("stream ended: %w", err)}
				return
			case m := <-r.Messages:
				name := m.Actor.Attributes["name"]
				switch m.Action {
				case events.ActionKill, events.ActionStop, events.ActionDie, events.ActionDestroy:
					if name == gone {
						continue
					}
				case events.ActionCreate:
				default:
					continue
				}
				out <- dockerEvent{Message: m}
				return
			}
		}
	}()
	return out
}

// dockerFiltered sends the first die or destroy of container gone on a
// stream with the backend's own filters (docker.EventFilters).
func dockerFiltered(ctx context.Context, t *testing.T, eng *client.Client, gone string) <-chan dockerEvent {
	t.Helper()
	ectx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	now := time.Now()
	r := eng.Events(ectx, client.EventsListOptions{Since: fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond()), Filters: docker.EventFilters()})
	out := make(chan dockerEvent, 1)
	go func() {
		for {
			select {
			case <-ectx.Done():
				return
			case err := <-r.Err:
				out <- dockerEvent{err: fmt.Errorf("stream ended: %w", err)}
				return
			case m := <-r.Messages:
				if (m.Action == events.ActionDie || m.Action == events.ActionDestroy) && m.Actor.Attributes["name"] == gone {
					out <- dockerEvent{Message: m}
					return
				}
			}
		}
	}()
	return out
}

type dockerEvent struct {
	events.Message
	err error
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

// dockerWaitReady waits for phase ready with the one printer and the one
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
		if len(st.Printers) != 1 || len(st.Scanners) != 1 {
			return false, fmt.Sprintf("phase %s %q, %d printers, %d scanners", st.Phase, st.Message, len(st.Printers), len(st.Scanners))
		}
		p, sc := st.Printers[0], st.Scanners[0]
		return st.Phase == "ready" && p.Available && sc.Available,
			fmt.Sprintf("phase %s %q, printer %s %v %s, scanner %s %v %s", st.Phase, st.Message,
				p.Phase, p.Available, p.URI, sc.Phase, sc.Available, sc.URL)
	})
	return st
}
