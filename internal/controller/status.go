package controller

import (
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
)

// Backend names serve passes as Options.BackendName (the Setup DTO's backend).
const (
	BackendK8s = "k8s"
	BackendSim = "sim"
)

// Status messages from the controller rules.
const (
	msgWaitingAgent  = "Waiting for the device agent"
	msgUnschedulable = "Printer not plugged in or not visible to the device plugin"
	msgNoPrinter     = "No printer found on USB"
	msgNoMatch       = "No USB device matches device.usb"
	msgNotReporting  = "Device agent not reporting"
	msgStarting      = "Starting the device workload"
	msgWaitPrinter   = "Waiting for the printer"
)

const defaultName = "Printer"

// initialStatus is Current() before any input (controller rules, "Initial Status").
func initialStatus() apitypes.Status {
	return apitypes.Status{
		Config: apitypes.StatusConfig{State: apitypes.ConfigMissing, Errors: []config.FieldError{}},
		Device: apitypes.StatusDevice{
			Phase: apitypes.PhaseNone, Mode: apitypes.ModeNone,
			Advertised: []apitypes.AdvertisedService{}, Blobs: []device.BlobStatus{},
		},
		Printer:   offlinePrinter(""),
		Scanner:   apitypes.StatusScanner{State: apitypes.StateUnknown},
		Reconcile: []apitypes.ReconcileItem{},
		Connect:   apitypes.Connect{IPPURIs: []string{}, ESCLURLs: []string{}},
	}
}

func offlinePrinter(uri string) apitypes.StatusPrinter {
	return apitypes.StatusPrinter{
		URI: uri, State: apitypes.StateUnknown,
		Reasons: []string{}, Formats: []string{}, Media: []string{}, Markers: []apitypes.Marker{},
	}
}

// modeOf is Status device.mode: the spec's mode, or none while the spec is nil.
func modeOf(s *device.Spec) string {
	if s == nil {
		return apitypes.ModeNone
	}
	return s.Mode
}

// pickService returns the browsed instance of type typ whose TXT has key: the
// one named preferName, else the first by name order.
func pickService(services []device.DNSSDService, typ, key, preferName string) (device.DNSSDService, bool) {
	var cands []device.DNSSDService
	for _, s := range services {
		if _, ok := s.TXT[key]; ok && s.Type == typ {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return device.DNSSDService{}, false
	}
	if i := slices.IndexFunc(cands, func(s device.DNSSDService) bool { return preferName != "" && s.Name == preferName }); i >= 0 {
		return cands[i], true
	}
	return slices.MinFunc(cands, func(a, b device.DNSSDService) int { return strings.Compare(a.Name, b.Name) }), true
}

// pickIPP picks the _ipp._tcp instance with TXT rp set, preferring preferName.
func pickIPP(services []device.DNSSDService, preferName string) (device.DNSSDService, bool) {
	return pickService(services, device.ServiceIPP, "rp", preferName)
}

func serviceURL(scheme, hostIP string, s device.DNSSDService, key string) string {
	return scheme + "://" + net.JoinHostPort(hostIP, strconv.Itoa(s.Port)) + "/" + strings.TrimPrefix(s.TXT[key], "/")
}

// preferredName is the DNS-SD name the printer advertises under when printscan
// names it (PAPPL and simulated families); ipp-usb advertises the device's own.
func preferredName(fam catalog.Printer, cfg *config.Config) string {
	if cfg == nil || fam.ID == "ipp-usb" {
		return ""
	}
	return cfg.Name
}

// printerURI is the one Printer URI rule for both backends.
func printerURI(backend string, fam catalog.Printer, obs device.Observation, r *device.AgentReport, preferName string) string {
	switch {
	case fam.ID == "":
		return ""
	case fam.App != "": // PAPPL: the agent's loopback URI, rewritten to the host IP
		if r == nil || r.Queue == nil || r.Queue.PrinterURI == "" || obs.HostIP == "" {
			return ""
		}
		u, err := url.Parse(r.Queue.PrinterURI)
		if err != nil || u.Port() == "" {
			return ""
		}
		return "ipp://" + net.JoinHostPort(obs.HostIP, u.Port()) + u.EscapedPath()
	case backend == BackendSim:
		return obs.Endpoints.PrinterURI
	case r == nil || obs.HostIP == "":
		return ""
	}
	s, ok := pickIPP(r.Services, preferName)
	if !ok {
		return ""
	}
	return serviceURL("ipp", obs.HostIP, s, "rp")
}

// scannerURL is the eSCL base URL: the observation in sim, _uscan._tcp in k8s.
func scannerURL(backend string, spec *device.Spec, obs device.Observation, r *device.AgentReport) string {
	switch {
	case spec == nil || spec.Scanner == "":
		return ""
	case backend == BackendSim:
		return obs.Endpoints.ScannerURL
	case r == nil || obs.HostIP == "":
		return ""
	}
	s, ok := pickService(r.Services, device.ServiceUScan, "rs", "")
	if !ok {
		return ""
	}
	return serviceURL("http", obs.HostIP, s, "rs")
}

func advertised(r *device.AgentReport) []apitypes.AdvertisedService {
	out := []apitypes.AdvertisedService{}
	if r != nil {
		for _, s := range r.Services {
			out = append(out, apitypes.AdvertisedService{Type: s.Type, Name: s.Name, Port: s.Port})
		}
	}
	return out
}

func connect(r *device.AgentReport, preferName, ippURI, esclURL string) apitypes.Connect {
	c := apitypes.Connect{IPPURIs: []string{}, ESCLURLs: []string{}}
	if r != nil {
		if s, ok := pickIPP(r.Services, preferName); ok {
			c.DNSSDName = s.Name
		}
	}
	if ippURI != "" {
		c.IPPURIs = append(c.IPPURIs, ippURI)
	}
	if esclURL != "" {
		c.ESCLURLs = append(c.ESCLURLs, esclURL)
	}
	return c
}

// accepted reports whether EULA id is accepted at the catalog's version.
func accepted(cat *catalog.Catalog, id string, acc []config.EULARef) bool {
	e, ok := cat.EULA(id)
	return ok && slices.Contains(acc, config.EULARef{ID: id, Version: e.Version})
}

// blobRefs lists, in catalog order, the blobs whose EULA the decision needs or
// offers and the config accepts.
func blobRefs(dec device.Decision, acc []config.EULARef, cat *catalog.Catalog) []device.BlobRef {
	refs := []device.BlobRef{}
	for _, b := range cat.Blobs {
		optional := slices.Contains(dec.OptionalEULAs, b.EULA)
		if !optional && !slices.Contains(dec.RequiredEULAs, b.EULA) || !accepted(cat, b.EULA, acc) {
			continue
		}
		e, _ := cat.EULA(b.EULA)
		refs = append(refs, device.BlobRef{ID: b.ID, Version: e.Version, Optional: optional})
	}
	return refs
}

func eulaTitle(cat *catalog.Catalog, id string) string {
	if e, ok := cat.EULA(id); ok && e.Title != "" {
		return e.Title
	}
	return id
}

// degradedMessage is the first EULA or blob problem: a required EULA not
// accepted, or a reported blob that failed or is disabled.
func degradedMessage(cat *catalog.Catalog, dec *device.Decision, cfg *config.Config, blobs []device.BlobStatus) string {
	if dec != nil && cfg != nil {
		for _, id := range dec.RequiredEULAs {
			if !accepted(cat, id, cfg.Licenses.AcceptedEULAs) {
				return "Accept EULA " + eulaTitle(cat, id) + " in Settings"
			}
		}
	}
	for _, b := range blobs {
		if b.State == device.BlobFailed || b.State == device.BlobDisabled {
			title := b.ID
			if cb, ok := cat.Blob(b.ID); ok {
				title = eulaTitle(cat, cb.EULA)
			}
			return title + ": " + b.Message
		}
	}
	return ""
}

// decisionFor is Decide for Status and Setup: the simulated decision for mode
// simulated, otherwise nil without an inventory or on any Decide error.
func decisionFor(cat *catalog.Catalog, sel config.Device, inv *device.Inventory) *device.Decision {
	if inv == nil && sel.Mode != config.ModeSimulated {
		return nil
	}
	var in device.Inventory
	if inv != nil {
		in = *inv
	}
	d, err := cat.Decide(in, sel)
	if err != nil {
		return nil
	}
	return &d
}

// noPrinterMessage is the probing message for a Decide error.
func noPrinterMessage(err error, sel config.Device) string {
	if errors.Is(err, catalog.ErrNoPrinter) && sel.USB != nil {
		return msgNoMatch
	}
	return msgNoPrinter
}

// sanitizeName turns a USB product string into a valid config name.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 63 {
		s = strings.TrimSpace(string(r[:63]))
	}
	if s == "" {
		return defaultName
	}
	return s
}

type setupInput struct {
	backend      string
	usbAvailable bool
	missing      bool
	cfg          *config.Config // the valid config, else the last-known-good, else nil
	inventory    *device.Inventory
}

// setup builds the Setup DTO (controller rules, "Setup DTO").
func setup(cat *catalog.Catalog, in setupInput) apitypes.Setup {
	sel := config.Default(defaultName).Device
	acc := []config.EULARef{}
	if in.cfg != nil {
		sel, acc = in.cfg.Device, in.cfg.Licenses.AcceptedEULAs
	}
	dec := decisionFor(cat, sel, in.inventory)
	s := apitypes.Setup{
		Required: in.missing, Backend: in.backend, USBResourceConfigured: in.usbAvailable,
		Inventory: in.inventory, Decision: dec, EULAs: []apitypes.SetupEULA{},
	}
	var ids []string
	var required []string
	if dec != nil {
		required = dec.RequiredEULAs
		ids = append(slices.Clone(dec.RequiredEULAs), dec.OptionalEULAs...)
	}
	for _, a := range acc {
		ids = append(ids, a.ID)
	}
	for _, id := range ids {
		e, ok := cat.EULA(id)
		if !ok || slices.ContainsFunc(s.EULAs, func(x apitypes.SetupEULA) bool { return x.ID == id }) {
			continue
		}
		s.EULAs = append(s.EULAs, apitypes.SetupEULA{
			ID: e.ID, Version: e.Version, Title: e.Title, Vendor: e.Vendor, URL: e.URL, Summary: e.Summary,
			Required: slices.Contains(required, id),
		})
	}
	name := defaultName
	if dec != nil && dec.Device != nil {
		name = sanitizeName(dec.Device.Product)
	}
	s.Suggested = config.Default(name)
	if !in.usbAvailable {
		s.Suggested.Device.Mode = config.ModeSimulated
	}
	return s
}
