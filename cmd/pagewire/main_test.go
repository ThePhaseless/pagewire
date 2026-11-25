package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ThePhaseless/pagewire/internal/api"
	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/controller"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/scanning"
	"github.com/ThePhaseless/pagewire/internal/schema"
)

// waitFor bounds every single wait inside the 2 min overall deadline.
const waitFor = 60 * time.Second

type sseEvent struct {
	name string
	data []byte
}

type instance struct {
	t       *testing.T
	ctx     context.Context
	base    string // http://host:port of the public listener
	addr    string
	dataDir string
	cfgPath string
}

// startServe runs serve in-process with the sim backend on random ports and
// stops it when the test ends.
func startServe(t *testing.T, ctx context.Context, dataDir string, extra ...string) *instance {
	t.Helper()
	cfgPath := filepath.Join(dataDir, "pagewire.yaml")
	args := append([]string{
		"--backend=sim", "--listen=127.0.0.1:0", "--internal-listen=127.0.0.1:0",
		"--data-dir=" + dataDir, "--config=" + cfgPath,
	}, extra...)
	sctx, cancel := context.WithCancel(ctx)
	addrs := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- serve(sctx, args, func(p, _ net.Addr) { addrs <- p }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve returned %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop within 20 s")
		}
	})
	select {
	case a := <-addrs:
		return &instance{t: t, ctx: sctx, base: "http://" + a.String(), addr: a.String(), dataDir: dataDir, cfgPath: cfgPath}
	case err := <-done:
		t.Fatalf("serve exited before listening: %v", err)
	case <-ctx.Done():
		t.Fatal("serve did not start before the deadline")
	}
	return nil
}

// do sends one request and returns the status code and the whole body.
func (in *instance) do(method, path string, body []byte, hdr ...string) (int, []byte) {
	in.t.Helper()
	req, err := http.NewRequestWithContext(in.ctx, method, in.base+path, bytes.NewReader(body))
	if err != nil {
		in.t.Fatal(err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		in.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		in.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, b
}

func (in *instance) expect(method, path string, body []byte, want int, out any, hdr ...string) {
	in.t.Helper()
	code, b := in.do(method, path, body, hdr...)
	if code != want {
		in.t.Fatalf("%s %s: status %d, want %d; body %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			in.t.Fatalf("%s %s: decode %s: %v", method, path, b, err)
		}
	}
}

// events opens the SSE stream and keeps draining it, so the hub never drops us.
func (in *instance) events() <-chan sseEvent {
	in.t.Helper()
	req, err := http.NewRequestWithContext(in.ctx, http.MethodGet, in.base+"/api/v1/events", nil)
	if err != nil {
		in.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // closed by the reader goroutine
	if err != nil {
		in.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		in.t.Fatalf("events: status %d", resp.StatusCode)
	}
	ch := make(chan sseEvent, 4096)
	go func() {
		defer func() { _ = resp.Body.Close() }()
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = []byte(strings.TrimPrefix(line, "data: "))
			case line == "" && ev.name != "":
				ch <- ev
				ev = sseEvent{}
			}
		}
	}()
	return ch
}

// waitEvent returns the first event named name whose payload satisfies ok.
func waitEvent[T any](in *instance, ch <-chan sseEvent, name, what string, ok func(T) bool) T {
	in.t.Helper()
	return waitEventWithin(in, ch, name, what, waitFor, ok)
}

func waitEventWithin[T any](in *instance, ch <-chan sseEvent, name, what string, within time.Duration, ok func(T) bool) T {
	in.t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	var last []byte
	for {
		select {
		case ev, open := <-ch:
			if !open {
				in.t.Fatalf("waiting for %s: event stream closed", what)
			}
			if ev.name != name {
				continue
			}
			last = ev.data
			var v T
			if err := json.Unmarshal(ev.data, &v); err != nil {
				in.t.Fatalf("decode %s event %s: %v", name, ev.data, err)
			}
			if ok(v) {
				return v
			}
		case <-timer.C:
			in.t.Fatalf("timed out after %s waiting for %s; last %s event: %s", within, what, name, last)
		case <-in.ctx.Done():
			in.t.Fatalf("deadline reached waiting for %s; last %s event: %s", what, name, last)
		}
	}
}

func waitStatus(in *instance, ch <-chan sseEvent, what string, ok func(apitypes.Status) bool) apitypes.Status {
	in.t.Helper()
	return waitEvent(in, ch, apitypes.EventStatus, what, ok)
}

// pollSetup polls GET /setup until ok holds.
func (in *instance) pollSetup(what string, ok func(apitypes.Setup) bool) apitypes.Setup {
	in.t.Helper()
	deadline := time.Now().Add(waitFor)
	for {
		var s apitypes.Setup
		in.expect(http.MethodGet, "/api/v1/setup", nil, http.StatusOK, &s)
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) || in.ctx.Err() != nil {
			in.t.Fatalf("setup never showed %s; last %+v", what, s)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// printer and scanner return the Status entry with id, or a zero entry.
func printer(s apitypes.Status, id string) apitypes.StatusPrinter {
	for _, p := range s.Printers {
		if p.ID == id {
			return p
		}
	}
	return apitypes.StatusPrinter{}
}

func scanner(s apitypes.Status, id string) apitypes.StatusScanner {
	for _, sc := range s.Scanners {
		if sc.ID == id {
			return sc
		}
	}
	return apitypes.StatusScanner{}
}

// ready reports every printer and scanner in ids ready, and the printers available.
func ready(ids ...string) func(apitypes.Status) bool {
	return func(s apitypes.Status) bool {
		for _, id := range ids {
			p, sc := printer(s, id), scanner(s, id)
			printerReady := p.Phase == apitypes.PhaseReady && p.Available
			if !printerReady && sc.Phase != apitypes.PhaseReady {
				return false
			}
		}
		return true
	}
}

func (in *instance) print(ch <-chan sseEvent, printerID, media string) apitypes.Job {
	in.t.Helper()
	pdf, err := os.ReadFile("../../internal/printing/testdata/sample.pdf")
	if err != nil {
		in.t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	opts, _ := json.Marshal(apitypes.PrintOptions{Copies: 1, PrintSettings: device.PrintSettings{Media: media}})
	ow, _ := mw.CreateFormField("options")
	_, _ = ow.Write(opts)
	fw, _ := mw.CreateFormFile("file", "sample.pdf")
	_, _ = fw.Write(pdf)
	_ = mw.Close()
	var res apitypes.PrintResult
	in.expect(http.MethodPost, "/api/v1/printers/"+printerID+"/print", body.Bytes(), http.StatusAccepted, &res, "Content-Type", mw.FormDataContentType())
	job := res.Job
	if job.Printer != printerID {
		in.t.Fatalf("job printer %q, want %q", job.Printer, printerID)
	}
	waitEventWithin(in, ch, apitypes.EventJob, fmt.Sprintf("job %d completed", job.ID), 30*time.Second, func(j apitypes.Job) bool {
		if j.ID == job.ID && (j.State == "aborted" || j.State == "canceled") {
			in.t.Fatalf("job %d ended %s", j.ID, j.State)
		}
		return j.ID == job.ID && j.Printer == printerID && j.State == "completed"
	})
	out, _ := filepath.Glob(filepath.Join(in.dataDir, "sim-output", "*"))
	if len(out) == 0 {
		in.t.Fatalf("no document in %s/sim-output after job %d completed", in.dataDir, job.ID)
	}
	return job
}

func (in *instance) writeConfig(content string) {
	in.t.Helper()
	if err := os.WriteFile(in.cfgPath, []byte(content), 0o600); err != nil { //nolint:gosec // the test's own temp dir
		in.t.Fatal(err)
	}
}

func validateStatus(t *testing.T, b []byte) {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema.API()))
	if err != nil {
		t.Fatal(err)
	}
	comp := jsonschema.NewCompiler()
	comp.AssertFormat()
	if err := comp.AddResource(schema.APIID, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := comp.Compile(schema.APIID + "#/$defs/Status")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("Status does not validate: %v\n%s", err, b)
	}
}

// ownerV1 is an owner-shaped pagewire/v1 file: the Samsung, monochrome, with comments.
const ownerV1 = `# yaml-language-server: $schema=https://raw.githubusercontent.com/ThePhaseless/pagewire/main/schema/config.schema.json
# The office printer.
apiVersion: pagewire/v1
name: Samsung
location: Office # by the window
device:
  mode: usb
  usb:
    vendorId: "04e8"
    productId: "342e"
  driver: auto
print:
  maxUploadMiB: 50 # big scans
  defaults:
    colorMode: monochrome
`

// officeV2 serves the office fixture: the HP and the Samsung as printer and
// scanner, a simulated printer and the LiDE scanner.
const officeV2 = `apiVersion: pagewire/v2
name: Office
printers:
  - {id: hp, name: HP OfficeJet, usb: {vendorId: "03f0", productId: "2754", serial: FAKE0005}}
  - {id: scx, name: Samsung SCX-4300, usb: {vendorId: "04e8", productId: "342e", serial: FAKE0008}}
  - {id: sim, name: Simulated, mode: simulated}
scanners:
  - {id: hp-scan, name: HP scanner, usb: {vendorId: "03f0", productId: "2754", serial: FAKE0005}}
  - {id: scx-scan, name: Samsung scanner, usb: {vendorId: "04e8", productId: "342e", serial: FAKE0008}}
  - {id: lide, name: CanoScan LiDE, usb: {vendorId: "04a9", productId: "1904", serial: FAKE0011}}
`

func hasItem(s apitypes.Status, item string) bool {
	return slices.ContainsFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == item })
}

// TestEndToEnd runs the whole control plane in-process in sim mode. Every
// request and wait shares one 2 min deadline, so a hang fails the test.
func TestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)

	t.Run("ippusb", func(t *testing.T) {
		t.Parallel()
		in := startServe(t, ctx, t.TempDir())

		// 1. Wizard state before any config.
		_, raw := in.do(http.MethodGet, "/api/v1/status", nil)
		validateStatus(t, raw)
		var first apitypes.Status
		_ = json.Unmarshal(raw, &first)
		if first.Config.State != apitypes.ConfigMissing || !slices.Contains([]string{"none", "probing"}, first.Phase) || len(first.Printers) != 0 {
			t.Fatalf("first Status: config.state %q, phase %q, printers %+v", first.Config.State, first.Phase, first.Printers)
		}
		in.expect(http.MethodGet, "/api/v1/config", nil, http.StatusNotFound, nil)
		setup := in.pollSetup("the ippusb-mfp device and suggestions", func(s apitypes.Setup) bool {
			return len(s.Devices) == 1 && s.Devices[0].SuggestedPrinter != nil && s.Devices[0].SuggestedScanner != nil
		})
		if !setup.Required || !setup.USBResourceConfigured || setup.Backend != "sim" {
			t.Fatalf("setup: required %v, usbResourceConfigured %v, backend %q", setup.Required, setup.USBResourceConfigured, setup.Backend)
		}
		d := setup.Devices[0]
		if d.Device.VendorID != "03f0" || d.Device.ProductID != "2754" || !d.CanPrint || !d.CanScan {
			t.Fatalf("setup device is not the ippusb-mfp fixture: %+v", d)
		}
		if d.PrinterDecision == nil || d.PrinterDecision.Rule != "ipp-over-usb" || d.PrinterDecision.Family != "ipp-usb" {
			t.Fatalf("setup printer decision: %+v", d.PrinterDecision)
		}

		ch := in.events()

		// 2. PUT the suggested config with the suggested entries.
		cfg := setup.Suggested
		cfg.Printers, cfg.Scanners = []config.Printer{*d.SuggestedPrinter}, []config.Scanner{*d.SuggestedScanner}
		pid, sid := d.SuggestedPrinter.ID, d.SuggestedScanner.ID
		body, _ := json.Marshal(cfg)
		var created apitypes.ConfigResponse
		in.expect(http.MethodPut, "/api/v1/config", body, http.StatusCreated, &created, "If-None-Match", "*", "Content-Type", "application/json")
		if created.State != apitypes.ConfigApplied || created.Config == nil {
			t.Fatalf("PUT: %+v", created)
		}

		// 3. Ready.
		st := waitStatus(in, ch, "printer and scanner ready", ready(pid, sid))
		if err := healthcheck(ctx, []string{"--listen", in.addr}); err != nil {
			t.Fatalf("healthcheck: %v", err)
		}

		// 4. Print, confirmed by a job event.
		media := "iso_a4_210x297mm"
		if m := printer(st, pid).Media; len(m) > 0 {
			media = m[0]
		}
		in.print(ch, pid, media)

		// 5. Scan two ADF pages into a PDF.
		set, _ := json.Marshal(apitypes.ScanSettings{Scanner: sid, Source: "adf", Resolution: 300, ColorMode: "color", Format: "pdf"})
		var scan apitypes.Scan
		in.expect(http.MethodPost, "/api/v1/scans", set, http.StatusAccepted, &scan, "Content-Type", "application/json")
		done := waitEvent(in, ch, apitypes.EventScan, "scan completed", func(s apitypes.Scan) bool {
			if s.ID == scan.ID && s.State == "failed" {
				t.Fatalf("scan failed: %v", *s.Error)
			}
			return s.ID == scan.ID && s.State == "completed"
		})
		if done.Pages != 2 || done.Scanner != sid {
			t.Fatalf("scan pages %d on %q, want 2 on %q", done.Pages, done.Scanner, sid)
		}
		code, pdf := in.do(http.MethodGet, "/api/v1/scans/"+scan.ID+"/download", nil)
		if code != http.StatusOK || !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			t.Fatalf("download: status %d, starts %q", code, pdf[:min(len(pdf), 8)])
		}

		// 6. PATCH with a stale and then the current ETag; comments survive.
		file, err := os.ReadFile(in.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		in.writeConfig("# hand-written comment\n" + string(file))
		var cur apitypes.ConfigResponse
		in.expect(http.MethodGet, "/api/v1/config", nil, http.StatusOK, &cur)
		patch := []byte(`{"name":"Renamed home"}`)
		var conflict apitypes.ConfigResponse
		in.expect(http.MethodPatch, "/api/v1/config", patch, http.StatusPreconditionFailed, &conflict,
			"If-Match", `"`+created.Etag+`"`, "Content-Type", "application/merge-patch+json")
		if conflict.State != apitypes.ConfigApplied || conflict.Config == nil || conflict.Etag != cur.Etag {
			t.Fatalf("stale PATCH 412 body: %+v (current etag %s)", conflict, cur.Etag)
		}
		var patched apitypes.ConfigResponse
		in.expect(http.MethodPatch, "/api/v1/config", patch, http.StatusOK, &patched,
			"If-Match", `"`+cur.Etag+`"`, "Content-Type", "application/merge-patch+json")
		if patched.Config == nil || patched.Config.Name != "Renamed home" {
			t.Fatalf("PATCH 200 body: %+v", patched)
		}
		good, err := os.ReadFile(in.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(good, []byte("# hand-written comment")) || !bytes.Contains(good, []byte("# yaml-language-server:")) {
			t.Fatalf("comments lost after PATCH:\n%s", good)
		}

		// 7. A broken hand edit: invalid with a line, printer still ready.
		in.writeConfig("apiVersion: pagewire/v2\nname: [unclosed\n")
		waitStatus(in, ch, "config invalid with a line", func(s apitypes.Status) bool {
			return s.Config.State == apitypes.ConfigInvalid && len(s.Config.Errors) > 0 && s.Config.Errors[0].Line > 0 && ready(pid)(s)
		})
		var invalid412 apitypes.ConfigResponse
		in.expect(http.MethodPatch, "/api/v1/config", patch, http.StatusPreconditionFailed, &invalid412,
			"If-Match", `"`+patched.Etag+`"`, "Content-Type", "application/merge-patch+json")
		if invalid412.Config != nil || invalid412.State != apitypes.ConfigInvalid || len(invalid412.Errors) == 0 {
			t.Fatalf("PATCH 412 against an invalid file: %+v", invalid412)
		}

		// 8. Fixed again.
		in.writeConfig(string(good))
		waitStatus(in, ch, "config applied again", func(s apitypes.Status) bool {
			return s.Config.State == apitypes.ConfigApplied && ready(pid, sid)(s)
		})

		// 9. An unknown EULA: valid YAML, invalid config, printer still ready.
		in.writeConfig("apiVersion: pagewire/v2\nname: Printer\nlicenses:\n  acceptedEulas:\n    - id: no-such-eula\n      version: \"1\"\n      textSha256: " + strings.Repeat("a", 64) + "\n")
		waitStatus(in, ch, "unknown EULA rejected", func(s apitypes.Status) bool {
			return s.Config.State == apitypes.ConfigInvalid && ready(pid)(s) && slices.ContainsFunc(s.Config.Errors, func(e config.FieldError) bool {
				return e.Path == "/licenses/acceptedEulas/0"
			})
		})
		if hasItem(st, migrationItem) {
			t.Fatalf("a v2 install has a %s item: %+v", migrationItem, st.Reconcile)
		}
	})

	t.Run("pappl", func(t *testing.T) {
		t.Parallel()
		// The wizard flow: the licence text is staged before any config exists,
		// and acceptance records its sha256.
		in := startServe(t, ctx, t.TempDir(), "--sim-device=hp-legacy-mfp")
		setup := in.pollSetup("the HP licence text", func(s apitypes.Setup) bool {
			return len(s.EULAs) == 1 && s.EULAs[0].State == apitypes.EULAAvailable
		})
		var eula apitypes.EULA
		in.expect(http.MethodGet, "/api/v1/eulas/hplip-plugin", nil, http.StatusOK, &eula)
		if eula.Text == nil || eula.TextSHA256 == nil || *eula.TextSHA256 != *setup.EULAs[0].TextSHA256 {
			t.Fatalf("eula %+v", eula)
		}
		ch := in.events()
		in.writeConfig("apiVersion: pagewire/v2\nname: HP test\nprinters:\n  - id: hp\n    name: HP test\nlicenses:\n  acceptedEulas:\n" +
			"    - id: hplip-plugin\n      version: \"3.24.4\"\n      textSha256: " + *eula.TextSHA256 + "\n")
		st := waitStatus(in, ch, "PAPPL printer ready with the blob installed", func(s apitypes.Status) bool {
			return ready("hp")(s) && strings.HasPrefix(printer(s, "hp").URI, "ipp://127.0.0.1:") &&
				slices.ContainsFunc(s.Workloads, func(w apitypes.StatusWorkload) bool {
					return slices.ContainsFunc(w.Blobs, func(b device.BlobStatus) bool { return b.ID == "hplip-plugin" && b.State == "installed" })
				})
		})
		if dec := printer(st, "hp").Decision; dec == nil || dec.Family != "hplip" {
			t.Fatalf("decision: %+v", dec)
		}
		in.print(ch, "hp", "iso_a4_210x297mm")
	})

	t.Run("no-usb", func(t *testing.T) {
		t.Parallel()
		in := startServe(t, ctx, t.TempDir(), "--sim-device=none")
		setup := in.pollSetup("the no-USB setup", func(apitypes.Setup) bool { return true })
		if setup.USBResourceConfigured || len(setup.Devices) != 0 {
			t.Fatalf("setup: usbResourceConfigured %v, devices %+v", setup.USBResourceConfigured, setup.Devices)
		}
		ch := in.events()
		cfg := setup.Suggested
		cfg.Printers = []config.Printer{{ID: "sim", Name: "Simulated", Enabled: true, Mode: config.ModeSimulated, Driver: config.Auto}}
		cfg.Scanners = []config.Scanner{{ID: "sim-scan", Name: "Simulated scanner", Enabled: true, Mode: config.ModeSimulated, Backend: config.BackendAuto}}
		body, _ := json.Marshal(cfg)
		in.expect(http.MethodPut, "/api/v1/config", body, http.StatusCreated, nil, "If-None-Match", "*", "Content-Type", "application/json")
		waitStatus(in, ch, "simulated devices ready", ready("sim", "sim-scan"))
		_, raw := in.do(http.MethodGet, "/api/v1/status", nil)
		validateStatus(t, raw)
		var doc struct {
			Printers []struct {
				Decision map[string]json.RawMessage `json:"decision"`
				Device   json.RawMessage            `json:"device"`
			} `json:"printers"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Printers) != 1 || string(doc.Printers[0].Decision["device"]) != "null" || string(doc.Printers[0].Device) != "null" {
			t.Fatalf("want \"device\":null in the simulated printer and its decision, got %s", raw)
		}
	})

	t.Run("office", func(t *testing.T) {
		t.Parallel()
		in := startServe(t, ctx, t.TempDir(), "--sim-device=office")
		ch := in.events()
		in.writeConfig(officeV2)
		// The Samsung's legacy PAPPL queue goes through the sim's system URL mapping.
		st := waitStatus(in, ch, "three printers and three scanners ready", ready("hp", "scx", "sim", "hp-scan", "scx-scan", "lide"))
		if len(st.Printers) != 3 || len(st.Scanners) != 3 {
			t.Fatalf("printers %d, scanners %d", len(st.Printers), len(st.Scanners))
		}
		if uris := []string{printer(st, "hp").URI, printer(st, "scx").URI, printer(st, "sim").URI}; uris[0] == uris[1] || uris[1] == uris[2] || uris[0] == uris[2] {
			t.Fatalf("printers share an endpoint: %v", uris)
		}
		// A photo scan on the Samsung, which reports no intents, is refused before it starts.
		set, _ := json.Marshal(apitypes.ScanSettings{Scanner: "scx-scan", Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf", Intent: "photo"})
		in.expect(http.MethodPost, "/api/v1/scans", set, http.StatusUnprocessableEntity, nil, "Content-Type", "application/json")
	})

	t.Run("v1-migrated", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "pagewire.yaml"), []byte(ownerV1), 0o600); err != nil {
			t.Fatal(err)
		}
		in := startServe(t, ctx, dir, "--sim-device=samsung-scx4300-mfp")
		deadline := time.Now().Add(waitFor)
		var file []byte
		for {
			file, _ = os.ReadFile(in.cfgPath)
			if !config.IsV1(file) || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		for _, want := range []string{"apiVersion: pagewire/v2", "# The office printer.", "# big scans", "maxUploadMiB: 50"} {
			if !bytes.Contains(file, []byte(want)) {
				t.Fatalf("rewritten file lacks %q:\n%s", want, file)
			}
		}
		ch := in.events()
		st := waitStatus(in, ch, "the migrated printer and scanner ready", ready("printer", "scanner"))
		if p := printer(st, "printer"); p.Location != "Office" {
			t.Fatalf("printer %+v", p)
		}
		if hasItem(st, migrationItem) {
			t.Fatalf("a successful rewrite left a %s item: %+v", migrationItem, st.Reconcile)
		}
	})

	t.Run("v1-read-only", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		cfgDir := t.TempDir()
		cfgPath := filepath.Join(cfgDir, "pagewire.yaml")
		if err := os.WriteFile(cfgPath, []byte(ownerV1), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cfgDir, 0o555); err != nil { //nolint:gosec // read-only on purpose
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o700) }) //nolint:gosec // let TempDir clean up
		in := startServe(t, ctx, t.TempDir(), "--sim-device=samsung-scx4300-mfp", "--config="+cfgPath)
		in.cfgPath = cfgPath
		ch := in.events()
		st := waitStatus(in, ch, "the migrated entries and a failed rewrite", func(s apitypes.Status) bool {
			return len(s.Printers) == 1 && s.Printers[0].ID == "printer" && len(s.Scanners) == 1 && s.Scanners[0].ID == "scanner" &&
				slices.ContainsFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool { return r.Item == migrationItem && !r.OK })
		})
		if file, _ := os.ReadFile(cfgPath); string(file) != ownerV1 { //nolint:gosec // the test's own temp dir
			t.Fatalf("the read-only file changed:\n%s", file)
		}
		if s := st.Config.State; s != apitypes.ConfigApplied {
			t.Fatalf("config state %q, want applied (the migrated config runs)", s)
		}

		// Writable again: the next content change is rewritten and the item turns ok.
		if err := os.Chmod(cfgDir, 0o700); err != nil { //nolint:gosec // the test's own temp dir
			t.Fatal(err)
		}
		in.writeConfig(strings.Replace(ownerV1, "# The office printer.", "# The hall printer.", 1))
		waitStatus(in, ch, "the config-migration item ok", func(s apitypes.Status) bool {
			return slices.ContainsFunc(s.Reconcile, func(r apitypes.ReconcileItem) bool {
				return r.Item == migrationItem && r.OK && r.Message == "config file rewritten as pagewire/v2"
			})
		})
		if file, err := os.ReadFile(cfgPath); err != nil || config.IsV1(file) || !bytes.Contains(file, []byte("# The hall printer.")) { //nolint:gosec // the test's own temp dir
			t.Fatalf("not rewritten as v2 with its comments (%v):\n%s", err, file)
		}
	})

	t.Run("unwritable-data-dir", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root can write anywhere")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // read-only on purpose
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // let TempDir clean up
		in := startServe(t, ctx, dir)
		in.expect(http.MethodGet, "/healthz", nil, http.StatusOK, nil)
		var st apitypes.Status
		in.expect(http.MethodGet, "/api/v1/status", nil, http.StatusOK, &st)
		if !slices.ContainsFunc(st.Reconcile, func(r apitypes.ReconcileItem) bool {
			return r.Item == "data-dir" && !r.OK && strings.Contains(r.Message, dir)
		}) {
			t.Fatalf("no data-dir reconcile item: %+v", st.Reconcile)
		}
	})
}

func TestServeBadOverrides(t *testing.T) {
	t.Setenv("PAGEWIRE_IMAGE_OVERRIDES", "device=x,bogus=y")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := serve(ctx, []string{"--backend=sim", "--listen=127.0.0.1:0", "--internal-listen=127.0.0.1:0", "--data-dir=" + t.TempDir()}, nil)
	if err == nil || !strings.Contains(err.Error(), "PAGEWIRE_IMAGE_OVERRIDES") {
		t.Fatalf("serve with malformed overrides: %v", err)
	}
}

func TestServeListenError(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- serve(t.Context(), []string{"--backend=sim", "--listen=" + ln.Addr().String(), "--internal-listen=127.0.0.1:0", "--data-dir=" + t.TempDir()}, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("serve on a busy port returned nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve on a busy port did not return within 15 s")
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(good, []byte("apiVersion: pagewire/v1\nname: Printer\n"), 0o600) // v1 is migrated in memory
	good2 := filepath.Join(dir, "good2.yaml")
	_ = os.WriteFile(good2, []byte("apiVersion: pagewire/v2\nname: Home\nprinters:\n  - id: p1\n    name: Printer\n"), 0o600)
	_ = os.WriteFile(bad, []byte("apiVersion: pagewire/v1\nname: Printer\ndevice:\n  mode: bogus\n"), 0o600)
	var out, errOut bytes.Buffer
	if code := validate([]string{good, good2}, &out, &errOut); code != 0 {
		t.Fatalf("good file: exit %d, %s", code, errOut.String())
	}
	out.Reset()
	if code := validate([]string{good, bad}, &out, &errOut); code != 1 {
		t.Fatalf("bad file: exit %d", code)
	}
	if want := bad + ":4:9: /device/mode:"; !strings.HasPrefix(errOut.String(), want) {
		t.Fatalf("bad file output %q, want prefix %q", errOut.String(), want)
	}
}

func TestRunDispatch(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"version"}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: exit %d, %q", code, out.String())
	}
	if code := run(t.Context(), []string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown command: exit %d", code)
	}
	if code := run(t.Context(), []string{"agent", "fetch", "--blob", "x@1"}, &out, &errOut); code != 1 {
		t.Fatalf("agent fetch without --dir: exit %d", code)
	}
	dir := t.TempDir()
	err := agentCmd(t.Context(), []string{"agent", "fetch", "--dir", dir, "--status-out", filepath.Join(dir, "s.json"), "--accept", "hplip-plugin@3.24.4"}[1:])
	if err == nil || !strings.Contains(err.Error(), "<id>@<version>@<sha256") {
		t.Fatalf("agent fetch with a two-part --accept: %v", err)
	}
	if err := agentCmd(t.Context(), []string{"run", "--blob-dir", dir}); err == nil || !strings.Contains(err.Error(), "--control-plane-url") {
		t.Fatalf("agent run --blob-dir: %v, want the missing --control-plane-url error", err)
	}
}

func TestAllowedHostsFor(t *testing.T) {
	simDefault := []string{"localhost", "127.0.0.1", "[::1]"}
	for _, tc := range []struct {
		backend, value, hostIP string
		want                   []string
	}{
		{controller.BackendSim, "", "", simDefault},
		{controller.BackendSim, " , ", "", simDefault},
		{controller.BackendSim, "*", "", nil},
		{controller.BackendK8s, "", "", nil},
		{controller.BackendK8s, "*", "", nil},
		{controller.BackendK8s, "print.example.org, 192.0.2.10 ,", "", []string{"print.example.org", "192.0.2.10"}},
		{controller.BackendSim, "printer.lan", "", []string{"printer.lan"}},
		{controller.BackendDocker, "", "192.0.2.7", append(slices.Clone(simDefault), "192.0.2.7")},
		{controller.BackendDocker, "", "127.0.0.1", simDefault},
		{controller.BackendDocker, "", "fd00::5", append(slices.Clone(simDefault), "fd00::5")},
		{controller.BackendDocker, "printer.lan", "192.0.2.7", []string{"printer.lan"}},
		{controller.BackendDocker, "*", "192.0.2.7", nil},
	} {
		if got := allowedHostsFor(tc.backend, tc.value, tc.hostIP); !slices.Equal(got, tc.want) {
			t.Errorf("allowedHostsFor(%s, %q, %q) = %q, want %q", tc.backend, tc.value, tc.hostIP, got, tc.want)
		}
	}
}

func TestDefaultBackend(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "docker.sock")
	marker := filepath.Join(dir, ".dockerenv")
	missing := filepath.Join(dir, "missing")
	_ = os.WriteFile(sock, nil, 0o600)
	_ = os.WriteFile(marker, nil, 0o600)
	for _, tc := range []struct {
		name, k8sHost, dockerHost, socket, marker, want string
	}{
		{name: "kubernetes", k8sHost: "10.0.0.1", dockerHost: "unix:///x.sock", socket: sock, marker: marker, want: controller.BackendK8s},
		{name: "DOCKER_HOST outside a container", dockerHost: "tcp://127.0.0.1:2375", socket: missing, marker: missing, want: controller.BackendSim},
		{name: "DOCKER_HOST in a container", dockerHost: "tcp://127.0.0.1:2375", socket: missing, marker: marker, want: controller.BackendDocker},
		{name: "container with socket", socket: sock, marker: marker, want: controller.BackendDocker},
		{name: "socket without container", socket: sock, marker: missing, want: controller.BackendSim},
		{name: "container without socket", socket: missing, marker: marker, want: controller.BackendSim},
		{name: "nothing", socket: missing, marker: missing, want: controller.BackendSim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KUBERNETES_SERVICE_HOST", tc.k8sHost)
			t.Setenv("DOCKER_HOST", tc.dockerHost)
			t.Setenv("PAGEWIRE_BACKEND", "")
			oldSock, oldMarkers := dockerSocket, containerMarkers
			t.Cleanup(func() { dockerSocket, containerMarkers = oldSock, oldMarkers })
			dockerSocket, containerMarkers = tc.socket, []string{missing, tc.marker}
			if got := defaultBackend(); got != tc.want {
				t.Fatalf("defaultBackend() = %q, want %q", got, tc.want)
			}
			f, err := parseServeFlags(nil)
			if err != nil || f.backend != tc.want {
				t.Fatalf("parseServeFlags: backend %q err %v, want %q", f.backend, err, tc.want)
			}
		})
	}
}

func TestBackendFlagValues(t *testing.T) {
	f, err := parseServeFlags([]string{"--backend=docker"})
	if err != nil || f.backend != controller.BackendDocker || f.dockerContainer != "pagewire" {
		t.Fatalf("--backend=docker: backend %q container %q err %v", f.backend, f.dockerContainer, err)
	}
	if _, err := parseServeFlags([]string{"--backend=bogus"}); err == nil || !strings.Contains(err.Error(), "k8s, docker or sim") {
		t.Fatalf("--backend=bogus: %v", err)
	}
}

func TestDockerAgentURL(t *testing.T) {
	for _, tc := range []struct{ explicit, listen, want string }{
		{"", "127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"", ":8081", "http://127.0.0.1:8081"},
		{"", "0.0.0.0:8081", "http://127.0.0.1:8081"},
		{"", "[::]:8081", "http://127.0.0.1:8081"},
		{"", "192.168.1.5:8081", "http://192.168.1.5:8081"},
		{"", "[fd00::5]:8081", "http://[fd00::5]:8081"},
		{"http://cp.lan:9000", "bogus", "http://cp.lan:9000"},
	} {
		if got, err := dockerAgentURL(tc.explicit, tc.listen); err != nil || got != tc.want {
			t.Errorf("dockerAgentURL(%q, %q) = %q, %v; want %q", tc.explicit, tc.listen, got, err, tc.want)
		}
	}
	for _, bad := range []string{"bogus", "127.0.0.1", "host:"} {
		if got, err := dockerAgentURL("", bad); err == nil {
			t.Errorf("dockerAgentURL(%q) = %q, want an error", bad, got)
		}
	}
}

// The Engine client connects lazily: serve starts on an unreachable socket,
// and the failure shows only in the status.
func TestServeDockerUnreachable(t *testing.T) {
	t.Setenv("PAGEWIRE_HOST_IP", "")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	got := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, []string{
			"--backend=docker", "--docker-host=unix:///nonexistent.sock",
			"--listen=127.0.0.1:0", "--internal-listen=127.0.0.1:0", "--host-ip=192.0.2.7",
			"--data-dir=" + dir, "--config=" + filepath.Join(dir, "pagewire.yaml"),
		},
			func(p, i net.Addr) {
				if i == nil {
					t.Error("docker serve opened no internal listener")
				}
				got <- p
			})
	}()
	var pub net.Addr
	select {
	case pub = <-got:
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
	case <-ctx.Done():
		t.Fatal("serve did not start")
	}
	get := func(path, host string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+pub.String()+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/healthz", pub.String()); code != http.StatusOK {
		t.Errorf("/healthz: %d", code)
	}
	if code := get("/api/v1/status", "192.0.2.7:8080"); code != http.StatusOK {
		t.Errorf("status via the host IP: %d, want 200", code)
	}
	if code := get("/api/v1/status", "evil.example"); code != http.StatusMisdirectedRequest {
		t.Errorf("status via evil.example: %d, want 421", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("serve did not stop within 20 s")
	}
}

func TestServeFlagsProxySecret(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "secret")
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(file, []byte("from-file\n"), 0o600)
	_ = os.WriteFile(empty, []byte("\n"), 0o600)
	for _, tc := range []struct {
		name, env, file, want, err string
	}{
		{name: "none"},
		{name: "env", env: "from-env", want: "from-env"},
		{name: "file", file: file, want: "from-file"},
		{name: "both", env: "x", file: file, err: "not both"},
		{name: "empty file", file: empty, err: "empty"},
		{name: "missing file", file: filepath.Join(dir, "nope"), err: "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PAGEWIRE_PROXY_SECRET", tc.env)
			args := []string{"--backend=sim"}
			if tc.file != "" {
				args = append(args, "--proxy-secret-file="+tc.file)
			}
			f, err := parseServeFlags(args)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || f.proxySecret != tc.want || f.proxySecretHeader != "X-Pagewire-Proxy-Secret" {
				t.Fatalf("secret %q header %q err %v, want %q", f.proxySecret, f.proxySecretHeader, err, tc.want)
			}
		})
	}
}

func TestServeUsage(t *testing.T) {
	var b bytes.Buffer
	serveFlagSet(&serveFlags{}, &b).Usage()
	usage := b.String()
	for _, want := range []string{"--allowed-hosts", "--docker-host", "--docker-container", "--host-ip", "--proxy-secret-header", "--proxy-secret-file", "PAGEWIRE_PROXY_SECRET", "PAGEWIRE_IMAGE_OVERRIDES"} {
		if !strings.Contains(usage, want) {
			t.Errorf("serve usage lacks %s:\n%s", want, usage)
		}
	}
	if strings.Contains(usage, `(default "")`) || strings.Contains(usage, "ui-dir") || strings.Contains(usage, "sim-scan-delay") {
		t.Errorf("serve usage shows an empty default or a hidden flag:\n%s", usage)
	}
}

func TestSubcommandHelp(t *testing.T) {
	for _, args := range [][]string{{"validate", "--help"}, {"validate", "-h"}, {"agent", "--help"}, {"agent", "-h"}, {"agent", "help"}} {
		var out, errOut bytes.Buffer
		code := run(t.Context(), args, &out, &errOut)
		all := out.String() + errOut.String()
		if code != 2 || !strings.Contains(all, "usage: pagewire "+args[0]) || strings.Contains(all, "no such file") || strings.Contains(all, "unknown agent command") {
			t.Errorf("%v: exit %d, output %q", args, code, all)
		}
	}
}

func TestServeSimHasNoInternalListener(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().String()
	_ = ln.Close()

	t.Setenv("PAGEWIRE_PROXY_SECRET", "s3cret")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type addrs struct{ public, internal net.Addr }
	got := make(chan addrs, 1)
	done := make(chan error, 1)
	go func() {
		dir := t.TempDir()
		done <- serve(ctx, []string{"--backend=sim", "--listen=127.0.0.1:0", "--internal-listen=" + port, "--data-dir=" + dir, "--config=" + filepath.Join(dir, "pagewire.yaml")},
			func(p, i net.Addr) { got <- addrs{p, i} })
	}()
	var a addrs
	select {
	case a = <-got:
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
	case <-ctx.Done():
		t.Fatal("serve did not start")
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	if a.internal != nil {
		t.Fatalf("sim serve opened an internal listener on %v", a.internal)
	}
	// The port is still free: nothing listens there.
	if l, err := lc.Listen(ctx, "tcp", port); err != nil {
		t.Fatalf("internal port %s is taken: %v", port, err)
	} else {
		_ = l.Close()
	}

	get := func(host string, hdr ...string) int {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+a.public.String()+"/api/v1/status", nil)
		if host != "" {
			req.Host = host
		}
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Errorf("without the proxy secret: %d, want 401", code)
	}
	if code := get("", "X-Pagewire-Proxy-Secret", "s3cret"); code != http.StatusOK {
		t.Errorf("with the proxy secret: %d, want 200", code)
	}
	if code := get("evil.example", "X-Pagewire-Proxy-Secret", "s3cret"); code != http.StatusMisdirectedRequest {
		t.Errorf("Host evil.example: %d, want 421", code)
	}
}

func TestCgroupMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		content string // "" means no file
		want    int64
	}{
		"limit":   {"268435456\n", 268435456},
		"max":     {"max\n", 0},
		"missing": {"", 0},
		"garbage": {"lots\n", 0},
	} {
		path := filepath.Join(dir, name)
		if tc.content != "" {
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if got := cgroupMemoryLimit(path); got != tc.want {
			t.Errorf("%s: cgroupMemoryLimit = %d, want %d", name, got, tc.want)
		}
	}
}

func TestSetMemoryLimit(t *testing.T) {
	before := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(before) })
	path := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(path, []byte("268435456\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GOMEMLIMIT", "")
	setMemoryLimit(path)
	if got, want := debug.SetMemoryLimit(-1), int64(268435456/5*4); got != want {
		t.Errorf("limit = %d, want %d (80%% of the cgroup limit)", got, want)
	}

	debug.SetMemoryLimit(before)
	t.Setenv("GOMEMLIMIT", "100MiB")
	setMemoryLimit(path)
	if got := debug.SetMemoryLimit(-1); got != before {
		t.Errorf("limit = %d with GOMEMLIMIT set, want it untouched (%d)", got, before)
	}
}

func TestStatusLookups(t *testing.T) {
	caps := device.PrinterCaps{Formats: []string{"application/pdf"}, ColorModes: []string{"monochrome"}}
	st := apitypes.Status{
		Printers: []apitypes.StatusPrinter{
			{ID: "on", Enabled: true, Available: true, URI: "ipp://a/ipp/print", Capabilities: caps},
			{ID: "off", Enabled: false, Available: true, URI: "ipp://b/ipp/print", Capabilities: caps},
			{ID: "down", Enabled: true, Available: false, Capabilities: caps},
			{ID: "nocaps", Enabled: true, Available: true, URI: "ipp://c/ipp/print"},
		},
		Scanners: []apitypes.StatusScanner{
			{ID: "on", Enabled: true, URL: "http://a/eSCL"},
			{ID: "off", Enabled: false, URL: "http://b/eSCL"},
			{ID: "down", Enabled: true},
		},
	}
	for id, want := range map[string]error{"on": nil, "off": api.ErrUnknownPrinter, "nope": api.ErrUnknownPrinter, "down": api.ErrNoPrinter} {
		if uri, err := printerURI(st, id); !errors.Is(err, want) || (err == nil) != (uri != "") {
			t.Errorf("printerURI(%s) = %q, %v; want %v", id, uri, err, want)
		}
	}
	for id, want := range map[string]error{"on": nil, "off": api.ErrUnknownScanner, "nope": api.ErrUnknownScanner, "down": scanning.ErrNoScanner} {
		if url, err := scannerURL(st, id); !errors.Is(err, want) || (err == nil) != (url != "") {
			t.Errorf("scannerURL(%s) = %q, %v; want %v", id, url, err, want)
		}
	}
	for id, want := range map[string]bool{"on": true, "off": false, "down": false, "nocaps": false, "nope": false} {
		if got, ok := printerCaps(st, id); ok != want || (ok && !slices.Equal(got.ColorModes, caps.ColorModes)) {
			t.Errorf("printerCaps(%s) = %+v, %v; want %v", id, got, ok, want)
		}
	}
}
