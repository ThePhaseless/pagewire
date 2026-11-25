// Package api serves the REST API (/api/v1), the SSE stream, the embedded SPA
// and, on a separate handler, the internal agent endpoint.
package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/scanning"
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
	// EULA returns the licence text of a catalog EULA, false for an unknown id.
	EULA(id string) (apitypes.EULA, bool)
	// Detect re-runs device detection (POST /setup/detect).
	Detect(ctx context.Context) error
}

var (
	// ErrNoPrinter is returned while a printer has no endpoint (API: 503).
	ErrNoPrinter = errors.New("no printer available")
	// ErrUnknownPrinter is a printer id that is not configured or is
	// disabled (API: 404).
	ErrUnknownPrinter = errors.New("unknown printer")
	// ErrUnknownScanner is a scanner id that is not configured or is
	// disabled (API: 404).
	ErrUnknownScanner = errors.New("unknown scanner")
)

// Printers looks printers up by entry id: ErrUnknownPrinter for an unknown or
// disabled id, ErrNoPrinter while it has no endpoint.
type Printers interface {
	Printer(id string) (Printer, error)
}

// Printer is one printer. Its methods mirror *printing.Client; they return
// ErrNoPrinter while the printer has no endpoint.
type Printer interface {
	Submit(ctx context.Context, name, user, format string, r io.Reader, opts apitypes.PrintOptions) (int, error)
	Jobs(ctx context.Context, limit int) ([]apitypes.Job, error)
	Job(ctx context.Context, id int) (apitypes.Job, error)
	Cancel(ctx context.Context, id int) error
	// Formats is the printer's document-format-supported list.
	Formats(ctx context.Context) ([]string, error)
}

// Scans is the scan service (*scanning.Service) plus each scanner's
// capabilities: ErrUnknownScanner for an unknown or disabled id,
// scanning.ErrNoScanner while it has no endpoint.
type Scans interface {
	Start(set apitypes.ScanSettings) (apitypes.Scan, error)
	List() []apitypes.Scan
	Get(id string) (apitypes.Scan, error)
	Page(id string, n int) (*os.File, error)
	Download(id string) (scanning.Download, error)
	Delete(id string) error
	// Cancel stops a running scan (scanning.ErrNotRunning once it has ended).
	Cancel(id string) (apitypes.Scan, error)
	Caps(ctx context.Context, id string) (apitypes.ScannerCaps, error)
}

// Agent is the internal agent endpoint's backend. With a nil Tokens every
// request gets 401 (the sim backend reports in-process).
type Agent struct {
	Report device.ReportFunc
	Tokens device.AgentTokenSource
}

// Deps is everything the API needs. Store, Check, Config, Status, Setup,
// Printers and Scans are required.
type Deps struct {
	Store ConfigStore
	// Check is the shared Parse plus Validate check.
	Check func([]byte) (*config.Config, error)
	// LastGood is the watcher's last-known-good config (nil: none).
	LastGood func() *config.Config
	// Config is the effective config (controller.Config); read on every request.
	Config   func() config.Config
	Status   StatusSource
	Setup    SetupSource
	Printers Printers
	// PrinterCaps is a printer's capabilities, false while they are not known
	// (print options are then sent unfitted). nil: never known.
	PrinterCaps func(id string) (device.PrinterCaps, bool)
	Scans       Scans
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
	// AllowedHosts are the Host names (port ignored, any case; IPv6 in
	// brackets) the public routes answer, others get 421. Empty: no check.
	// /healthz and /readyz are exempt.
	AllowedHosts []string
	// ProxySecret, when set, must arrive in ProxySecretHeader (default
	// X-Pagewire-Proxy-Secret) on every /api/ request, else 401.
	ProxySecret       string
	ProxySecretHeader string

	heartbeat time.Duration // SSE heartbeat, default 25 s; tests shorten it
	// Request body read deadlines; tests shorten them.
	readTimeout, printReadTimeout time.Duration
}

// DefaultProxySecretHeader carries the proxy secret unless Deps names another.
const DefaultProxySecretHeader = "X-Pagewire-Proxy-Secret" //nolint:gosec // G101 false positive: a header name

const (
	defaultReadTimeout      = 30 * time.Second // config and scan bodies
	defaultPrintReadTimeout = 10 * time.Minute // a print upload
	// maxJobWatchers caps the goroutines that follow web-submitted jobs.
	maxJobWatchers = 32
)

type server struct {
	d        Deps
	ctx      context.Context
	hub      *Hub
	watchers chan struct{} // one slot per running job watcher
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
	{"POST /api/v1/setup/detect", (*server).postDetect},
	{"GET /api/v1/eulas/{id}", (*server).getEULA},
	{"GET /api/v1/status", (*server).getStatus},
	{"GET /api/v1/events", (*server).events},
	{"POST /api/v1/printers/{id}/print", (*server).print},
	{"GET /api/v1/printers/{id}/jobs", (*server).getJobs},
	{"GET /api/v1/printers/{id}/jobs/{job}", (*server).getJob},
	{"DELETE /api/v1/printers/{id}/jobs/{job}", (*server).deleteJob},
	{"GET /api/v1/scanners/{id}/capabilities", (*server).getScannerCaps},
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
const contentSecurityPolicy = "default-src 'self'; img-src 'self' blob: data:; frame-ancestors 'none'; " +
	"base-uri 'self'; form-action 'self'; object-src 'none'"

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
	if d.PrinterCaps == nil {
		d.PrinterCaps = func(string) (device.PrinterCaps, bool) { return device.PrinterCaps{}, false }
	}
	if d.LastGood == nil {
		d.LastGood = func() *config.Config { return nil }
	}
	if d.Ready == nil {
		d.Ready = func() bool { return true }
	}
	if d.ProxySecretHeader == "" {
		d.ProxySecretHeader = DefaultProxySecretHeader
	}
	if d.readTimeout <= 0 {
		d.readTimeout = defaultReadTimeout
	}
	if d.printReadTimeout <= 0 {
		d.printReadTimeout = defaultPrintReadTimeout
	}
	s := &server{d: d, ctx: d.BaseContext, hub: d.Hub, watchers: make(chan struct{}, maxJobWatchers)}
	s.startPumps()

	mux := http.NewServeMux()
	for _, rt := range publicRoutes {
		h := rt.handler
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { h(s, w, r) })
	}
	cop := http.NewCrossOriginProtection()
	public = securityHeaders(allowedHosts(d.AllowedHosts, proxySecret(d.ProxySecret, d.ProxySecretHeader, cop.Handler(mux))))

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
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// allowedHosts answers 421 to a request whose Host is not in hosts, so a
// DNS-rebinding page cannot reach the API. No hosts: no check.
func allowedHosts(hosts []string, next http.Handler) http.Handler {
	if len(hosts) == 0 {
		return next
	}
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[hostName(strings.TrimSpace(h))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[hostName(r.Host)] && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			writeProblem(w, http.StatusMisdirectedRequest, "misdirected-request",
				"this host name is not in pagewire's allowed hosts (PAGEWIRE_ALLOWED_HOSTS)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostName is host without its port and IPv6 brackets, lower-cased.
func hostName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// proxySecret answers 401 to an /api/ request without the shared secret the
// auth proxy adds, so the API fails closed when the proxy is bypassed.
func proxySecret(secret, header string, next http.Handler) http.Handler {
	if secret == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get(header)), []byte(secret)) != 1 {
			writeProblem(w, http.StatusUnauthorized, "unauthorized", "missing or wrong proxy secret")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readDeadline bounds how long a handler waits for the request body.
func readDeadline(w http.ResponseWriter, d time.Duration) {
	// Not every ResponseWriter supports it (httptest's recorder does not).
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
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

// postDetect re-runs device detection; the result arrives in Status.
func (s *server) postDetect(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Setup.Detect(r.Context()); err != nil {
		slog.Warn("detect devices", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "detect-failed", "device detection could not be restarted; the server log has the details")
		return
	}
	w.WriteHeader(http.StatusAccepted)
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
