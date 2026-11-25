// Package sim is the simulated device backend: in-process virtual devices on
// 127.0.0.1 instead of a device pod. For one workload spec it serves a
// virtual IPP printer (go-mfp) per ipp-usb and sim printer, a fake PAPPL
// system (papplfake) per PAPPL family with devices derived from that family's
// printers, and a virtual eSCL scanner (go-mfp) per scanner, each with the
// capabilities of its fixture device. An in-memory agent (agent.Core) reports
// the fixture inventory, reconciles the PAPPL queues and installs blobs from
// a synthetic vendor (blobtest), so nothing reaches the network or spawns a
// process.
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
	"github.com/ThePhaseless/pagewire/internal/ippcaps"
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

var _ device.Backend = (*Backend)(nil)

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
	endpoints []device.Endpoint
	// papplURLs maps each PAPPL family's fixed system URI
	// (device.PAPPLSystemURI of its catalog port, what the controller sends)
	// to its papplfake's random-port URL; pappls holds those fakes by family.
	papplURLs map[string]string
	pappls    map[string]*papplfake.System
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
// non-empty Apply; a running agent keeps the handler it started with.
func (b *Backend) SetReporter(r device.ReportFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.report = r
}

// Apply starts the virtual devices for the one spec, swapping them when the
// spec hash changes, or stops everything for empty specs. More than one spec
// is an error: the sim is a single node-less workload. Apply never calls the
// ReportFunc and never waits for the agent goroutine, so a controller holding
// a lock its report handler also takes cannot deadlock.
func (b *Backend) Apply(ctx context.Context, specs []*device.Spec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(specs) == 0 {
		b.stopLocked()
		return nil
	}
	if len(specs) > 1 || specs[0] == nil {
		return fmt.Errorf("sim: Apply takes at most one workload spec, got %d", len(specs))
	}
	spec := specs[0]
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
	s.Printers, s.Scanners = slices.Clone(spec.Printers), slices.Clone(spec.Scanners)
	for i := range s.Scanners {
		s.Scanners[i].SaneBackends = slices.Clone(s.Scanners[i].SaneBackends)
	}
	s.Blobs, s.AcceptedEULAs = slices.Clone(spec.Blobs), slices.Clone(spec.AcceptedEULAs)
	b.spec, b.hash = &s, hash
	if b.core != nil {
		b.core.Wake() // the next cycle reads the new papplURLs
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	core := agent.NewCore(podUID, b.inv)
	core.Attached = func() []device.USBDevice { return slices.Clone(b.inv.Devices) } // b.inv never changes
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
	// Read every cycle: a re-Apply's fakes listen on new ports, and the agent
	// keys a new PAPPL client by the new URL. Unknown URIs give "".
	core.SystemURL = func(u string) string {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.papplURLs[u]
	}
	b.core, b.stop = core, cancel
	go core.Loop(runCtx, b.report)
	return nil
}

// Observe reports the node-less workload running with one endpoint per
// served entry (PAPPL printers' PrinterURI empty), or absent.
func (b *Backend) Observe(context.Context) ([]device.Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.spec == nil {
		return []device.Observation{{Phase: device.PhaseAbsent, Endpoints: []device.Endpoint{}}}, nil
	}
	return []device.Observation{{Phase: device.PhaseRunning, HostIP: hostIP, Endpoints: slices.Clone(b.endpoints)}}, nil
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
	b.servers, b.endpoints, b.papplURLs, b.pappls = nil, []device.Endpoint{}, map[string]string{}, map[string]*papplfake.System{}
}

func (b *Backend) startLocked(ctx context.Context, spec *device.Spec) error {
	if spec.Mode == device.ModeProbe {
		return nil
	}
	out := filepath.Join(b.opts.DataDir, "sim-output")
	if len(spec.Printers) > 0 {
		if err := os.MkdirAll(out, 0o750); err != nil {
			slog.Warn("sim: printed documents will be lost", "err", err)
		}
	}
	families := map[string][]device.PrinterSpec{} // PAPPL family id -> its printers
	var order []catalog.Printer
	for _, p := range spec.Printers {
		fam, _ := catalog.Default().Printer(p.Family)
		if fam.App != "" {
			if _, ok := families[fam.ID]; !ok {
				order = append(order, fam)
			}
			families[fam.ID] = append(families[fam.ID], p)
			b.endpoints = append(b.endpoints, device.Endpoint{ID: p.ID})
			continue
		}
		addr, err := b.serve(ctx, func(addr string) http.Handler { return b.newPrinter(p, addr, out) })
		if err != nil {
			return err
		}
		b.endpoints = append(b.endpoints, device.Endpoint{ID: p.ID, PrinterURI: "ipp://" + addr + "/ipp/print"})
	}
	for _, fam := range order {
		sys := papplfake.New(b.papplOptions(fam, families[fam.ID], out))
		addr, err := b.serve(ctx, func(string) http.Handler { return sys })
		if err != nil {
			return err
		}
		b.pappls[fam.ID] = sys
		b.papplURLs[device.PAPPLSystemURI(fam.Port)] = "ipp://" + addr + "/ipp/system"
	}
	for _, s := range spec.Scanners {
		d, _ := b.device(s.USB)
		addr, err := b.serve(ctx, func(string) http.Handler { return slowPages(newScanner(d), b.opts.ScanDelay) })
		if err != nil {
			return err
		}
		b.endpoints = append(b.endpoints, device.Endpoint{ID: s.ID, ScannerURL: "http://" + addr + "/eSCL"})
	}
	return nil
}

// device is the fixture device with key k; false for a simulated entry (zero
// key) or a device the fixture lacks.
func (b *Backend) device(k device.USBKey) (device.USBDevice, bool) {
	i := slices.IndexFunc(b.inv.Devices, func(d device.USBDevice) bool { return k != (device.USBKey{}) && d.Key() == k })
	if i < 0 {
		return device.USBDevice{}, false
	}
	return b.inv.Devices[i], true
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

// papplOptions lists the fixture devices of fam's printers the way the
// retrofit apps' CUPS backends would, with one driver matching each device's
// manufacturer and the index driver the dispatcher picks for it in fam.
//
// shortcut: papplfake reports one caps set for all its queues, so a family
// holding a fixture model with its own caps (the Samsung) reports those for
// every queue; give papplfake per-queue caps when a test puts two models in
// one family.
func (b *Backend) papplOptions(fam catalog.Printer, printers []device.PrinterSpec, out string) papplfake.Options {
	o := papplfake.Options{OutputDir: out}
	addDriver := func(d papplfake.Driver) {
		if !slices.ContainsFunc(o.Drivers, func(x papplfake.Driver) bool { return x.Name == d.Name }) {
			o.Drivers = append(o.Drivers, d)
		}
	}
	for _, p := range printers {
		d, ok := b.device(p.USB)
		if canPrint, _ := catalog.Default().Roles(d); !ok || !canPrint {
			continue
		}
		if c, ok := papplCaps[modelKey(d)]; ok && o.Caps.Formats == nil {
			o.Caps = c // the first model with its own caps; zero: papplfake's default
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
		addDriver(papplfake.Driver{
			Name: fam.ID + "_" + strings.ToLower(strings.ReplaceAll(mfg, " ", "_")), Description: mfg + " (" + fam.App + ", simulated)", MatchMFG: mfg,
		})
		if dec := catalog.Default().DecidePrinter(&d, config.Printer{Mode: config.ModeUSB, Driver: fam.ID}); dec.Driver != "" {
			addDriver(papplfake.Driver{Name: dec.Driver, Description: dec.DriverDescription, MatchMFG: mfg})
		}
	}
	return o
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

// Fixture models (vendor:product) with capabilities of their own.
const (
	samsungSCX4300 = "04e8:342e"
	hpOfficeJet    = "03f0:2754"
	canonLiDE100   = "04a9:1904"
)

func modelKey(d device.USBDevice) string { return d.VendorID + ":" + d.ProductID }

// papplCaps are the caps a PAPPL family's fake reports for a fixture model.
// Other models get papplfake's default.
var papplCaps = map[string]device.PrinterCaps{
	// The Samsung SCX-4300's live legacy queue: monochrome and one-sided, no
	// tray, type, bin, finishings, layout or content kind; PAPPL always
	// reports copies 1-999.
	samsungSCX4300: {
		Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, MediaDefault: "na_letter_8.5x11in",
		Sides: []string{"one-sided"}, SidesDefault: "one-sided",
		ColorModes: []string{"monochrome"}, ColorModeDefault: "monochrome",
		Qualities: []string{"draft", "normal", "high"}, QualityDefault: "normal",
		Resolutions: []int{300, 600}, ResolutionDefault: 600,
		CopiesMax: 999,
		Formats:   []string{"application/pdf", "image/jpeg", "image/png", "image/pwg-raster", "image/urf"},
	},
}

// virtualCaps are the caps of an ipp-usb or sim printer on device d (zero for
// a simulated printer). The HP OfficeJet reports every option kind, with a
// single output bin and finishing; any other printer A4 and Letter plus
// copies 1-99.
func virtualCaps(d device.USBDevice) device.PrinterCaps {
	if modelKey(d) == hpOfficeJet {
		return device.PrinterCaps{
			Media: []string{
				"iso_a4_210x297mm", "na_letter_8.5x11in", "na_legal_8.5x14in", "na_number-10_4.125x9.5in", "na_index-4x6_4x6in",
			},
			MediaDefault: "iso_a4_210x297mm",
			MediaSources: []string{"auto", "tray-1", "manual"}, MediaSourceDefault: "auto",
			MediaTypes: []string{"stationery", "photographic-glossy"}, MediaTypeDefault: "stationery",
			Sides: []string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}, SidesDefault: "one-sided",
			ColorModes: []string{"auto", "color", "monochrome"}, ColorModeDefault: "auto",
			Qualities: []string{"draft", "normal", "high"}, QualityDefault: "normal",
			Resolutions: []int{300, 600}, ResolutionDefault: 600,
			ContentOptimize: []string{"auto", "photo", "text"}, ContentOptimizeDefault: "auto",
			Orientations: []string{"portrait", "landscape"}, OrientationDefault: "portrait",
			NumberUp: []int{1, 2, 4}, NumberUpDefault: 1,
			PrintScalings: []string{"auto", "fit", "fill", "none"}, PrintScalingDefault: "auto",
			OutputBins: []string{"face-down"}, OutputBinDefault: "face-down",
			Finishings: []string{"none"}, FinishingsDefault: "none",
			CopiesMax: 99,
		}
	}
	return device.PrinterCaps{Media: []string{"iso_a4_210x297mm", "na_letter_8.5x11in"}, CopiesMax: 99}
}

// setCaps writes c into go-mfp's job-template and media attributes; empty
// lists and defaults stay unreported.
func setCaps(a *ipp.PrinterAttributes, c device.PrinterCaps) {
	enums := func(table map[string]goipp.Integer, vs ...string) []int {
		var out []int
		for _, v := range vs {
			if n, ok := table[v]; ok {
				out = append(out, int(n))
			}
		}
		return out
	}
	enum := func(table map[string]goipp.Integer, v string) optional.Val[int] {
		if n, ok := table[v]; ok {
			return optional.New(int(n))
		}
		return nil
	}
	for _, m := range c.Media {
		a.MediaSupported = append(a.MediaSupported, ipp.KwMedia(m))
	}
	a.MediaDefault = optional.NotZero(ipp.KwMedia(c.MediaDefault))
	a.MediaSourceSupported, a.MediaTypeSupported = c.MediaSources, c.MediaTypes
	if c.MediaSourceDefault != "" || c.MediaTypeDefault != "" {
		a.MediaColDefault = optional.New(ipp.MediaCol{
			MediaSizeName: optional.NotZero(c.MediaDefault),
			MediaSource:   optional.NotZero(c.MediaSourceDefault),
			MediaType:     optional.NotZero(c.MediaTypeDefault),
		})
	}
	for _, v := range c.Sides {
		a.SidesSupported = append(a.SidesSupported, ipp.KwSides(v))
	}
	a.SidesDefault = optional.NotZero(ipp.KwSides(c.SidesDefault))
	a.PrintColorModeSupported, a.PrintColorModeDefault = c.ColorModes, optional.NotZero(c.ColorModeDefault)
	a.PrintQualitySupported, a.PrintQualityDefault = enums(ippcaps.Qualities, c.Qualities...), enum(ippcaps.Qualities, c.QualityDefault)
	for _, r := range c.Resolutions {
		a.PrinterResolutionSupported = append(a.PrinterResolutionSupported, goipp.Resolution{Xres: r, Yres: r, Units: goipp.UnitsDpi})
	}
	if c.ResolutionDefault > 0 {
		a.PrinterResolutionDefault = optional.New(goipp.Resolution{Xres: c.ResolutionDefault, Yres: c.ResolutionDefault, Units: goipp.UnitsDpi})
	}
	a.PrintContentOptimizeSupported, a.PrintContentOptimizeDefault = c.ContentOptimize, optional.NotZero(c.ContentOptimizeDefault)
	a.OrientationRequestedSupported = enums(ippcaps.Orientations, c.Orientations...)
	a.OrientationRequestedDefault = enum(ippcaps.Orientations, c.OrientationDefault)
	for _, n := range c.NumberUp {
		a.NumberUpSupported = append(a.NumberUpSupported, goipp.Integer(int32(n))) //nolint:gosec // small fixture values
	}
	a.NumberUpDefault = optional.NotZero(c.NumberUpDefault)
	a.PrintScalingSupported, a.PrintScalingDefault = c.PrintScalings, optional.NotZero(c.PrintScalingDefault)
	a.OutputBinSupported, a.OutputBinDefault = c.OutputBins, optional.NotZero(c.OutputBinDefault)
	a.FinishingsSupported, a.FinishingsDefault = enums(ippcaps.Finishings, c.Finishings...), enums(ippcaps.Finishings, c.FinishingsDefault)
	if c.CopiesMax > 0 {
		a.CopiesSupported, a.CopiesDefault = optional.New(goipp.Range{Lower: 1, Upper: c.CopiesMax}), optional.New(1)
	}
}

// newPrinter builds go-mfp's virtual IPP printer for ipp-usb or sim printer
// p, writing documents to out/<id>-<job-id>.bin.
func (b *Backend) newPrinter(p device.PrinterSpec, addr, out string) http.Handler {
	d, _ := b.device(p.USB)
	name := p.Name
	if name == "" {
		name = d.Product
	}
	if name == "" {
		name = "Simulated printer"
	}
	idle := 3
	attrs := &ipp.PrinterAttributes{
		PrinterName:             optional.New(name),
		PrinterLocation:         optional.New(p.Location),
		PrinterInfo:             optional.New(name),
		PrinterMakeAndModel:     optional.New("pagewire virtual printer"),
		PrinterState:            &idle,
		PrinterStateReasons:     []ipp.KwPrinterStateReasons{"none"},
		PrinterIsAcceptingJobs:  optional.New(true),
		PrinterURISupported:     []string{"ipp://" + addr + "/ipp/print"},
		DocumentFormatDefault:   optional.New("application/pdf"),
		DocumentFormatSupported: []string{"application/pdf", "image/jpeg", "image/pwg-raster", "image/urf"},
		MarkerNames:             []string{"Black Toner"},
		MarkerColors:            []string{"#000000"},
		MarkerTypes:             []string{"toner"},
		MarkerLevels:            []int{80},
	}
	setCaps(attrs, virtualCaps(d))
	vp := &virtualPrinter{dir: out, id: p.ID, done: map[int]bool{}, created: map[int]time.Time{}}
	pr := ipp.NewPrinter(attrs, ipp.PrinterOptions{ServerOptions: ipp.ServerOptions{
		Hooks: ipp.ServerHooks{OnIPPRequest: vp.onRequest, OnIPPResponse: vp.onResponse},
	}})
	pr.SetPrintBackend(vp)
	return pr
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
	id      string // the printer's entry id, prefixed to its document names
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

// PrintDocument writes the document to dir/<id>-<job-id>.bin and marks the job done.
func (v *virtualPrinter) PrintDocument(p abstract.PrinterRequest, body io.Reader) error {
	id, err := strconv.Atoi(p.JobName)
	name := p.JobName
	if err != nil || id <= 0 || name != strconv.Itoa(id) { // a Send-Document by job-uri: no id to complete
		v.mu.Lock()
		v.seq++
		id, name = 0, fmt.Sprintf("doc-%d", v.seq)
		v.mu.Unlock()
	}
	if err := writeFile(filepath.Join(v.dir, v.id+"-"+name+".bin"), body); err != nil {
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

// newScanner builds go-mfp's virtual eSCL scanner for device d (zero for the
// simulated scanner), every input holding the A4 300 dpi test page and the
// ADF two pages:
//   - the Samsung SCX-4300 as AirSane reports it: platen and simplex ADF,
//     75-600 dpi, colour and greyscale, no intents;
//   - the CanoScan LiDE 100: platen only, 75-1200 dpi, colour and greyscale,
//     intents document and photo;
//   - any other: platen and ADF (simplex and duplex), 75-300 dpi, colour and
//     greyscale, intents document, text-and-graphic, photo and preview.
func newScanner(d device.USBDevice) http.Handler {
	input := func(intents []abstract.Intent, dpi ...int) *abstract.InputCapabilities {
		res := make([]abstract.Resolution, len(dpi))
		for i, r := range dpi {
			res[i] = abstract.Resolution{XResolution: r, YResolution: r}
		}
		return &abstract.InputCapabilities{
			MaxWidth:  abstract.A4Width,
			MaxHeight: abstract.A4Height,
			Intents:   generic.MakeBitset(intents...),
			Profiles: []abstract.SettingsProfile{{
				ColorModes:  generic.MakeBitset(abstract.ColorModeColor, abstract.ColorModeMono),
				Depths:      generic.MakeBitset(abstract.ColorDepth8),
				Resolutions: res,
			}},
		}
	}
	page := testPage()
	caps := &abstract.ScannerCapabilities{MakeAndModel: "pagewire virtual scanner", DocumentFormats: []string{"image/jpeg"}}
	vs := &abstract.VirtualScanner{
		ScanCaps:    caps,
		Resolution:  abstract.Resolution{XResolution: 300, YResolution: 300},
		PlatenImage: page,
		ADFImages:   [][]byte{page, page},
	}
	switch modelKey(d) {
	case samsungSCX4300:
		caps.Platen = input(nil, 75, 150, 300, 600)
		caps.ADFSimplex, caps.ADFCapacity = caps.Platen, 50
	case canonLiDE100:
		caps.Platen = input([]abstract.Intent{abstract.IntentDocument, abstract.IntentPhoto}, 75, 150, 300, 600, 1200)
		vs.ADFImages = nil
	default:
		caps.Platen = input([]abstract.Intent{
			abstract.IntentDocument, abstract.IntentTextAndGraphic, abstract.IntentPhoto, abstract.IntentPreview,
		}, 75, 150, 300)
		caps.ADFSimplex, caps.ADFDuplex, caps.ADFCapacity = caps.Platen, caps.Platen, 50
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
