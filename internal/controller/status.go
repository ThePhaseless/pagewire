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

// Status messages the controller shows.
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

// initialStatus is Current() before any input: config missing, phase and mode
// none, printer and scanner state unknown, empty lists, no decision or inventory.
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
		Reasons: []string{}, Formats: []string{}, ConvertedFormats: []apitypes.FormatConversion{},
		Media: []string{}, Markers: []apitypes.Marker{},
	}
}

// modeOf is Status device.mode: the spec's mode, or none while the spec is nil.
func modeOf(s *device.Spec) string {
	if s == nil {
		return apitypes.ModeNone
	}
	return s.Mode
}

// pickService returns the browsed instance of type typ whose TXT has key.
// Instances on one of the family's own ports (familyPort, nil: any) come
// first, so another service of the same type on the node cannot win by
// name; among them the one named preferName, else the first by name order.
func pickService(services []device.DNSSDService, typ, key, preferName string, familyPort func(int) bool) (device.DNSSDService, bool) {
	var cands []device.DNSSDService
	for _, s := range services {
		if _, ok := s.TXT[key]; ok && s.Type == typ {
			cands = append(cands, s)
		}
	}
	if len(cands) == 0 {
		return device.DNSSDService{}, false
	}
	if familyPort != nil && slices.ContainsFunc(cands, func(s device.DNSSDService) bool { return familyPort(s.Port) }) {
		cands = slices.DeleteFunc(cands, func(s device.DNSSDService) bool { return !familyPort(s.Port) })
	}
	if i := slices.IndexFunc(cands, func(s device.DNSSDService) bool { return preferName != "" && s.Name == preferName }); i >= 0 {
		return cands[i], true
	}
	return slices.MinFunc(cands, func(a, b device.DNSSDService) int { return strings.Compare(a.Name, b.Name) }), true
}

// pickIPP picks the _ipp._tcp instance with TXT rp set for printer family
// fam: on ipp-usb's ports for the ipp-usb family, then preferName.
func pickIPP(services []device.DNSSDService, fam catalog.Printer, preferName string) (device.DNSSDService, bool) {
	var ports func(int) bool
	if fam.ID == "ipp-usb" {
		ports = ippUSBPort
	}
	return pickService(services, device.ServiceIPP, "rp", preferName, ports)
}

// ippUSBPort reports whether port is one ipp-usb listens on.
func ippUSBPort(port int) bool { return port >= device.IPPUSBMinPort && port <= device.IPPUSBMaxPort }

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
	s, ok := pickIPP(r.Services, fam, preferName)
	if !ok {
		return ""
	}
	return serviceURL("ipp", obs.HostIP, s, "rp")
}

// scannerURL is the eSCL base URL: the observation in sim, _uscan._tcp in
// k8s, preferring the scanner family's catalog port (ipp-usb's own ports for
// ipp-usb's eSCL).
func scannerURL(cat *catalog.Catalog, backend string, spec *device.Spec, obs device.Observation, r *device.AgentReport) string {
	switch {
	case spec == nil || spec.Scanner == "":
		return ""
	case backend == BackendSim:
		return obs.Endpoints.ScannerURL
	case r == nil || obs.HostIP == "":
		return ""
	}
	var ports func(int) bool
	if i := slices.IndexFunc(cat.Scanners, func(s catalog.Scanner) bool { return s.ID == spec.Scanner }); i >= 0 {
		switch port := cat.Scanners[i].Port; {
		case port != 0:
			ports = func(p int) bool { return p == port }
		case spec.Scanner == "ipp-usb":
			ports = ippUSBPort
		}
	}
	s, ok := pickService(r.Services, device.ServiceUScan, "rs", "", ports)
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

func connect(r *device.AgentReport, fam catalog.Printer, preferName, ippURI, esclURL string) apitypes.Connect {
	c := apitypes.Connect{IPPURIs: []string{}, ESCLURLs: []string{}}
	if r != nil {
		if s, ok := pickIPP(r.Services, fam, preferName); ok {
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

// acceptedRef returns the config entry accepting EULA id at the catalog's
// version, matched on id and version only.
func acceptedRef(cat *catalog.Catalog, id string, acc []config.EULARef) (config.EULARef, bool) {
	e, ok := cat.EULA(id)
	if !ok {
		return config.EULARef{}, false
	}
	i := slices.IndexFunc(acc, func(a config.EULARef) bool { return a.ID == id && a.Version == e.Version })
	if i < 0 {
		return config.EULARef{}, false
	}
	return acc[i], true
}

// accepted reports whether EULA id is accepted at the catalog's version.
func accepted(cat *catalog.Catalog, id string, acc []config.EULARef) bool {
	_, ok := acceptedRef(cat, id, acc)
	return ok
}

// acceptedEULAs is Spec.AcceptedEULAs: the accepted entries, with their text
// sha256, of the EULAs of refs, sorted by id.
func acceptedEULAs(refs []device.BlobRef, acc []config.EULARef, cat *catalog.Catalog) []device.EULARef {
	out := []device.EULARef{}
	for _, r := range refs {
		b, ok := cat.Blob(r.ID)
		if !ok || slices.ContainsFunc(out, func(e device.EULARef) bool { return e.ID == b.EULA }) {
			continue
		}
		if a, ok := acceptedRef(cat, b.EULA, acc); ok {
			out = append(out, device.EULARef{ID: a.ID, Version: a.Version, TextSHA256: a.TextSHA256})
		}
	}
	slices.SortFunc(out, func(a, b device.EULARef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// eulaIDs lists the catalog EULAs the Setup DTO shows, without duplicates:
// the decision's required and optional EULAs, then the accepted ones.
func eulaIDs(cat *catalog.Catalog, dec *device.Decision, acc []config.EULARef) []string {
	var ids []string
	if dec != nil {
		ids = append(slices.Clone(dec.RequiredEULAs), dec.OptionalEULAs...)
	}
	for _, a := range acc {
		ids = append(ids, a.ID)
	}
	out := []string{}
	for _, id := range ids {
		if _, ok := cat.EULA(id); ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// stageEULAs is DesiredState.StageEULAs: every EULA of eulaIDs at its
// catalog version, sorted by id so an unchanged list compares equal.
// Accepted EULAs stay in, as texts live only in memory and Settings needs
// them after a control-plane restart.
func stageEULAs(cat *catalog.Catalog, dec *device.Decision, acc []config.EULARef) []device.EULARef {
	out := []device.EULARef{}
	for _, id := range eulaIDs(cat, dec, acc) {
		e, _ := cat.EULA(id)
		out = append(out, device.EULARef{ID: id, Version: e.Version})
	}
	slices.SortFunc(out, func(a, b device.EULARef) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// eulaText is the stored agent text of EULA e: pending until a report carried
// it at the catalog version.
func eulaText(e catalog.EULAInfo, texts map[string]device.EULAText) device.EULAText {
	if t, ok := texts[e.ID]; ok && t.Version == e.Version {
		return t
	}
	return device.EULAText{ID: e.ID, Version: e.Version, State: device.EULAPending}
}

// eulaDTO is the body of GET /eulas/{id}.
func eulaDTO(e catalog.EULAInfo, texts map[string]device.EULAText) apitypes.EULA {
	t := eulaText(e, texts)
	out := apitypes.EULA{
		ID: e.ID, Version: e.Version, Title: e.Title, Vendor: e.Vendor, Summary: e.Summary,
		State: t.State, Message: t.Message,
	}
	if t.State == device.EULAAvailable {
		out.TextSHA256, out.Text = &t.TextSHA256, &t.Text
	}
	return out
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
	// A leading - is refused by validation (ippeveprinter would read it as an option).
	s = strings.TrimLeft(strings.TrimSpace(s), "- ")
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
	texts        map[string]device.EULAText // latest agent EULA texts by id
}

// setup builds the Setup DTO: the inventory and decision for the configured
// (or default) selector, the EULAs that decision needs or offers plus the
// accepted ones with their text state, and a suggested config.
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
	var required []string
	if dec != nil {
		required = dec.RequiredEULAs
	}
	for _, id := range eulaIDs(cat, dec, acc) {
		e, _ := cat.EULA(id)
		d := eulaDTO(e, in.texts)
		s.EULAs = append(s.EULAs, apitypes.SetupEULA{
			ID: e.ID, Version: e.Version, Title: e.Title, Vendor: e.Vendor, Summary: e.Summary,
			Required: slices.Contains(required, id), State: d.State, TextSHA256: d.TextSHA256,
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
