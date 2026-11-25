//go:build integration

// Package integration runs the device pod's real daemons from compose.test.yaml
// (dbus, avahi, ippeveprinter, AirSane with the SANE test backend,
// ps-printer-app and the agent, all on the host network) and plays the
// control plane for the agent. See README.md.
package integration

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/ippcaps"
	"github.com/ThePhaseless/pagewire/internal/pappl"
	"github.com/ThePhaseless/pagewire/internal/printing"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

// Fixed host ports from compose.test.yaml.
const (
	controlPlaneAddr = "127.0.0.1:18081"
	papplSystemURI   = "ipp://127.0.0.1:18001/ipp/system"
	simPrinterURI    = "ipp://127.0.0.1:18631/ipp/print"
	queueName        = "it"
)

// controlPlane answers agent reports with one fixed DesiredState and keeps
// the latest report.
type controlPlane struct {
	token   string
	desired device.DesiredState

	mu     sync.Mutex
	latest *device.AgentReport
	errs   []string
}

func (cp *controlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != device.AgentReportPath {
		http.NotFound(w, r)
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), device.TokenScheme+" ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(cp.token)) != 1 {
		cp.fail("agent sent a wrong token")
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	var rep device.AgentReport
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rep); err != nil {
		cp.fail("malformed report: " + err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cp.mu.Lock()
	cp.latest = &rep
	cp.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	cp.mu.Lock()
	desired := cp.desired
	cp.mu.Unlock()
	_ = json.NewEncoder(w).Encode(desired)
}

// setDefaults changes the queue defaults the agent is told to keep.
func (cp *controlPlane) setDefaults(d device.PrintSettings) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	q := slices.Clone(cp.desired.Queues)
	q[0].Defaults = d
	cp.desired.Queues = q
}

func (cp *controlPlane) fail(msg string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	cp.errs = append(cp.errs, msg)
}

func (cp *controlPlane) report() (*device.AgentReport, []string) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.latest, slices.Clone(cp.errs)
}

func TestDevicePod(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	raw, err := os.ReadFile("token")
	if err != nil {
		t.Fatalf("read the agent token (written before compose up): %v", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		t.Fatal("test/integration/token is empty")
	}

	driver := genericPostScriptDriver(ctx, t)
	t.Logf("PAPPL driver: %s", driver)

	want := device.QueueSpec{
		ID:        queueName,
		Name:      queueName,
		Location:  "Integration test",
		Driver:    driver,
		DeviceURI: "cups:" + simPrinterURI,
		Schemes:   []string{"ipp"},
		// The agent's own printer app, named by the controller in production.
		SystemURI: papplSystemURI,
		Device:    device.USBDevice{DeviceID: "MFG:Generic;MDL:PostScript Printer;CMD:POSTSCRIPT;"},
		Defaults:  device.PrintSettings{Media: "iso_a4_210x297mm", Sides: "one-sided"},
	}
	cp := &controlPlane{token: token, desired: device.DesiredState{
		Hash:   "it",
		Queues: []device.QueueSpec{want},
		Browse: device.BrowseTypes,
	}}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", controlPlaneAddr)
	if err != nil {
		t.Fatalf("listen as the control plane: %v", err)
	}
	srv := &http.Server{Handler: cp, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	// The agent backs off up to 30 s while the control plane is down, then
	// reconciles and browses once per 5 s cycle.
	rep := waitReport(ctx, t, cp, want, 120*time.Second)
	if rep.PodUID != "it" {
		t.Errorf("podUid = %q, want it", rep.PodUID)
	}
	if q := rep.Queues[0]; q.ID != queueName || q.DeviceURI != "cups:"+simPrinterURI {
		t.Errorf("queue %+v, want id %s and device URI cups:%s", q, queueName, simPrinterURI)
	}
	queueURI := rep.Queues[0].PrinterURI
	t.Logf("PAPPL queue: %s", queueURI)

	sample, err := os.ReadFile("../../internal/printing/testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("print through PAPPL to ippeveprinter", func(t *testing.T) {
		before := completedJobs(ctx, t, simPrinterURI)
		printAndWait(ctx, t, queueURI, sample)
		// The PAPPL job completes once the CUPS ipp backend has handed the
		// document over; ippeveprinter then finishes its own job.
		waitFor(ctx, t, 60*time.Second, "a new completed job on ippeveprinter", func() (bool, string) {
			n := completedJobs(ctx, t, simPrinterURI)
			return n > before, fmt.Sprintf("%d completed jobs, %d before", n, before)
		})
	})

	t.Run("print to ippeveprinter directly", func(t *testing.T) {
		printAndWait(ctx, t, simPrinterURI, sample)
	})

	t.Run("scan through AirSane", func(t *testing.T) {
		url := scannerURL(t, rep.Services)
		t.Logf("eSCL: %s", url)
		for _, client := range []struct {
			name string
			new  func(string) (scanning.Scanner, error)
		}{{"mfp", scanning.NewMFP}, {"xml", scanning.NewXML}} {
			t.Run(client.name, func(t *testing.T) {
				s, err := client.new(url)
				if err != nil {
					t.Fatal(err)
				}
				// The _uscan._tcp advertisement can come before AirSane accepts connections.
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

	// Every default the queue reports and PAPPL accepts reads back as set, so
	// the agent's set-only-on-change guard does not resend it every cycle. A
	// default the printer refuses (ps-printer-app refuses output-bin-default
	// face-down while listing it as supported) is left out of the check.
	t.Run("queue defaults read back", func(t *testing.T) {
		c := pappl.New(papplSystemURI)
		q := onlyQueue(ctx, t, c)
		fitted, dropped := device.FitPrint(otherDefaults(q.Caps), q.Caps)
		if len(dropped) != 0 {
			t.Fatalf("dropped %+v from values the queue reports", dropped)
		}
		t.Logf("caps %+v; setting %+v", q.Caps, fitted)
		cp.setDefaults(fitted) // the agent then wants the same and leaves the queue alone
		rejected, err := c.SetDefaults(ctx, q.URI, ippcaps.Defaults(fitted))
		if err != nil {
			t.Fatalf("set-printer-attributes: %v", err)
		}
		refused := func(attrs ...string) bool {
			return slices.ContainsFunc(rejected, func(a goipp.Attribute) bool { return slices.Contains(attrs, a.Name) })
		}
		t.Logf("refused: %v", rejected)
		// The agent may still be applying the previous desired defaults: poll.
		waitFor(ctx, t, 30*time.Second, "every set default to read back", func() (bool, string) {
			got := onlyQueue(ctx, t, c).Caps
			var bad []string
			media := []string{"media-default", "media-col-default"}
			for _, k := range []struct {
				kind            string
				attrs           []string
				set, read, zero any
			}{
				{"media", media, fitted.Media, got.MediaDefault, ""},
				{"mediaSource", media, fitted.MediaSource, got.MediaSourceDefault, ""},
				{"mediaType", media, fitted.MediaType, got.MediaTypeDefault, ""},
				{"sides", []string{"sides-default"}, fitted.Sides, got.SidesDefault, ""},
				{"colorMode", []string{"print-color-mode-default"}, fitted.ColorMode, got.ColorModeDefault, ""},
				{"quality", []string{"print-quality-default"}, fitted.Quality, got.QualityDefault, ""},
				{"resolution", []string{"printer-resolution-default"}, fitted.Resolution, got.ResolutionDefault, 0},
				{"contentOptimize", []string{"print-content-optimize-default"}, fitted.ContentOptimize, got.ContentOptimizeDefault, ""},
				{"orientation", []string{"orientation-requested-default"}, fitted.Orientation, got.OrientationDefault, ""},
				{"outputBin", []string{"output-bin-default"}, fitted.OutputBin, got.OutputBinDefault, ""},
			} {
				if k.set != k.zero && k.set != k.read && !refused(k.attrs...) {
					bad = append(bad, fmt.Sprintf("%s set %v, reads %v", k.kind, k.set, k.read))
				}
			}
			return len(bad) == 0, strings.Join(bad, "; ")
		})
	})

	if _, errs := cp.report(); len(errs) > 0 {
		t.Errorf("control plane saw bad agent requests: %v", errs)
	}
}

// genericPostScriptDriver asks ps-printer-app for every driver and returns
// the first one whose name or description mentions generic PostScript.
// Driverless entries are skipped: libppd makes them for printers it finds on
// the network (here ippeveprinter), and PAPPL refuses them for any other
// device. Level 1 drivers come last: their foomatic PPD renders with
// Ghostscript's pswrite device, which Ghostscript 10 no longer has.
func genericPostScriptDriver(ctx context.Context, t *testing.T) string {
	t.Helper()
	c := pappl.New(papplSystemURI)
	var drivers []pappl.Driver
	waitFor(ctx, t, 90*time.Second, "ps-printer-app to list its drivers", func() (bool, string) {
		var err error
		drivers, err = c.FindDrivers(ctx, "")
		if err != nil {
			return false, err.Error()
		}
		return len(drivers) > 0, "no drivers listed"
	})
	var level1 string
	for _, d := range drivers {
		s := strings.ToLower(d.Name + " " + d.Description)
		switch {
		case !strings.Contains(s, "generic") || !strings.Contains(s, "postscript") || strings.Contains(s, "driverless"):
		case strings.Contains(s, "level 1"):
			if level1 == "" {
				level1 = d.Name
			}
		default:
			return d.Name
		}
	}
	if level1 != "" {
		return level1
	}
	list := make([]string, 0, len(drivers))
	for _, d := range drivers {
		list = append(list, d.Name+" ("+d.Description+")")
	}
	t.Fatalf("no generic PostScript driver among %d drivers:\n%s", len(drivers), strings.Join(list, "\n"))
	return ""
}

// waitReport waits for a report with the queue ok and both service types
// browsed, and for PAPPL to hold exactly the wanted queue. Checking PAPPL
// itself matters when the stack is reused: an earlier run's queue reports ok
// until the agent replaces it.
func waitReport(ctx context.Context, t *testing.T, cp *controlPlane, want device.QueueSpec, d time.Duration) device.AgentReport {
	t.Helper()
	var rep device.AgentReport
	c := pappl.New(papplSystemURI)
	waitFor(ctx, t, d, "an agent report with the queue ok and both service types", func() (bool, string) {
		r, _ := cp.report()
		if r == nil {
			return false, "no report yet"
		}
		rep = *r
		var types []string
		for _, s := range r.Services {
			types = append(types, s.Type+" "+s.Name)
		}
		state := fmt.Sprintf("queues %+v, services %q", r.Queues, types)
		if len(r.Queues) != 1 || r.Queues[0].State != device.QueueOK || r.Queues[0].PrinterURI == "" ||
			!hasService(r.Services, device.ServiceIPP) || !hasService(r.Services, device.ServiceUScan) {
			return false, state
		}
		printers, err := c.Printers(ctx)
		if err != nil {
			return false, state + "; get-printers: " + err.Error()
		}
		state += fmt.Sprintf("; PAPPL printers %+v", printers)
		// Location is not compared: PAPPL 1.4.12's Set-Printer-Attributes passes
		// printer-location to papplPrinterSetGeoLocation (printer-ipp.c), so it never sticks.
		return len(printers) == 1 && printers[0].Name == want.Name && printers[0].DeviceURI == want.DeviceURI &&
			printers[0].Driver == want.Driver && printers[0].Caps.MediaDefault == want.Defaults.Media &&
			printers[0].URI == r.Queues[0].PrinterURI, state
	})
	return rep
}

// onlyQueue is the printer app's one queue.
func onlyQueue(ctx context.Context, t *testing.T, c *pappl.Client) pappl.Printer {
	t.Helper()
	printers, err := c.Printers(ctx)
	if err != nil || len(printers) != 1 {
		t.Fatalf("get-printers: %+v, %v; want one queue", printers, err)
	}
	return printers[0]
}

// otherDefaults picks, for every kind caps reports, a supported value other
// than the current default when there is one.
func otherDefaults(caps device.PrinterCaps) device.PrintSettings {
	str := func(supported []string, def string) string {
		for _, v := range supported {
			if v != def {
				return v
			}
		}
		return def
	}
	num := func(supported []int, def int) int {
		for _, v := range supported {
			if v != def {
				return v
			}
		}
		return def
	}
	return device.PrintSettings{
		Media:           str(caps.Media, caps.MediaDefault),
		MediaSource:     str(caps.MediaSources, caps.MediaSourceDefault),
		MediaType:       str(caps.MediaTypes, caps.MediaTypeDefault),
		Sides:           str(caps.Sides, caps.SidesDefault),
		ColorMode:       str(caps.ColorModes, caps.ColorModeDefault),
		Quality:         str(caps.Qualities, caps.QualityDefault),
		Resolution:      num(caps.Resolutions, caps.ResolutionDefault),
		ContentOptimize: str(caps.ContentOptimize, caps.ContentOptimizeDefault),
		Orientation:     str(caps.Orientations, caps.OrientationDefault),
		NumberUp:        num(caps.NumberUp, caps.NumberUpDefault),
		PrintScaling:    str(caps.PrintScalings, caps.PrintScalingDefault),
		OutputBin:       str(caps.OutputBins, caps.OutputBinDefault),
		Finishings:      str(caps.Finishings, caps.FinishingsDefault),
	}
}

func hasService(services []device.DNSSDService, typ string) bool {
	return slices.ContainsFunc(services, func(s device.DNSSDService) bool { return s.Type == typ && s.Name != "" })
}

// scannerURL builds the eSCL root URL from the browsed _uscan._tcp instance:
// its port and TXT rs on the loopback address, as the controller does with the
// node IP.
func scannerURL(t *testing.T, services []device.DNSSDService) string {
	t.Helper()
	for _, s := range services {
		if s.Type == device.ServiceUScan {
			return fmt.Sprintf("http://127.0.0.1:%d/%s", s.Port, strings.Trim(s.TXT["rs"], "/"))
		}
	}
	t.Fatalf("no %s service in %+v", device.ServiceUScan, services)
	return ""
}

func printAndWait(ctx context.Context, t *testing.T, uri string, doc []byte) {
	t.Helper()
	c := printing.New(uri)
	id, err := c.Submit(ctx, "sample.pdf", "it", "application/pdf", bytes.NewReader(doc), printing.PrintOptions{Copies: 1})
	if err != nil {
		t.Fatalf("submit to %s: %v", uri, err)
	}
	waitFor(ctx, t, 60*time.Second, fmt.Sprintf("job %d on %s to complete", id, uri), func() (bool, string) {
		j, err := c.Job(ctx, id)
		if err != nil {
			return false, err.Error()
		}
		switch j.State {
		case "canceled", "aborted":
			t.Fatalf("job %d on %s ended %s: %v", id, uri, j.State, j.Reasons)
		}
		return j.State == "completed", fmt.Sprintf("%s %v", j.State, j.Reasons)
	})
}

// completedJobs counts the printer's completed jobs (Get-Jobs which-jobs=completed).
func completedJobs(ctx context.Context, t *testing.T, uri string) int {
	t.Helper()
	jobs, err := printing.New(uri).Jobs(ctx, -1)
	if err != nil {
		t.Fatalf("get-jobs on %s: %v", uri, err)
	}
	n := 0
	for _, j := range jobs {
		if j.State == "completed" {
			n++
		}
	}
	return n
}

func scanOnePage(ctx context.Context, t *testing.T, s scanning.Scanner) {
	t.Helper()
	caps, err := s.Capabilities(ctx)
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if !slices.Contains(caps.Sources, "platen") || len(caps.Resolutions) == 0 || len(caps.ColorModes) == 0 {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}
	res := slices.Min(caps.Resolutions) // the SANE test pattern is quick at any resolution; the lowest is quickest
	set := scanning.Settings{Source: "platen", Resolution: res, ColorMode: caps.ColorModes[0], Format: "jpeg"}
	job, err := s.Start(ctx, set)
	if err != nil {
		t.Fatalf("start %+v: %v", set, err)
	}
	var pages [][]byte
	for {
		rc, err := s.NextPage(ctx, job)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("page %d: %v", len(pages)+1, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read page %d: %v", len(pages)+1, err)
		}
		pages = append(pages, b)
	}
	if len(pages) != 1 {
		t.Fatalf("got %d pages from the platen, want 1", len(pages))
	}
	img, err := jpeg.Decode(bytes.NewReader(pages[0]))
	if err != nil {
		t.Fatalf("page is not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
		t.Fatalf("empty page: %v", b)
	}
	var pdf bytes.Buffer
	page := func(i int) (io.ReadSeekCloser, error) {
		return struct {
			io.ReadSeeker
			io.Closer
		}{bytes.NewReader(pages[i]), io.NopCloser(nil)}, nil
	}
	if err := scanning.WriteJPEGPDF(&pdf, len(pages), page, res); err != nil {
		t.Fatalf("pdf: %v", err)
	}
	if !bytes.HasPrefix(pdf.Bytes(), []byte("%PDF-")) {
		t.Fatalf("pdf output starts %q", pdf.Bytes()[:min(8, pdf.Len())])
	}
}

// waitFor polls cond every second until it holds, d passes or ctx ends, and
// fails with cond's last state on a timeout.
func waitFor(ctx context.Context, t *testing.T, d time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		ok, state := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s; last state: %s", d, what, state)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("stopped waiting for %s: %v; last state: %s", what, ctx.Err(), state)
		case <-tick.C:
		}
	}
}
