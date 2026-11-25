package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ThePhaseless/printscan/internal/api"
	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/backend/k8s"
	"github.com/ThePhaseless/printscan/internal/backend/sim"
	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/config/store"
	"github.com/ThePhaseless/printscan/internal/config/watch"
	"github.com/ThePhaseless/printscan/internal/controller"
	"github.com/ThePhaseless/printscan/internal/device"
	"github.com/ThePhaseless/printscan/internal/printing"
	"github.com/ThePhaseless/printscan/internal/scanning"
	"github.com/ThePhaseless/printscan/ui"
)

const (
	saNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	noUSBResource   = "device.mode usb needs a USB resource (set --usb-resource)"
	lastGoodName    = "printscan.last-good.yaml"
	// The print handler streams the upload to the printer inside the request,
	// and only SSE and downloads lift the write deadline, so it must cover a
	// full upload (up to 500 MiB) plus the IPP submit on a slow link.
	publicWriteTimeout = 10 * time.Minute
	shutdownTimeout    = 10 * time.Second
)

type serveFlags struct {
	listen, internalListen, configPath, dataDir string
	backend, simDevice, esclClient, logLevel    string
	namespace, owner, usbResource, blobPVC      string
	agentURL, registry, tag, uiDir              string
	allowedHosts, proxySecretHeader             string
	proxySecretFile, simScanDelay               string
	proxySecret                                 string // from PRINTSCAN_PROXY_SECRET or the file; never a flag
	internalListenSet                           bool
	scanDelay                                   time.Duration // parsed --sim-scan-delay
}

// simAllowedHosts is the sim backend's default Host allowlist.
var simAllowedHosts = []string{"localhost", "127.0.0.1", "[::1]"}

func parseServeFlags(args []string) (serveFlags, error) {
	var f serveFlags
	fl := serveFlagSet(&f, os.Stderr)
	if err := fl.Parse(args); err != nil {
		return f, err
	}
	if fl.NArg() > 0 {
		return f, fmt.Errorf("serve takes no arguments, got %q", fl.Args())
	}
	if f.backend != controller.BackendK8s && f.backend != controller.BackendSim {
		return f, fmt.Errorf("--backend must be k8s or sim, got %q", f.backend)
	}
	if f.esclClient != "mfp" && f.esclClient != "xml" {
		return f, fmt.Errorf("--escl-client must be mfp or xml, got %q", f.esclClient)
	}
	if f.backend == controller.BackendSim && f.simDevice != "none" && len(catalog.Fixture(f.simDevice).Devices) == 0 {
		return f, fmt.Errorf("--sim-device: unknown fixture %q", f.simDevice)
	}
	if f.backend == controller.BackendSim {
		d, err := time.ParseDuration(f.simScanDelay)
		if err != nil || d < 0 {
			return f, fmt.Errorf("--sim-scan-delay must be a duration such as 30s, got %q", f.simScanDelay)
		}
		f.scanDelay = d
	}
	fl.Visit(func(fg *flag.Flag) { f.internalListenSet = f.internalListenSet || fg.Name == "internal-listen" })
	f.internalListenSet = f.internalListenSet || os.Getenv("PRINTSCAN_INTERNAL_LISTEN") != ""
	secret, err := readProxySecret(os.Getenv("PRINTSCAN_PROXY_SECRET"), f.proxySecretFile)
	if err != nil {
		return f, err
	}
	f.proxySecret = secret
	return f, setLogLevel(f.logLevel)
}

// readProxySecret takes the proxy secret from the environment or a file,
// never both. An empty file is an error; an unset secret is "".
func readProxySecret(fromEnv, file string) (string, error) {
	if file == "" {
		return fromEnv, nil
	}
	if fromEnv != "" {
		return "", errors.New("set PRINTSCAN_PROXY_SECRET or --proxy-secret-file, not both")
	}
	b, err := os.ReadFile(file) //nolint:gosec // the operator names the secret file
	if err != nil {
		return "", fmt.Errorf("--proxy-secret-file: %w", err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("--proxy-secret-file %s is empty", file)
	}
	return secret, nil
}

// allowedHostsFor is the Host allowlist for --allowed-hosts: the backend's
// default when empty (sim: loopback names; k8s: no check), none for "*",
// else the comma-separated names.
func allowedHostsFor(backend, value string) []string {
	if strings.TrimSpace(value) == "*" {
		return nil
	}
	var hosts []string
	for h := range strings.SplitSeq(value, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 && backend == controller.BackendSim {
		return simAllowedHosts
	}
	return hosts
}

// serveFlagSet defines serve's flags into f, with usage written to out.
func serveFlagSet(f *serveFlags, out io.Writer) *flag.FlagSet {
	fl := flag.NewFlagSet("serve", flag.ContinueOnError)
	fl.SetOutput(out)
	defBackend := controller.BackendSim
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		defBackend = controller.BackendK8s
	}
	fl.StringVar(&f.listen, "listen", env("PRINTSCAN_LISTEN", ":8080"), "public listen address (env PRINTSCAN_LISTEN)")
	fl.StringVar(&f.internalListen, "internal-listen", env("PRINTSCAN_INTERNAL_LISTEN", ":8081"), "agent listen address (env PRINTSCAN_INTERNAL_LISTEN)")
	fl.StringVar(&f.configPath, "config", env("PRINTSCAN_CONFIG", "/data/printscan.yaml"), "config file (env PRINTSCAN_CONFIG)")
	fl.StringVar(&f.dataDir, "data-dir", env("PRINTSCAN_DATA_DIR", "/data"), "data directory (env PRINTSCAN_DATA_DIR)")
	fl.StringVar(&f.backend, "backend", env("PRINTSCAN_BACKEND", defBackend), "k8s or sim (env PRINTSCAN_BACKEND)")
	fl.StringVar(&f.simDevice, "sim-device", env("PRINTSCAN_SIM_DEVICE", "ippusb-mfp"), "sim USB fixture, or none (env PRINTSCAN_SIM_DEVICE)")
	// hidden test seam (e2e scan cancel): sim backend only, holds each scanned page this long
	fl.StringVar(&f.simScanDelay, "sim-scan-delay", env("PRINTSCAN_SIM_SCAN_DELAY", "0s"), "")
	fl.StringVar(&f.esclClient, "escl-client", env("PRINTSCAN_ESCL_CLIENT", "mfp"), "mfp or xml (env PRINTSCAN_ESCL_CLIENT)")
	fl.StringVar(&f.logLevel, "log-level", env("PRINTSCAN_LOG_LEVEL", "info"), "debug, info, warn or error (env PRINTSCAN_LOG_LEVEL)")
	fl.StringVar(&f.namespace, "k8s-namespace", env("PRINTSCAN_K8S_NAMESPACE", ""), "namespace (env PRINTSCAN_K8S_NAMESPACE; default: the service account's)")
	fl.StringVar(&f.owner, "k8s-owner", env("PRINTSCAN_K8S_OWNER", "printscan"), "owner Deployment (env PRINTSCAN_K8S_OWNER)")
	fl.StringVar(&f.usbResource, "usb-resource", env("PRINTSCAN_USB_RESOURCE", ""), "USB extended resource, e.g. devic.es/printer (env PRINTSCAN_USB_RESOURCE)")
	fl.StringVar(&f.blobPVC, "blob-pvc", env("PRINTSCAN_BLOB_PVC", "printscan-blobs"), "blob PVC (env PRINTSCAN_BLOB_PVC)")
	fl.StringVar(&f.agentURL, "agent-url", env("PRINTSCAN_AGENT_URL", ""), "control-plane URL for the agent (env PRINTSCAN_AGENT_URL; default http://printscan.<ns>.svc:8081)")
	fl.StringVar(&f.registry, "image-registry", env("PRINTSCAN_IMAGE_REGISTRY", "ghcr.io/thephaseless"), "image registry (env PRINTSCAN_IMAGE_REGISTRY)")
	fl.StringVar(&f.tag, "image-tag", env("PRINTSCAN_IMAGE_TAG", k8s.ImageTag(version)), "device and driver image tag (env PRINTSCAN_IMAGE_TAG)")
	fl.StringVar(&f.allowedHosts, "allowed-hosts", env("PRINTSCAN_ALLOWED_HOSTS", ""),
		"comma-separated host names the UI and API answer; others get 421. * turns the check off (env PRINTSCAN_ALLOWED_HOSTS; default: localhost,127.0.0.1,[::1] with the sim backend, no check with k8s)")
	fl.StringVar(&f.proxySecretHeader, "proxy-secret-header", env("PRINTSCAN_PROXY_SECRET_HEADER", api.DefaultProxySecretHeader),
		"header that must carry the proxy secret on /api/ requests (env PRINTSCAN_PROXY_SECRET_HEADER)")
	fl.StringVar(&f.proxySecretFile, "proxy-secret-file", env("PRINTSCAN_PROXY_SECRET_FILE", ""),
		"file holding the proxy secret; or set env PRINTSCAN_PROXY_SECRET, not both (env PRINTSCAN_PROXY_SECRET_FILE)")
	fl.StringVar(&f.uiDir, "ui-dir", "", "") // hidden: serve a directory instead of the embedded UI (bun run dev)
	fl.Usage = func() {
		_, _ = fmt.Fprintln(fl.Output(), "usage: printscan serve [flags]")
		fl.VisitAll(func(fg *flag.Flag) {
			switch {
			case fg.Name == "ui-dir", fg.Name == "sim-scan-delay":
			case fg.DefValue == "":
				_, _ = fmt.Fprintf(fl.Output(), "  --%s\t%s\n", fg.Name, fg.Usage)
			default:
				_, _ = fmt.Fprintf(fl.Output(), "  --%s\t%s (default %q)\n", fg.Name, fg.Usage, fg.DefValue)
			}
		})
		_, _ = fmt.Fprintln(fl.Output(), "environment only:\n"+
			"  PRINTSCAN_PROXY_SECRET\tproxy secret (see --proxy-secret-file)\n"+
			"  PRINTSCAN_IMAGE_OVERRIDES\tper-image references, e.g. device=<ref>,driver-ps=<ref>; keys: printscan, device, driver-ps, driver-hplip, driver-gutenprint, driver-ghostscript")
	}
	return fl
}

// serve runs the control plane until ctx is done. onListen, when set, gets the
// bound public and internal addresses once the listeners are up; internal is
// nil with the sim backend, which opens no internal listener.
func serve(ctx context.Context, args []string, onListen func(public, internal net.Addr)) error {
	f, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	// Everything below stops when serve returns, also on a listen error.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Checked before anything starts, whatever the backend.
	overrides, err := k8s.ParseOverrides(os.Getenv("PRINTSCAN_IMAGE_OVERRIDES"))
	if err != nil {
		return fmt.Errorf("PRINTSCAN_IMAGE_OVERRIDES: %w", err)
	}
	uiFS, err := uiFiles(f.uiDir)
	if err != nil {
		return err
	}

	known := catalog.Known()
	check := func(b []byte) (*config.Config, error) { return config.Check(b, known) }
	newScanner := scanning.NewMFP
	if f.esclClient == "xml" {
		newScanner = scanning.NewXML
	}

	var backend device.Backend
	var simBackend *sim.Backend
	usbAvailable, usbUnavailable := f.usbResource != "", ""
	switch f.backend {
	case controller.BackendK8s:
		if backend, err = newK8sBackend(f, overrides); err != nil {
			return err
		}
		if !usbAvailable {
			usbUnavailable = noUSBResource
		}
	default:
		simBackend = sim.New(sim.Options{Fixture: f.simDevice, DataDir: f.dataDir, ScanDelay: f.scanDelay})
		backend = simBackend
		usbAvailable = f.simDevice != "none"
	}

	ctrl := controller.New(controller.Options{
		Backend: backend, BackendName: f.backend,
		USBAvailable: usbAvailable, USBUnavailable: usbUnavailable,
		Scanner: func(url string) controller.ScannerObserver {
			s, err := newScanner(url)
			if err != nil {
				return failedScanner{err}
			}
			return s
		},
	})
	if simBackend != nil {
		simBackend.SetReporter(ctrl.AgentReport)
		// The sim devices live in this process; stop them on the way out. Never
		// on k8s, where Apply(nil) deletes the device Deployment.
		defer func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			if err := simBackend.Apply(sctx, nil); err != nil {
				slog.Warn("stop simulated devices", "err", err)
			}
		}()
	}

	checkDataDir(f.dataDir, ctrl)

	scanEvents := make(chan apitypes.Scan, 16)
	scans := scanning.New(scanning.Options{
		Dir: filepath.Join(f.dataDir, "scans"),
		Scanner: func() scanning.Scanner {
			url := ctrl.Current().Scanner.URL
			if url == "" {
				return nil
			}
			s, err := newScanner(url)
			if err != nil {
				return nil
			}
			return s
		},
		Retention: func() time.Duration {
			return time.Duration(ctrl.Config().Scan.RetentionDays) * 24 * time.Hour
		},
		Events:  scanEvents,
		Context: ctx,
	})

	hub := api.NewHub()
	var ready, readFailed atomic.Bool
	seen := func(etag string) {
		ready.Store(true)
		if readFailed.Swap(false) {
			ctrl.SetReconcileItem("config", true, "config file read")
		}
		hub.Publish(apitypes.EventConfig, apitypes.ConfigEvent{Etag: etag})
	}
	w := &watch.Watcher[config.Config]{
		Dir:  filepath.Dir(f.configPath),
		Name: filepath.Base(f.configPath),
		Parse: func(b []byte) (config.Config, error) {
			c, err := check(b)
			if err != nil {
				return config.Config{}, err
			}
			return *c, nil
		},
		OnApply: func(c config.Config, etag string) {
			ctrl.ConfigApplied(&c, etag)
			seen(etag)
		},
		OnInvalid: func(err error, etag string, lastGood *config.Config) {
			ctrl.ConfigInvalid(err, etag, lastGood)
			seen(etag)
		},
		OnMissing: func() {
			ctrl.ConfigMissing()
			seen("")
		},
		LastGoodPath: filepath.Join(f.dataDir, lastGoodName),
	}

	var tokens device.AgentTokenSource
	if ts, ok := backend.(device.AgentTokenSource); ok {
		tokens = ts
	}
	public, internal := api.New(api.Deps{
		Store:       store.New(f.configPath, func(b []byte) error { _, err := check(b); return err }),
		Check:       check,
		LastGood:    w.LastGood,
		Config:      ctrl.Config,
		Status:      ctrl,
		Setup:       ctrl,
		Printer:     printerAdapter{ctrl},
		Scans:       scanAdapter{Service: scans, scanner: func() string { return ctrl.Current().Scanner.URL }, newScanner: newScanner},
		ScanEvents:  scanEvents,
		Agent:       api.Agent{Report: ctrl.AgentReport, Tokens: tokens},
		UI:          uiFS,
		Hub:         hub,
		Ready:       ready.Load,
		BaseContext: ctx,

		AllowedHosts:      allowedHostsFor(f.backend, f.allowedHosts),
		ProxySecret:       f.proxySecret,
		ProxySecretHeader: f.proxySecretHeader,
	})

	// The first Status a client sees already reflects the file.
	if err := w.Start(ctx); err != nil {
		// The watcher already logged a watch failure; seen() never clears this item.
		if errors.Is(err, watch.ErrWatchUnavailable) {
			ctrl.SetReconcileItem("config-watch", false, fmt.Sprintf("cannot watch %s; config changes apply within 30 s", filepath.Dir(f.configPath)))
		}
		// No callback ran, so the first read itself failed.
		if !ready.Load() {
			slog.Error("first config read failed; the controller waits for the first read", "path", f.configPath, "err", err)
			readFailed.Store(true)
			ctrl.SetReconcileItem("config", false, err.Error())
		}
	}
	var wg sync.WaitGroup
	wg.Go(func() { ctrl.Run(ctx) })
	wg.Go(func() { scans.RunJanitor(ctx) })
	defer func() { cancel(); wg.Wait() }()

	var lc net.ListenConfig
	pubLn, err := lc.Listen(ctx, "tcp", f.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", f.listen, err)
	}
	base := func(net.Listener) context.Context { return ctx }
	servers := []*http.Server{
		{Handler: public, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: publicWriteTimeout, IdleTimeout: 2 * time.Minute, BaseContext: base},
	}
	listeners := []net.Listener{pubLn}
	var intAddr net.Addr
	intDesc := "none (sim)"
	if simBackend == nil {
		intLn, err := lc.Listen(ctx, "tcp", f.internalListen)
		if err != nil {
			_ = pubLn.Close()
			return fmt.Errorf("listen %s: %w", f.internalListen, err)
		}
		intAddr = intLn.Addr()
		intDesc = intAddr.String()
		listeners = append(listeners, intLn)
		servers = append(servers, &http.Server{Handler: internal, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, BaseContext: base})
	} else if f.internalListenSet {
		// The sim agent reports in-process; the flag stays accepted for scripts that pass it.
		slog.Info("--internal-listen is ignored with the sim backend: its agent reports in-process", "internal-listen", f.internalListen)
	}
	errc := make(chan error, len(servers))
	for i, ln := range listeners {
		go func() { errc <- servers[i].Serve(ln) }()
	}
	slog.Info("printscan serving", "version", version, "listen", pubLn.Addr().String(), "internal", intDesc, "backend", f.backend)
	if onListen != nil {
		onListen(pubLn.Addr(), intAddr)
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(sctx); err != nil {
			slog.Warn("shutdown", "err", err)
		}
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// checkDataDir creates <data-dir>/scans and proves it writable; a failure is
// logged and shown as the data-dir reconcile item, and serving continues.
func checkDataDir(dir string, ctrl *controller.Controller) {
	scans := filepath.Join(dir, "scans")
	err := os.MkdirAll(scans, 0o750)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(scans, ".write-test-*"); err == nil {
			_ = f.Close()
			err = os.Remove(f.Name())
		}
	}
	if err != nil {
		slog.Error("data directory is not writable", "dir", dir, "err", err)
		ctrl.SetReconcileItem("data-dir", false, fmt.Sprintf("data directory %s is not writable: %v", dir, err))
	}
}

func uiFiles(dir string) (fs.FS, error) {
	if dir != "" {
		return os.DirFS(dir), nil
	}
	return fs.Sub(ui.Dist, "dist")
}

func newK8sBackend(f serveFlags, overrides map[string]string) (*k8s.Backend, error) {
	ns := f.namespace
	if ns == "" {
		b, err := os.ReadFile(saNamespaceFile)
		if err != nil {
			return nil, fmt.Errorf("--k8s-namespace not set and no service-account namespace: %w", err)
		}
		ns = strings.TrimSpace(string(b))
	}
	agentURL := f.agentURL
	if agentURL == "" {
		agentURL = "http://printscan." + ns + ".svc:8081"
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return k8s.New(client, k8s.Params{
		Namespace: ns, Owner: k8s.Owner{Name: f.owner},
		Registry: f.registry, Tag: f.tag, Overrides: overrides,
		AgentURL: agentURL, USBResource: f.usbResource, BlobPVC: f.blobPVC,
	}), nil
}

// failedScanner reports a scanner URL that could not be turned into a client.
type failedScanner struct{ err error }

func (s failedScanner) Status(context.Context) (scanning.State, error) {
	return scanning.StateUnknown, s.err
}

// printerAdapter is the API's printer: the controller's current printer URI.
type printerAdapter struct{ ctrl *controller.Controller }

func (p printerAdapter) client() (*printing.Client, error) {
	uri := p.ctrl.Current().Printer.URI
	if uri == "" {
		return nil, api.ErrNoPrinter
	}
	return printing.New(uri), nil
}

func (p printerAdapter) Submit(ctx context.Context, name, user, format string, r io.Reader, opts apitypes.PrintOptions) (int, error) {
	c, err := p.client()
	if err != nil {
		return 0, err
	}
	return c.Submit(ctx, name, user, format, r, opts)
}

func (p printerAdapter) Jobs(ctx context.Context, limit int) ([]apitypes.Job, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	return c.Jobs(ctx, limit)
}

func (p printerAdapter) Job(ctx context.Context, id int) (apitypes.Job, error) {
	c, err := p.client()
	if err != nil {
		return apitypes.Job{}, err
	}
	return c.Job(ctx, id)
}

func (p printerAdapter) Cancel(ctx context.Context, id int) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	return c.Cancel(ctx, id)
}

func (p printerAdapter) Formats(ctx context.Context) ([]string, error) {
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	a, err := c.Attributes(ctx)
	return a.Formats, err
}

// scanAdapter is the scan service plus the current scanner's capabilities.
type scanAdapter struct {
	*scanning.Service
	scanner    func() string
	newScanner func(string) (scanning.Scanner, error)
}

func (s scanAdapter) Caps(ctx context.Context) (apitypes.ScannerCaps, error) {
	url := s.scanner()
	if url == "" {
		return apitypes.ScannerCaps{}, scanning.ErrNoScanner
	}
	sc, err := s.newScanner(url)
	if err != nil {
		return apitypes.ScannerCaps{}, err
	}
	return sc.Capabilities(ctx)
}
