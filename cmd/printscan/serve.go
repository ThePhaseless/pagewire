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
	"net/netip"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ThePhaseless/printscan/internal/api"
	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/backend/docker"
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
	dockerHost, dockerContainer, hostIP         string
	proxySecret                                 string // from PRINTSCAN_PROXY_SECRET or the file; never a flag
	internalListenSet                           bool
	scanDelay                                   time.Duration // parsed --sim-scan-delay
}

// simAllowedHosts is the sim backend's default Host allowlist.
var simAllowedHosts = []string{"localhost", "127.0.0.1", "[::1]"}

// The Engine socket and the files that mark a container; vars so tests can
// point them at temp files.
var (
	dockerSocket     = "/var/run/docker.sock"
	containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}
)

// defaultBackend picks --backend's default: k8s inside Kubernetes; docker in a
// container with DOCKER_HOST set or the Engine socket mounted; else sim. A host
// shell's DOCKER_HOST (rootless Docker, colima) alone does not pick docker.
func defaultBackend() string {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return controller.BackendK8s
	}
	inContainer := slices.ContainsFunc(containerMarkers, func(m string) bool { _, err := os.Stat(m); return err == nil })
	if inContainer {
		if _, err := os.Stat(dockerSocket); err == nil || os.Getenv("DOCKER_HOST") != "" {
			return controller.BackendDocker
		}
	}
	return controller.BackendSim
}

// dockerAgentURL is the agent's control-plane URL on the docker backend: the
// explicit --agent-url, else http://<internal listen host>:<port>, with
// 127.0.0.1 for an empty or unspecified host (the agent shares the host network).
func dockerAgentURL(explicit, internalListen string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	host, port, err := net.SplitHostPort(internalListen)
	if err != nil || port == "" {
		return "", fmt.Errorf("--internal-listen %q: want host:port to derive --agent-url", internalListen)
	}
	if a, err := netip.ParseAddr(host); host == "" || (err == nil && a.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

func parseServeFlags(args []string) (serveFlags, error) {
	var f serveFlags
	fl := serveFlagSet(&f, os.Stderr)
	if err := fl.Parse(args); err != nil {
		return f, err
	}
	if fl.NArg() > 0 {
		return f, fmt.Errorf("serve takes no arguments, got %q", fl.Args())
	}
	if f.backend != controller.BackendK8s && f.backend != controller.BackendDocker && f.backend != controller.BackendSim {
		return f, fmt.Errorf("--backend must be k8s, docker or sim, got %q", f.backend)
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

// cgroupMemoryMax holds the container's cgroup v2 memory limit.
const cgroupMemoryMax = "/sys/fs/cgroup/memory.max"

// setMemoryLimit sets the Go soft memory limit to 80% of the cgroup memory
// limit in path, unless GOMEMLIMIT is set. The 20% left over covers memory the
// Go runtime does not count but the cgroup does (kernel socket
// buffers, page tables, mapped binary pages), so the GC works harder before
// the OOM killer acts.
func setMemoryLimit(path string) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	limit := cgroupMemoryLimit(path)
	if limit <= 0 {
		slog.Debug("no cgroup v2 memory limit found; Go memory limit not set", "path", path)
		return
	}
	debug.SetMemoryLimit(limit / 5 * 4)
	slog.Debug("memory limit from cgroup", "cgroupBytes", limit, "goMemLimitBytes", limit/5*4)
}

// cgroupMemoryLimit returns the limit in a cgroup v2 memory.max file in bytes,
// or 0 when the file is missing or says "max" (no limit).
// shortcut: cgroup v2 only; on a cgroup v1 host set GOMEMLIMIT by hand.
func cgroupMemoryLimit(path string) int64 {
	b, err := os.ReadFile(path) //nolint:gosec // the cgroup path, or a temp file in tests
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
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
// default when empty (sim: loopback names; docker: loopback names plus hostIP;
// k8s: no check), none for "*", else the comma-separated names.
func allowedHostsFor(backend, value, hostIP string) []string {
	if strings.TrimSpace(value) == "*" {
		return nil
	}
	var hosts []string
	for h := range strings.SplitSeq(value, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) > 0 {
		return hosts
	}
	switch backend {
	case controller.BackendSim:
		return simAllowedHosts
	case controller.BackendDocker:
		if hostIP == "" || slices.Contains(simAllowedHosts, hostIP) {
			return simAllowedHosts
		}
		return append(slices.Clone(simAllowedHosts), hostIP)
	}
	return nil
}

// serveFlagSet defines serve's flags into f, with usage written to out.
func serveFlagSet(f *serveFlags, out io.Writer) *flag.FlagSet {
	fl := flag.NewFlagSet("serve", flag.ContinueOnError)
	fl.SetOutput(out)
	fl.StringVar(&f.listen, "listen", env("PRINTSCAN_LISTEN", ":8080"), "public listen address (env PRINTSCAN_LISTEN)")
	fl.StringVar(&f.internalListen, "internal-listen", env("PRINTSCAN_INTERNAL_LISTEN", ":8081"), "agent listen address (env PRINTSCAN_INTERNAL_LISTEN)")
	fl.StringVar(&f.configPath, "config", env("PRINTSCAN_CONFIG", "/data/printscan.yaml"), "config file (env PRINTSCAN_CONFIG)")
	fl.StringVar(&f.dataDir, "data-dir", env("PRINTSCAN_DATA_DIR", "/data"), "data directory (env PRINTSCAN_DATA_DIR)")
	fl.StringVar(&f.backend, "backend", env("PRINTSCAN_BACKEND", defaultBackend()), "k8s, docker or sim (env PRINTSCAN_BACKEND)")
	fl.StringVar(&f.simDevice, "sim-device", env("PRINTSCAN_SIM_DEVICE", "ippusb-mfp"), "sim USB fixture, or none (env PRINTSCAN_SIM_DEVICE)")
	// hidden test seam (e2e scan cancel): sim backend only, holds each scanned page this long
	fl.StringVar(&f.simScanDelay, "sim-scan-delay", env("PRINTSCAN_SIM_SCAN_DELAY", "0s"), "")
	fl.StringVar(&f.esclClient, "escl-client", env("PRINTSCAN_ESCL_CLIENT", "mfp"), "mfp or xml (env PRINTSCAN_ESCL_CLIENT)")
	fl.StringVar(&f.logLevel, "log-level", env("PRINTSCAN_LOG_LEVEL", "info"), "debug, info, warn or error (env PRINTSCAN_LOG_LEVEL)")
	fl.StringVar(&f.namespace, "k8s-namespace", env("PRINTSCAN_K8S_NAMESPACE", ""), "namespace (env PRINTSCAN_K8S_NAMESPACE; default: the service account's)")
	fl.StringVar(&f.owner, "k8s-owner", env("PRINTSCAN_K8S_OWNER", "printscan"), "owner Deployment (env PRINTSCAN_K8S_OWNER)")
	fl.StringVar(&f.usbResource, "usb-resource", env("PRINTSCAN_USB_RESOURCE", ""), "USB extended resource, e.g. devic.es/printer (env PRINTSCAN_USB_RESOURCE)")
	fl.StringVar(&f.blobPVC, "blob-pvc", env("PRINTSCAN_BLOB_PVC", "printscan-blobs"), "blob PVC (env PRINTSCAN_BLOB_PVC)")
	fl.StringVar(&f.agentURL, "agent-url", env("PRINTSCAN_AGENT_URL", ""), "control-plane URL for the agent (env PRINTSCAN_AGENT_URL; default k8s: http://printscan.<ns>.svc:8081; docker: http://<internal host or 127.0.0.1>:<internal port>)")
	fl.StringVar(&f.dockerHost, "docker-host", env("DOCKER_HOST", ""), "docker: Engine API address, unix:// or tcp:// (env DOCKER_HOST; default unix:///var/run/docker.sock)")
	fl.StringVar(&f.dockerContainer, "docker-container", env("PRINTSCAN_DOCKER_CONTAINER", docker.DefaultContainer), "docker: the control plane's container, whose image the probe and agent run (env PRINTSCAN_DOCKER_CONTAINER)")
	fl.StringVar(&f.hostIP, "host-ip", env("PRINTSCAN_HOST_IP", ""), "docker: the address the device serves on (env PRINTSCAN_HOST_IP; default the default route's source address)")
	fl.StringVar(&f.registry, "image-registry", env("PRINTSCAN_IMAGE_REGISTRY", "ghcr.io/thephaseless"), "image registry (env PRINTSCAN_IMAGE_REGISTRY)")
	fl.StringVar(&f.tag, "image-tag", env("PRINTSCAN_IMAGE_TAG", k8s.ImageTag(version)), "device and driver image tag (env PRINTSCAN_IMAGE_TAG)")
	fl.StringVar(&f.allowedHosts, "allowed-hosts", env("PRINTSCAN_ALLOWED_HOSTS", ""),
		"comma-separated host names the UI and API answer; others get 421. * turns the check off (env PRINTSCAN_ALLOWED_HOSTS; default: localhost,127.0.0.1,[::1] with the sim backend, those plus the host IP with docker, no check with k8s)")
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
	setMemoryLimit(cgroupMemoryMax)
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
	var dockerBackend *docker.Backend
	usbAvailable, usbUnavailable := f.usbResource != "", ""
	switch f.backend {
	case controller.BackendK8s:
		if backend, err = newK8sBackend(f, overrides); err != nil {
			return err
		}
		if !usbAvailable {
			usbUnavailable = noUSBResource
		}
	case controller.BackendDocker:
		if dockerBackend, err = newDockerBackend(f, overrides); err != nil {
			return err
		}
		backend = dockerBackend
		// The backend's USB gate covers a host without a printer.
		usbAvailable = true
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
		// on k8s or docker, where Apply(nil) deletes the device workload, which
		// keeps serving the LAN across control-plane restarts.
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

	// shortcut: the docker allowlist takes the host IP once at start, so a DHCP
	// change (or a route lookup that fell back to 127.0.0.1) needs a restart or
	// --allowed-hosts/--host-ip; upgrade to a per-request check if that bites.
	hostIP := f.hostIP
	if dockerBackend != nil && hostIP == "" && strings.TrimSpace(f.allowedHosts) == "" {
		hostIP = dockerBackend.HostIP(ctx)
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

		AllowedHosts:      allowedHostsFor(f.backend, f.allowedHosts, hostIP),
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
	if dockerBackend != nil {
		// The worker stops with ctx and never tears the device containers down.
		wg.Go(func() { dockerBackend.Run(ctx) })
	}
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

func newDockerBackend(f serveFlags, overrides map[string]string) (*docker.Backend, error) {
	agentURL, err := dockerAgentURL(f.agentURL, f.internalListen)
	if err != nil {
		return nil, err
	}
	eng, err := docker.NewClient(f.dockerHost)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return docker.New(eng, docker.Params{
		Registry: f.registry, Tag: f.tag, Overrides: overrides,
		AgentURL: agentURL, Container: f.dockerContainer, HostIP: f.hostIP,
		TokenFile: filepath.Join(f.dataDir, "agent-token"), SysRoot: "/sys",
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
