// Package api serves the REST API (/api/v1), the SSE stream, the embedded SPA
// and, on a separate handler, the internal agent endpoint.
package api

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
	"github.com/ThePhaseless/printscan/internal/scanning"
)

// ConfigStore is the config file store (*store.Store).
type ConfigStore interface {
	Read() (raw []byte, etag string, err error)
	Create(raw []byte) (etag string, err error)
	Replace(etag string, raw []byte) (newEtag string, err error)
	Patch(etag string, mergePatch []byte) (newRaw []byte, newEtag string, err error)
}

// StatusSource is the aggregated status (*controller.Controller).
type StatusSource interface {
	Current() apitypes.Status
	Subscribe() (<-chan apitypes.Status, func())
}

// SetupSource is the wizard data (*controller.Controller).
type SetupSource interface {
	Setup() apitypes.Setup
}

// ErrNoPrinter is returned by a Printer while no printer endpoint is
// available (API: 503).
var ErrNoPrinter = errors.New("no printer available")

// Printer is the current printer. Its methods mirror *printing.Client; they
// return ErrNoPrinter while there is no printer.
type Printer interface {
	Submit(ctx context.Context, name, user, format string, r io.Reader, opts apitypes.PrintOptions) (int, error)
	Jobs(ctx context.Context, limit int) ([]apitypes.Job, error)
	Job(ctx context.Context, id int) (apitypes.Job, error)
	Cancel(ctx context.Context, id int) error
	// Formats is the printer's document-format-supported list.
	Formats(ctx context.Context) ([]string, error)
}

// Scans is the scan service (*scanning.Service) plus the current scanner's
// capabilities (scanning.ErrNoScanner while there is none).
type Scans interface {
	Start(set apitypes.ScanSettings) (apitypes.Scan, error)
	List() []apitypes.Scan
	Get(id string) (apitypes.Scan, error)
	Page(id string, n int) (*os.File, error)
	Download(id string) (scanning.Download, error)
	Delete(id string) error
	Caps(ctx context.Context) (apitypes.ScannerCaps, error)
}

// Agent is the internal agent endpoint's backend. With a nil Tokens every
// request gets 401 (the sim backend reports in-process).
type Agent struct {
	Report device.ReportFunc
	Tokens device.AgentTokenSource
}

// Deps is everything the API needs. Store, Check, Config, Status, Setup,
// Printer and Scans are required.
type Deps struct {
	Store ConfigStore
	// Check is the shared Parse plus Validate check.
	Check func([]byte) (*config.Config, error)
	// LastGood is the watcher's last-known-good config (nil: none).
	LastGood func() *config.Config
	// Config is the effective config (controller.Config); read on every request.
	Config  func() config.Config
	Status  StatusSource
	Setup   SetupSource
	Printer Printer
	Scans   Scans
	// ScanEvents is the scan service's event channel; the server drains it
	// into the scan SSE event until BaseContext is done.
	ScanEvents <-chan apitypes.Scan
	Agent      Agent
	// UI is the built SPA, rooted at the directory holding index.html.
	UI fs.FS
	// Hub fans SSE events out; serve publishes config events on it. nil: a new hub.
	Hub *Hub
	// Ready reports whether the config has been read once (/readyz). nil: always ready.
	Ready func() bool
	// BaseContext bounds job watchers and the event pumps. nil: Background.
	BaseContext     context.Context
	JobPollInterval time.Duration // default 1 s
	JobWatchTimeout time.Duration // default 30 min

	heartbeat time.Duration // SSE heartbeat, default 25 s; tests shorten it
}

type server struct {
	d   Deps
	ctx context.Context
	hub *Hub
}

type route struct {
	pattern string
	handler func(*server, http.ResponseWriter, *http.Request)
}

// publicRoutes is the public listener's route table.
var publicRoutes = []route{
	{"GET /api/v1/config", (*server).getConfig},
	{"GET /api/v1/config/raw", (*server).getConfigRaw},
	{"PUT /api/v1/config", (*server).putConfig},
	{"PATCH /api/v1/config", (*server).patchConfig},
	{"GET /api/v1/setup", (*server).getSetup},
	{"GET /api/v1/status", (*server).getStatus},
	{"GET /api/v1/events", (*server).events},
	{"POST /api/v1/print", (*server).print},
	{"GET /api/v1/jobs", (*server).getJobs},
	{"GET /api/v1/jobs/{id}", (*server).getJob},
	{"DELETE /api/v1/jobs/{id}", (*server).deleteJob},
	{"GET /api/v1/scanner", (*server).getScanner},
	{"POST /api/v1/scans", (*server).postScan},
	{"GET /api/v1/scans", (*server).getScans},
	{"GET /api/v1/scans/{id}", (*server).getScan},
	{"GET /api/v1/scans/{id}/pages/{n}", (*server).getScanPage},
	{"GET /api/v1/scans/{id}/download", (*server).getScanDownload},
	{"DELETE /api/v1/scans/{id}", (*server).deleteScan},
	{"/api/", (*server).apiNotFound},
	{"GET /healthz", (*server).healthz},
	{"GET /readyz", (*server).readyz},
	{"/", (*server).spa},
}

// Security headers on every public response.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' blob: data:; frame-ancestors 'none'"

// New returns the public handler (API, SSE, SPA) and the internal handler
// (agent endpoint). It starts the Status and scan event pumps on BaseContext.
func New(d Deps) (public, internal http.Handler) {
	if d.BaseContext == nil {
		d.BaseContext = context.Background()
	}
	if d.JobPollInterval <= 0 {
		d.JobPollInterval = time.Second
	}
	if d.JobWatchTimeout <= 0 {
		d.JobWatchTimeout = 30 * time.Minute
	}
	if d.heartbeat <= 0 {
		d.heartbeat = 25 * time.Second
	}
	if d.Hub == nil {
		d.Hub = NewHub()
	}
	if d.LastGood == nil {
		d.LastGood = func() *config.Config { return nil }
	}
	if d.Ready == nil {
		d.Ready = func() bool { return true }
	}
	s := &server{d: d, ctx: d.BaseContext, hub: d.Hub}
	s.startPumps()

	mux := http.NewServeMux()
	for _, rt := range publicRoutes {
		h := rt.handler
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	}
	cop := http.NewCrossOriginProtection()
	public = securityHeaders(cop.Handler(mux))

	imux := http.NewServeMux()
	imux.HandleFunc("POST "+device.AgentReportPath, s.agentReport)
	return public, imux
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// startPumps forwards Status changes and scan events to the hub until
// BaseContext is done, whether or not any client is connected.
func (s *server) startPumps() {
	ch, cancel := s.d.Status.Subscribe()
	go func() {
		defer cancel()
		for {
			select {
			case <-s.ctx.Done():
				return
			case st := <-ch:
				s.hub.Publish(apitypes.EventStatus, st)
			}
		}
	}()
	if s.d.ScanEvents != nil {
		go func() {
			for {
				select {
				case <-s.ctx.Done():
					return
				case sc := <-s.d.ScanEvents:
					s.hub.Publish(apitypes.EventScan, sc)
				}
			}
		}()
	}
}

// noWriteDeadline lifts the server's write timeout for a long response
// (SSE, downloads).
func noWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func (s *server) getSetup(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.d.Setup.Setup())
}

func (s *server) getStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.d.Status.Current())
}

func (s *server) apiNotFound(w http.ResponseWriter, _ *http.Request) {
	writeProblem(w, http.StatusNotFound, "not-found", "no such API route")
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = io.WriteString(w, "ok")
}

func (s *server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.d.Ready() {
		http.Error(w, "config not read yet", http.StatusServiceUnavailable)
		return
	}
	_, _ = io.WriteString(w, "ok")
}
