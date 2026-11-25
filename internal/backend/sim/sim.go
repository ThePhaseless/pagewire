// Package sim is the simulated device backend: in-process virtual devices on
// 127.0.0.1 instead of a device pod. It serves a virtual IPP printer (go-mfp)
// for the ipp-usb and sim families, a fake PAPPL system (papplfake) with
// devices derived from the fixture for PAPPL families, and a virtual eSCL
// scanner (go-mfp). An in-memory agent (agent.Core) reports the fixture
// inventory, reconciles the PAPPL queue and installs blobs from a synthetic
// vendor (blobtest), so nothing reaches the network or spawns a process.
package sim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenPrinting/go-mfp/abstract"
	"github.com/OpenPrinting/go-mfp/proto/escl"
	"github.com/OpenPrinting/go-mfp/proto/ipp"
	"github.com/OpenPrinting/go-mfp/transport"
	"github.com/OpenPrinting/go-mfp/util/generic"
	"github.com/OpenPrinting/go-mfp/util/optional"
	"github.com/OpenPrinting/goipp"
	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/pagewire/internal/agent"
	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/blobs/blobtest"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/pappl/papplfake"
)

const (
	hostIP    = "127.0.0.1"
	vendorURL = "https://vendor.sim.invalid/"
	podUID    = "sim"
)

// Options configure the simulated backend.
type Options struct {
	Fixture string // catalog fixture reported as the USB inventory; "none" (or unknown): empty
	DataDir string // documents land in DataDir/sim-output, blobs in DataDir/sim-blobs
	// ScanDelay holds each scanned page this long, so a scan can be cancelled
	// while it runs (e2e tests). Zero: no delay.
	ScanDelay time.Duration
}

// Backend is the simulated device.Backend. It implements neither
// device.PodRestarter nor device.AgentTokenSource.
type Backend struct {
	opts   Options
	inv    device.Inventory
	vendor *http.Client
	keys   openpgp.EntityList

	mu        sync.Mutex
	report    device.ReportFunc
	spec      *device.Spec // nil: nothing runs
	hash      string
	servers   []*http.Server
	endpoints device.Endpoints
	papplURL  string
	core      *agent.Core
	stop      context.CancelFunc
}

// New returns a backend with nothing running. It generates the synthetic
// vendor's signing key and signs one archive per catalog blob.
func New(o Options) *Backend {
	key := blobtest.NewKey()
	files := map[string][]byte{}
	// shortcut: every blob gets the synthetic HPLIP archive (the only blob
	// format today); add per-format archives to blobtest when a second exists.
	run := blobtest.Archive(runtime.GOARCH)
	sig := blobtest.Sign(key, run)
	for _, b := range catalog.Default().Blobs {
		files[blobURL(b.ID)] = run
		files[blobURL(b.ID)+".asc"] = sig
	}
	go testPage() // warm the scanner page off Apply's path
	return &Backend{
		opts:   o,
		inv:    simulatedInventory(o.Fixture),
		vendor: &http.Client{Transport: blobtest.Transport(files)},
		keys:   openpgp.EntityList{key},
	}
}

func blobURL(id string) string { return vendorURL + id + ".run" }

// simulatedInventory is the fixture's inventory, flagged as simulated so the
// UI never presents it as real hardware.
func simulatedInventory(fixture string) device.Inventory {
	inv := catalog.Fixture(fixture)
	inv.Simulated = true
	return inv
}

// SetReporter sets the controller's report handler. Call it before the first
// non-nil Apply; a running agent keeps the handler it started with.
func (b *Backend) SetReporter(r device.ReportFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.report = r
}

// Apply starts the virtual devices for spec, swapping them when the spec hash
// changes, or stops everything for a nil spec. It never calls the ReportFunc
// and never waits for the agent goroutine, so a controller holding a lock its
// report handler also takes cannot deadlock.
func (b *Backend) Apply(ctx context.Context, spec *device.Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if spec == nil {
		b.stopLocked()
		return nil
	}
	if b.report == nil {
		return errors.New("sim: Apply before SetReporter")
	}
	hash := device.SpecHash(spec)
	if b.spec != nil && hash == b.hash {
		return nil
	}
	b.closeServersLocked()
	if err := b.startLocked(ctx, spec); err != nil {
		b.stopLocked()
		return err
	}
	s := *spec
	s.SaneBackends, s.Blobs = slices.Clone(spec.SaneBackends), slices.Clone(spec.Blobs)
	s.AcceptedEULAs = slices.Clone(spec.AcceptedEULAs)
	b.spec, b.hash = &s, hash
	if b.core != nil {
		b.core.Wake()
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	core := agent.NewCore(podUID, b.inv)
	core.Blobs = func() []device.BlobStatus { return b.fetchBlobs(runCtx) }
	stager := &agent.Stager{
		Dir: b.blobDir(), Client: b.vendor, Wake: core.Wake,
		Blob: func(id string) (catalog.Blob, bool) {
			cb, ok := catalog.Default().Blob(id)
			return b.simBlob(cb), ok
		},
		Keys: func(string) (openpgp.EntityList, error) { return b.keys, nil },
	}
	core.Stage = stager.Stage
	core.PAPPLURL = func() string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.papplURL
	}
	b.core, b.stop = core, cancel
	go core.Loop(runCtx, b.report)
	return nil
}

// Observe reports running with the endpoints of the current spec, or absent.
func (b *Backend) Observe(context.Context) (device.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spec == nil {
		return device.Observation{Phase: device.PhaseAbsent}, nil
	}
	return device.Observation{Phase: device.PhaseRunning, HostIP: hostIP, Endpoints: b.endpoints}, nil
}

// stopLocked cancels the agent without waiting for it (an in-flight report's
// result is dropped by Core.Loop) and closes every server.
func (b *Backend) stopLocked() {
	if b.stop != nil {
		b.stop()
	}
	b.core, b.stop = nil, nil
	b.closeServersLocked()
	b.spec, b.hash = nil, ""
}

func (b *Backend) closeServersLocked() {
	for _, s := range b.servers {
		_ = s.Close() // closes listeners and connections without waiting for handlers
	}
	b.servers, b.endpoints, b.papplURL = nil, device.Endpoints{}, ""
}

func (b *Backend) startLocked(ctx context.Context, spec *device.Spec) error {
	if spec.Mode == device.ModeProbe {
		return nil
	}
	out := filepath.Join(b.opts.DataDir, "sim-output")
	if spec.Printer != "" {
		fam, _ := catalog.Default().Printer(spec.Printer)
		if fam.App != "" {
			if err := os.MkdirAll(out, 0o750); err != nil {
				slog.Warn("sim: printed documents will be lost", "err", err)
			}
			addr, err := b.serve(ctx, func(string) http.Handler { return papplfake.New(b.papplOptions(fam, out)) })
			if err != nil {
				return err
			}
			b.papplURL = "ipp://" + addr + "/ipp/system"
		} else {
			addr, err := b.serve(ctx, func(addr string) http.Handler { return b.newPrinter(spec, addr, out) })
			if err != nil {
				return err
			}
			b.endpoints.PrinterURI = "ipp://" + addr + "/ipp/print"
		}
	}
	if spec.Scanner != "" {
		addr, err := b.serve(ctx, func(string) http.Handler { return slowPages(newScanner(), b.opts.ScanDelay) })
		if err != nil {
			return err
		}
		b.endpoints.ScannerURL = "http://" + addr + "/eSCL"
	}
	return nil
}

// serve listens on 127.0.0.1:0 and serves the handler built for that address.
func (b *Backend) serve(ctx context.Context, handler func(addr string) http.Handler) (string, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", hostIP+":0")
	if err != nil {
		return "", fmt.Errorf("sim: listen: %w", err)
	}
	addr := ln.Addr().String()
	srv := &http.Server{Handler: handler(addr), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	b.servers = append(b.servers, srv)
	return addr, nil
}

// papplOptions lists every printer-class fixture device the way the retrofit
// apps' CUPS backends would, with one driver matching its manufacturer and
// the index driver the dispatcher picks for it in fam.
func (b *Backend) papplOptions(fam catalog.Printer, out string) papplfake.Options {
	o := papplfake.Options{OutputDir: out}
	for _, d := range b.inv.Devices {
		if !isPrinter(d) {
			continue
		}
		id := device.ParseDeviceID(d.DeviceID)
		mfg, mdl := id["MFG"], id["MDL"]
		serial := "?serial=" + url.QueryEscape(d.Serial)
		o.Devices = append(o.Devices, papplfake.Device{
			URI: "cups:usb://" + url.PathEscape(mfg) + "/" + url.PathEscape(mdl) + serial, Info: mfg + " " + mdl + " (USB)", DeviceID: d.DeviceID,
		})
		if slices.Contains(fam.DeviceSchemes, "hp") {
			o.Devices = append(o.Devices, papplfake.Device{
				URI: "cups:hp:/usb/" + strings.ReplaceAll(mdl, " ", "_") + serial, Info: mdl + " (HPLIP)", DeviceID: d.DeviceID,
			})
		}
		o.Drivers = append(o.Drivers, papplfake.Driver{
			Name: fam.ID + "_" + strings.ToLower(strings.ReplaceAll(mfg, " ", "_")), Description: mfg + " (" + fam.App + ", simulated)", MatchMFG: mfg,
		})
		inv := device.Inventory{Devices: []device.USBDevice{d}}
		if dec, err := catalog.Default().Decide(inv, config.Device{Mode: config.ModeUSB, Driver: fam.ID}); err == nil && dec.Driver != "" {
			o.Drivers = append(o.Drivers, papplfake.Driver{Name: dec.Driver, Description: dec.DriverDescription, MatchMFG: mfg})
		}
	}
	return o
}

func isPrinter(d device.USBDevice) bool {
	return slices.ContainsFunc(d.Interfaces, func(i device.USBInterface) bool { return i.Class == 7 })
}

// fetchBlobs installs the spec's blobs from the synthetic vendor. It runs in
// the agent goroutine; Fetch is a no-op once a blob is installed.
func (b *Backend) fetchBlobs(ctx context.Context) []device.BlobStatus {
	b.mu.Lock()
	var refs []device.BlobRef
	var accepted []device.EULARef
	if b.spec != nil {
		refs, accepted = slices.Clone(b.spec.Blobs), slices.Clone(b.spec.AcceptedEULAs)
	}
	b.mu.Unlock()
	out := []device.BlobStatus{}
	for _, ref := range refs {
		cb, ok := catalog.Default().Blob(ref.ID)
		if !ok {
			out = append(out, device.BlobStatus{ID: ref.ID, Version: ref.Version, State: device.BlobFailed, Message: "unknown blob " + ref.ID})
			continue
		}
		out = append(out, blobs.Fetch(ctx, b.simBlob(cb), accepted, b.blobDir(), blobs.Options{Client: b.vendor, Keys: b.keys}))
	}
	return out
}

// simBlob is the sim's own copy of a catalog blob: never the real vendor URL
// or key, signature-checked against the synthetic vendor key.
func (b *Backend) simBlob(cb catalog.Blob) catalog.Blob {
	cb.Enabled, cb.URL = true, blobURL(cb.ID)
	cb.Verify = catalog.Verify{PGPSignatureURL: cb.URL + ".asc"}
	return cb
}

func (b *Backend) blobDir() string { return filepath.Join(b.opts.DataDir, "sim-blobs") }

// newPrinter builds go-mfp's virtual IPP printer for the ipp-usb and sim
// families, writing documents to out/<job-id>.bin.
func (b *Backend) newPrinter(spec *device.Spec, addr, out string) http.Handler {
	name := spec.Name
	if name == "" {
		name = "Simulated printer"
		if i := slices.IndexFunc(b.inv.Devices, isPrinter); i >= 0 && b.inv.Devices[i].Product != "" {
			name = b.inv.Devices[i].Product
		}
	}
	idle := 3
	attrs := &ipp.PrinterAttributes{
		PrinterName:             optional.New(name),
		PrinterLocation:         optional.New(spec.Location),
		PrinterInfo:             optional.New(name),
		PrinterMakeAndModel:     optional.New("pagewire virtual printer"),
		PrinterState:            &idle,
		PrinterStateReasons:     []ipp.KwPrinterStateReasons{"none"},
		PrinterIsAcceptingJobs:  optional.New(true),
		PrinterURISupported:     []string{"ipp://" + addr + "/ipp/print"},
		DocumentFormatDefault:   optional.New("application/pdf"),
		DocumentFormatSupported: []string{"application/pdf", "image/jpeg", "image/pwg-raster", "image/urf"},
		MediaSupported:          []ipp.KwMedia{"iso_a4_210x297mm", "na_letter_8.5x11in"},
		MarkerNames:             []string{"Black Toner"},
		MarkerColors:            []string{"#000000"},
		MarkerTypes:             []string{"toner"},
		MarkerLevels:            []int{80},
	}
	vp := &virtualPrinter{dir: out, done: map[int]bool{}, created: map[int]time.Time{}}
	p := ipp.NewPrinter(attrs, ipp.PrinterOptions{ServerOptions: ipp.ServerOptions{
		Hooks: ipp.ServerHooks{OnIPPRequest: vp.onRequest, OnIPPResponse: vp.onResponse},
	}})
	p.SetPrintBackend(vp)
	return p
}

// virtualPrinter is the go-mfp printer's backend plus the hooks that make
// its jobs complete. go-mfp (b3cc636803df) leaves every job pending-held
// after Send-Document, gives the backend no job handle and sends no
// date-time-at-creation, so the hooks pass the job id in, report written jobs
// as completed and add the creation time.
//
// shortcut: go-mfp still holds the job internally, so Cancel-Job on a
// completed job succeeds and which-jobs=completed omits it; fix upstream or
// rewrite those responses too if a client depends on them.
type virtualPrinter struct {
	dir     string
	mu      sync.Mutex
	done    map[int]bool      // job ids whose document was written
	created map[int]time.Time // when each job id was first answered (Create-Job)
	seq     int
}

// onRequest names each Send-Document after its job id: go-mfp hands
// document-name to the backend as JobName, and nothing else identifies the job.
func (v *virtualPrinter) onRequest(_ *transport.ServerQuery, m *goipp.Message) *goipp.Message {
	if goipp.Op(m.Code) != goipp.OpSendDocument {
		return nil
	}
	if id := jobID(m.Operation); id > 0 {
		m.Operation = slices.DeleteFunc(m.Operation, func(a goipp.Attribute) bool { return a.Name == "document-name" })
		m.Operation.Add(goipp.MakeAttribute("document-name", goipp.TagName, goipp.String(strconv.Itoa(id))))
	}
	return nil
}

// onResponse reports every written job as completed and stamps its creation time.
func (v *virtualPrinter) onResponse(_ *transport.ServerQuery, m *goipp.Message) *goipp.Message {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.complete(m.Job)
	m.Job = v.stamp(m.Job)
	for i := range m.Groups {
		if m.Groups[i].Tag == goipp.TagJobGroup {
			v.complete(m.Groups[i].Attrs)
			m.Groups[i].Attrs = v.stamp(m.Groups[i].Attrs)
		}
	}
	return nil
}

// stamp adds date-time-at-creation to a job's attributes, when missing.
func (v *virtualPrinter) stamp(attrs goipp.Attributes) goipp.Attributes {
	id := jobID(attrs)
	if id <= 0 || slices.ContainsFunc(attrs, func(a goipp.Attribute) bool { return a.Name == "date-time-at-creation" }) {
		return attrs
	}
	t, ok := v.created[id]
	if !ok {
		t = time.Now()
		v.created[id] = t
	}
	return append(attrs, goipp.MakeAttribute("date-time-at-creation", goipp.TagDateTime, goipp.Time{Time: t}))
}

func (v *virtualPrinter) complete(attrs goipp.Attributes) {
	if !v.done[jobID(attrs)] {
		return
	}
	for i := range attrs {
		switch attrs[i].Name {
		case "job-state":
			attrs[i].Values = goipp.Values{{T: goipp.TagEnum, V: goipp.Integer(9)}} // completed
		case "job-state-reasons":
			attrs[i].Values = goipp.Values{{T: goipp.TagKeyword, V: goipp.String("job-completed-successfully")}}
		}
	}
}

// PrintDocument writes the document to dir/<job-id>.bin and marks the job done.
func (v *virtualPrinter) PrintDocument(p abstract.PrinterRequest, body io.Reader) error {
	id, err := strconv.Atoi(p.JobName)
	name := p.JobName
	if err != nil || id <= 0 || name != strconv.Itoa(id) { // a Send-Document by job-uri: no id to complete
		v.mu.Lock()
		v.seq++
		id, name = 0, fmt.Sprintf("doc-%d", v.seq)
		v.mu.Unlock()
	}
	if err := writeFile(filepath.Join(v.dir, name+".bin"), body); err != nil {
		return err
	}
	if id > 0 {
		v.mu.Lock()
		v.done[id] = true
		v.mu.Unlock()
	}
	return nil
}

func writeFile(path string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.Create(path) //nolint:gosec // DataDir plus a numeric job id
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func jobID(attrs goipp.Attributes) int {
	for _, a := range attrs {
		if a.Name == "job-id" && len(a.Values) > 0 {
			if n, ok := a.Values[0].V.(goipp.Integer); ok {
				return int(n)
			}
		}
	}
	return 0
}

// newScanner builds go-mfp's virtual eSCL scanner: a platen and a two-page
// ADF (simplex and duplex), all holding the A4 300 dpi test page.
func newScanner() http.Handler {
	input := &abstract.InputCapabilities{
		MaxWidth:  abstract.A4Width,
		MaxHeight: abstract.A4Height,
		Profiles: []abstract.SettingsProfile{{
			ColorModes:  generic.MakeBitset(abstract.ColorModeColor, abstract.ColorModeMono),
			Depths:      generic.MakeBitset(abstract.ColorDepth8),
			Resolutions: []abstract.Resolution{{XResolution: 75, YResolution: 75}, {XResolution: 150, YResolution: 150}, {XResolution: 300, YResolution: 300}},
		}},
	}
	caps := &abstract.ScannerCapabilities{
		MakeAndModel:    "pagewire virtual scanner",
		DocumentFormats: []string{"image/jpeg"},
		Platen:          input,
		ADFSimplex:      input,
		ADFDuplex:       input,
		ADFCapacity:     50,
	}
	page := testPage()
	vs := &abstract.VirtualScanner{
		ScanCaps:    caps,
		Resolution:  abstract.Resolution{XResolution: 300, YResolution: 300},
		PlatenImage: page,
		ADFImages:   [][]byte{page, page},
	}
	return escl.NewAbstractServer(escl.AbstractServerOptions{Scanner: vs, BasePath: "/eSCL"})
}

// slowPages holds every page fetch (GET .../NextDocument) for d, or until
// the client gives up; other eSCL requests pass straight through.
func slowPages(h http.Handler, d time.Duration) http.Handler {
	if d <= 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/NextDocument") {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
			case <-r.Context().Done():
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// testPage is an A4 page at 300 dpi (2480x3508) with eight colour bars,
// encoded once per process.
var testPage = sync.OnceValue(func() []byte {
	const w, h = 2480, 3508
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bars := []color.RGBA{
		{255, 255, 255, 255},
		{255, 255, 0, 255},
		{0, 255, 255, 255},
		{0, 255, 0, 255},
		{255, 0, 255, 255},
		{255, 0, 0, 255},
		{0, 0, 255, 255},
		{0, 0, 0, 255},
	}
	for i, c := range bars {
		draw.Draw(img, image.Rect(i*w/len(bars), 0, (i+1)*w/len(bars), h), &image.Uniform{C: c}, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		panic("sim: encode test page: " + err.Error()) // in-memory encode of a valid image
	}
	return buf.Bytes()
})
