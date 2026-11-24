package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/ThePhaseless/printscan/internal/device"
	"github.com/ThePhaseless/printscan/internal/dnssd"
	"github.com/ThePhaseless/printscan/internal/pappl"
	"github.com/ThePhaseless/printscan/internal/usb"
)

const (
	defaultInterval  = 5 * time.Second
	minBackoff       = time.Second
	maxBackoff       = 30 * time.Second
	browseWait       = 2 * time.Second
	reconcileTimeout = 30 * time.Second

	// MsgNoPAPPLURL is the QueueResult message when a Queue is desired but the
	// agent was started without --pappl-url.
	MsgNoPAPPLURL = "agent has no --pappl-url"
)

// Browser browses DNS-SD for the given service types.
type Browser func(ctx context.Context, types []string) ([]device.DNSSDService, error)

// Core is the transport-independent agent: one cycle's logic plus Loop. Make
// it with NewCore (a zero Core panics in Loop) and set the exported fields
// before calling Loop; they must not change afterwards.
type Core struct {
	PodUID    string
	Inventory device.Inventory
	Present   func() bool                // nil: the device is always present
	Blobs     func() []device.BlobStatus // nil: no blob reporting; called from the Loop goroutine
	PAPPLURL  func() string              // nil or "": a desired Queue is reported as MsgNoPAPPLURL
	Browse    Browser                    // nil: no DNS-SD browsing
	Interval  time.Duration              // wait between successful cycles; 0: 5 s

	minBackoff time.Duration
	maxBackoff time.Duration
	after      func(time.Duration) <-chan time.Time
	wake       chan struct{}
	desired    *device.DesiredState
	failing    map[string]bool
}

// NewCore returns a Core reporting inv as pod podUID every 5 s.
func NewCore(podUID string, inv device.Inventory) *Core {
	return &Core{
		PodUID: podUID, Inventory: inv,
		minBackoff: minBackoff, maxBackoff: maxBackoff,
		after: time.After, wake: make(chan struct{}, 1), failing: map[string]bool{},
	}
}

// Wake makes Loop run its next cycle now. It never blocks and is safe to call
// from any goroutine.
func (c *Core) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Loop runs cycles until ctx is done: it builds a report, sends it through
// report and stores the returned DesiredState. It waits the interval between
// cycles, backs off from 1 s to 30 s while report fails, and runs the next
// cycle at once after the DesiredState changed or on Wake. Loop is the only
// caller of report; a result that arrives after ctx is done is dropped.
func (c *Core) Loop(ctx context.Context, report device.ReportFunc) {
	var backoff time.Duration
	for ctx.Err() == nil {
		ds, err := report(ctx, c.cycle(ctx))
		if ctx.Err() != nil {
			return
		}
		c.problem("report", "control plane unreachable", err)
		wait := c.Interval
		if wait <= 0 {
			wait = defaultInterval
		}
		if err != nil {
			backoff = min(max(2*backoff, c.minBackoff), c.maxBackoff)
			wait = backoff
		} else {
			backoff = 0
			if c.desired == nil || !reflect.DeepEqual(*c.desired, ds) {
				wait = 0
			}
			c.desired = &ds
		}
		if wait == 0 {
			continue
		}
		select {
		case <-ctx.Done():
		case <-c.wake:
		case <-c.after(wait):
		}
	}
}

// cycle checks presence, re-reads blob statuses, browses DNS-SD and
// reconciles the desired PAPPL queue, in that order.
func (c *Core) cycle(ctx context.Context) device.AgentReport {
	r := device.AgentReport{
		PodUID: c.PodUID, Inventory: c.Inventory, DevicePresent: true,
		Services: []device.DNSSDService{}, Blobs: []device.BlobStatus{},
	}
	if r.Inventory.Devices == nil {
		r.Inventory.Devices = []device.USBDevice{}
	}
	if c.Present != nil {
		r.DevicePresent = c.Present()
	}
	if c.Blobs != nil {
		if b := c.Blobs(); b != nil {
			r.Blobs = b
		}
	}
	types := device.BrowseTypes
	if c.desired != nil {
		r.AppliedHash, types = c.desired.Hash, c.desired.Browse
	}
	if c.Browse != nil && len(types) > 0 {
		s, err := c.Browse(ctx, types)
		c.problem("browse", "DNS-SD browse failed", err)
		if err == nil && s != nil {
			r.Services = s
		}
	}
	if c.desired != nil && c.desired.Queue != nil {
		q := c.reconcile(ctx, *c.desired.Queue)
		r.Queue = &q
	}
	return r
}

func (c *Core) reconcile(ctx context.Context, q device.QueueSpec) device.QueueResult {
	url := ""
	if c.PAPPLURL != nil {
		url = c.PAPPLURL()
	}
	if url == "" {
		return device.QueueResult{State: device.QueueError, Message: MsgNoPAPPLURL}
	}
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	return pappl.Reconcile(ctx, pappl.New(url), q, c.Inventory)
}

// problem logs a recurring failure only when it starts and when it ends, so a
// daemon that stays down does not log every cycle.
func (c *Core) problem(key, msg string, err error) {
	switch {
	case err != nil && !c.failing[key]:
		slog.Warn(msg, "err", err)
		c.failing[key] = true
	case err == nil && c.failing[key]:
		slog.Info(msg + ": recovered")
		c.failing[key] = false
	}
}

// Config holds the `agent run` flags.
type Config struct {
	ControlPlaneURL string        // --control-plane-url
	TokenFile       string        // --token-file, re-read on every report
	PodUID          string        // --pod-uid
	InventoryFile   string        // --inventory; "": empty inventory, always present
	BlobStatusFile  string        // --blob-status, re-read every cycle; "": no blob reporting
	SysRoot         string        // --sys-root; "": /sys
	PAPPLURL        string        // --pappl-url
	DBusAddress     string        // --dbus-address; "": no DNS-SD browsing
	Interval        time.Duration // --interval; 0: 5 s
	Browse          Browser       // nil: Avahi over DBusAddress
	Client          *http.Client  // nil: a client with a 15 s timeout
}

// Run reports to the control plane until ctx is done. It fails only on bad
// flags or an unreadable inventory file; everything else is retried.
func Run(ctx context.Context, cfg Config) error {
	core, report, err := newRun(cfg)
	if err != nil {
		return err
	}
	core.Loop(ctx, report)
	return nil
}

func newRun(cfg Config) (*Core, device.ReportFunc, error) {
	if cfg.ControlPlaneURL == "" || cfg.TokenFile == "" {
		return nil, nil, errors.New("agent run needs --control-plane-url and --token-file")
	}
	inv := device.Inventory{Devices: []device.USBDevice{}}
	if cfg.InventoryFile != "" {
		b, err := os.ReadFile(cfg.InventoryFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read inventory: %w", err)
		}
		if err := json.Unmarshal(b, &inv); err != nil {
			return nil, nil, fmt.Errorf("read inventory %s: %w", cfg.InventoryFile, err)
		}
	}
	core := NewCore(cfg.PodUID, inv)
	core.Interval = cfg.Interval
	if cfg.InventoryFile != "" {
		sysRoot := cfg.SysRoot
		if sysRoot == "" {
			sysRoot = "/sys"
		}
		// The printer is the first printer-class device, as Decide and the PAPPL
		// reconcile pick it without a selector; with none, nothing can vanish.
		// shortcut: a device.usb selector that picks another printer is not
		// followed (the agent protocol carries no selector); send it in
		// DesiredState if multi-printer hosts need loss detection.
		if d := printerDevice(inv); d != nil {
			dev := *d
			core.Present = func() bool { return usb.Present(sysRoot, dev) }
		}
	}
	if cfg.BlobStatusFile != "" {
		core.Blobs = func() []device.BlobStatus {
			b, err := readBlobStatus(cfg.BlobStatusFile)
			core.problem("blob-status", "cannot read the blob status file "+cfg.BlobStatusFile, err)
			return b
		}
	}
	if cfg.PAPPLURL != "" {
		core.PAPPLURL = func() string { return cfg.PAPPLURL }
	}
	core.Browse = cfg.Browse
	if core.Browse == nil && cfg.DBusAddress != "" {
		core.Browse = avahiBrowser(cfg.DBusAddress)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return core, httpReport(cfg.ControlPlaneURL, cfg.TokenFile, client), nil
}

// readBlobStatus reads the fetch-blobs status file: missing means no blobs yet,
// unreadable or malformed is an error and an empty list.
func readBlobStatus(path string) ([]device.BlobStatus, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path from the --blob-status flag
	if errors.Is(err, fs.ErrNotExist) {
		return []device.BlobStatus{}, nil
	}
	if err != nil {
		return []device.BlobStatus{}, err
	}
	var st []device.BlobStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return []device.BlobStatus{}, err
	}
	if st == nil {
		st = []device.BlobStatus{}
	}
	return st, nil
}

func httpReport(baseURL, tokenFile string, client *http.Client) device.ReportFunc {
	url := strings.TrimSuffix(baseURL, "/") + device.AgentReportPath
	return func(ctx context.Context, r device.AgentReport) (device.DesiredState, error) {
		var ds device.DesiredState
		token, err := os.ReadFile(tokenFile) //nolint:gosec // path from the --token-file flag
		if err != nil {
			return ds, err
		}
		body, err := json.Marshal(r)
		if err != nil {
			return ds, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return ds, err
		}
		req.Header.Set("Authorization", device.TokenScheme+" "+strings.TrimSpace(string(token)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return ds, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			return ds, fmt.Errorf("control plane answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ds); err != nil {
			return ds, fmt.Errorf("decode desired state: %w", err)
		}
		return ds, nil
	}
}

// avahiBrowser connects to the bus lazily and reconnects after a failure. It
// is called only from the Loop goroutine.
func avahiBrowser(addr string) Browser {
	var conn *dbus.Conn
	return func(ctx context.Context, types []string) ([]device.DNSSDService, error) {
		if conn == nil {
			c, err := dbus.Connect(addr)
			if err != nil {
				return nil, err
			}
			conn = c
		}
		s, err := dnssd.Browse(ctx, conn, types, browseWait)
		if err != nil {
			_ = conn.Close()
			conn = nil
		}
		return s, err
	}
}
