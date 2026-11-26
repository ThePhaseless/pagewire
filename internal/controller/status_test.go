package controller

import (
	"reflect"
	"testing"
	"time"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

func svc(typ, name string, port int, txt ...string) device.DNSSDService {
	m := map[string]string{}
	for i := 0; i+1 < len(txt); i += 2 {
		m[txt[i]] = txt[i+1]
	}
	return device.DNSSDService{Type: typ, Name: name, Port: port, TXT: m}
}

// Docker reports endpoints like k8s (agent-browsed services plus HostIP);
// only sim takes them from the observation.
func TestEndpointsByBackend(t *testing.T) {
	cat := catalog.Default()
	hp := catalog.Fixture("ippusb-mfp").Devices[0]
	w := workloadIn{
		obs: device.Observation{HostIP: "192.168.1.20", Endpoints: []device.Endpoint{
			{ID: "p", PrinterURI: "ipp://sim/printer"}, {ID: "s", ScannerURL: "http://sim/eSCL"},
		}},
		rs: &reportState{r: device.AgentReport{Services: []device.DNSSDService{
			svc(device.ServiceIPP, "p", device.IPPUSBMinPort, "rp", "ipp/print"),
			svc(device.ServiceUScan, "s", device.IPPUSBMinPort, "rs", "eSCL"),
		}}},
	}
	p, ps := config.Printer{ID: "p"}, device.PrinterSpec{ID: "p", Family: "ipp-usb"}
	sc, ss := config.Scanner{ID: "s"}, device.ScannerSpec{ID: "s", Family: "ipp-usb"}
	for _, tc := range []struct{ backend, ipp, escl string }{
		{BackendK8s, "ipp://192.168.1.20:60000/ipp/print", "http://192.168.1.20:60000/eSCL"},
		{BackendDocker, "ipp://192.168.1.20:60000/ipp/print", "http://192.168.1.20:60000/eSCL"},
		{BackendSim, "ipp://sim/printer", "http://sim/eSCL"},
	} {
		e := printerEndpoint(cat, tc.backend, w, p, ps, hp)
		if e.uri != tc.ipp {
			t.Errorf("%s printer URI %q, want %q", tc.backend, e.uri, tc.ipp)
		}
		if want := tc.backend != BackendSim; e.ippUSB != want { // sim serves the family itself
			t.Errorf("%s printer ippUSB = %v, want %v", tc.backend, e.ippUSB, want)
		}
		if got := scannerEndpoint(cat, tc.backend, w, sc, ss, hp, nil).uri; got != tc.escl {
			t.Errorf("%s scanner URL %q, want %q", tc.backend, got, tc.escl)
		}
	}
}

func TestIPPUSBMatchesByTXT(t *testing.T) {
	cat := catalog.Default()
	hp := catalog.Fixture("ippusb-mfp").Devices[0]
	other := device.USBDevice{DeviceID: "MFG:Canon;MDL:PIXMA G3000;"}
	services := []device.DNSSDService{
		svc(device.ServiceIPP, "AAA Canon", 60000, "rp", "ipp/print", "usb_MFG", "Canon", "usb_MDL", "PIXMA G3000"),
		svc(device.ServiceUScan, "AAA Canon scan", 60000, "rs", "eSCL"),
		svc(device.ServiceIPP, "ZZZ HP", 60001, "rp", "ipp/print", "usb_MFG", "hp", "usb_MDL", "officejet pro 8020 SERIES"),
		svc(device.ServiceUScan, "ZZZ HP scan", 60001, "rs", "eSCL"),
		svc(device.ServiceIPP, "AAA foreign", 631, "rp", "ipp/print"),
	}
	w := workloadIn{obs: device.Observation{HostIP: "10.0.0.1"}, rs: &reportState{r: device.AgentReport{Services: services}}}
	ps := device.PrinterSpec{Family: "ipp-usb"}
	if e := printerEndpoint(cat, BackendK8s, w, config.Printer{}, ps, hp); e.uri != "ipp://10.0.0.1:60001/ipp/print" || e.dnssd != "ZZZ HP" {
		t.Errorf("HP printer %+v", e)
	}
	if e := printerEndpoint(cat, BackendK8s, w, config.Printer{}, ps, other); e.dnssd != "AAA Canon" {
		t.Errorf("Canon printer %+v", e)
	}
	ss := device.ScannerSpec{Family: "ipp-usb"}
	if e := scannerEndpoint(cat, BackendK8s, w, config.Scanner{}, ss, hp, nil); e.uri != "http://10.0.0.1:60001/eSCL" || e.dnssd != "ZZZ HP scan" {
		t.Errorf("HP scanner %+v, want the eSCL instance on the printer's port", e)
	}
	// Without the TXT keys (ipp-usb releases after 0.9.34): the single candidate.
	single := workloadIn{obs: w.obs, rs: &reportState{r: device.AgentReport{Services: []device.DNSSDService{
		svc(device.ServiceIPP, "HP", 60002, "rp", "ipp/print"),
	}}}}
	if e := printerEndpoint(cat, BackendK8s, single, config.Printer{}, ps, hp); e.uri != "ipp://10.0.0.1:60002/ipp/print" {
		t.Errorf("single candidate %+v", e)
	}
}

func TestAirSaneMatchesByTy(t *testing.T) {
	cat := catalog.Default()
	office := catalog.Fixture("office").Devices
	sam, lide := office[2], office[3]
	services := []device.DNSSDService{
		svc(device.ServiceUScan, "A", 8090, "rs", "eSCL/a", "ty", "CanoScan LiDE 100"),
		svc(device.ServiceUScan, "B", 8090, "rs", "eSCL/b", "ty", "Samsung SCX-4300 Series"),
		svc(device.ServiceUScan, "C", 8090, "rs", "eSCL/c", "ty", "Test scanner"),
		svc(device.ServiceUScan, "D", 9000, "rs", "eSCL/d", "ty", "SCX-4300 Series"),
	}
	w := workloadIn{obs: device.Observation{HostIP: "10.0.0.1"}, rs: &reportState{r: device.AgentReport{Services: services}}}
	air := device.ScannerSpec{Family: "airsane"}
	if e := scannerEndpoint(cat, BackendK8s, w, config.Scanner{}, air, sam, nil); e.dnssd != "B" {
		t.Errorf("Samsung %+v", e)
	}
	if e := scannerEndpoint(cat, BackendK8s, w, config.Scanner{}, air, lide, nil); e.dnssd != "A" {
		t.Errorf("LiDE %+v", e)
	}
	sim := device.ScannerSpec{Family: "sim"}
	if e := scannerEndpoint(cat, BackendK8s, w, config.Scanner{}, sim, device.USBDevice{}, []device.USBDevice{sam, lide}); e.dnssd != "C" {
		t.Errorf("simulated scanner %+v, want the instance no USB scanner matches", e)
	}
}

func TestSimulatedPrinterEndpoint(t *testing.T) {
	cat := catalog.Default()
	for _, tc := range []struct {
		services []device.DNSSDService
		uri      string
		name     string
	}{
		{[]device.DNSSDService{svc(device.ServiceIPP, "Other printer", 631, "rp", "ipp/print"), svc(device.ServiceIPP, "Living room printer", 8631, "rp", "ipp/print")}, "ipp://10.0.0.1:8631/ipp/print", "Living room printer"},
		{[]device.DNSSDService{svc(device.ServiceIPP, "Something else", 8631, "rp", "ipp/print")}, "ipp://10.0.0.1:8631/ipp/print", "Something else"},
		{[]device.DNSSDService{svc(device.ServiceIPP, "Living room printer", 9999, "rp", "ipp/print")}, "ipp://10.0.0.1:9999/ipp/print", "Living room printer"},
		{[]device.DNSSDService{svc(device.ServiceIPP, "Other", 9999, "rp", "ipp/print")}, "", ""},
	} {
		w := workloadIn{obs: device.Observation{HostIP: "10.0.0.1"}, rs: &reportState{r: device.AgentReport{Services: tc.services}}}
		e := printerEndpoint(cat, BackendK8s, w, config.Printer{Name: "Living room printer"}, device.PrinterSpec{Family: "sim", Port: 8631}, device.USBDevice{})
		if e.uri != tc.uri || e.dnssd != tc.name {
			t.Errorf("%+v, want %q %q", e, tc.uri, tc.name)
		}
	}
}

func TestLossKey(t *testing.T) {
	p := catalog.Fixture("ippusb-mfp").Devices[0]
	at := func(addr int) device.USBDevice { d := p; d.Address, d.DeviceID = addr, ""; return d }
	rep := func(inv []device.USBDevice, att []device.USBDevice, nodes []string) *device.AgentReport {
		return &device.AgentReport{Inventory: device.Inventory{Devices: inv}, Attached: att, ProbeNodes: nodes}
	}
	moved := at(5)
	for name, tc := range map[string]struct {
		k8s, docker, printer, idKnown bool
		r                             *device.AgentReport
		want                          string
	}{
		"healthy":           {k8s: true, r: rep([]device.USBDevice{p}, []device.USBDevice{at(4)}, []string{p.Node()})},
		"old agent":         {k8s: true, r: rep([]device.USBDevice{p}, nil, nil)},
		"moved":             {k8s: true, r: rep([]device.USBDevice{p}, []device.USBDevice{moved}, nil), want: "moved " + moved.Node()},
		"switched off":      {k8s: true, r: rep([]device.USBDevice{p}, []device.USBDevice{}, []string{p.Node()})},
		"unallocated":       {k8s: true, r: rep([]device.USBDevice{p}, []device.USBDevice{at(4)}, []string{"001/099"}), want: "unallocated " + p.Node()},
		"appeared k8s":      {k8s: true, r: rep(nil, []device.USBDevice{moved}, []string{}), want: "appeared " + moved.Node()},
		"appeared docker":   {docker: true, printer: true, r: rep(nil, []device.USBDevice{moved}, nil), want: "appeared " + moved.Node()},
		"docker cached id":  {docker: true, printer: true, idKnown: true, r: rep(nil, []device.USBDevice{moved}, nil)},
		"docker scanner":    {docker: true, r: rep(nil, []device.USBDevice{moved}, nil)},
		"docker moved":      {docker: true, printer: true, r: rep([]device.USBDevice{p}, []device.USBDevice{moved}, nil)},
		"sim never":         {printer: true, r: rep(nil, []device.USBDevice{moved}, nil)},
		"two of the model":  {k8s: true, r: rep([]device.USBDevice{p}, []device.USBDevice{at(5), at(6)}, nil)},
		"probe lacks moved": {k8s: true, r: rep([]device.USBDevice{p}, []device.USBDevice{moved}, []string{p.Node()}), want: "moved " + moved.Node()},
	} {
		if got := lossKey(tc.k8s, tc.docker, tc.printer, p.Key(), tc.r, tc.idKnown); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestFitScan(t *testing.T) {
	caps := apitypes.ScannerCaps{Sources: []string{"platen", "adf"}, Resolutions: []int{150, 300}, ColorModes: []string{"color"}, Intents: []string{}}
	got := fitScan(config.ScanDefaults{Source: "adf-duplex", Resolution: 600, ColorMode: "grayscale", Intent: "photo", Format: "jpeg"}, caps)
	want := []apitypes.Warning{
		warning("source", "adf-duplex", "platen", "scanner"),
		warning("resolution", "600", "150", "scanner"),
		warning("colorMode", "grayscale", "color", "scanner"),
		warning("intent", "photo", "", "scanner"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fitScan = %+v", got)
	}
	if got := fitScan(config.ScanDefaults{Source: "platen", Resolution: 300}, caps); len(got) != 0 {
		t.Fatalf("supported defaults warned: %+v", got)
	}
}

func TestKnownDevices(t *testing.T) {
	p := catalog.Fixture("ippusb-mfp").Devices[0]
	kb := catalog.Fixture("office").Devices[0]
	live := p
	live.Address, live.DeviceID = 6, ""
	kbLive := kb
	got := known(device.AgentReport{Inventory: device.Inventory{Devices: []device.USBDevice{p}}, Attached: []device.USBDevice{live, kbLive}}, map[device.USBKey]string{})
	if len(got) != 2 || got[0].Address != 6 || got[0].DeviceID != p.DeviceID || got[1].VendorID != kb.VendorID {
		t.Fatalf("known %+v, want the moved printer with its device ID, then the keyboard", got)
	}
	cached := known(device.AgentReport{Attached: []device.USBDevice{live}}, map[device.USBKey]string{p.Key(): "MFG:HP;"})
	if len(cached) != 1 || cached[0].DeviceID != "MFG:HP;" {
		t.Fatalf("known %+v, want the cached device ID", cached)
	}
}

func TestWorkloadSet(t *testing.T) {
	now := t0.Add(time.Hour)
	fresh := map[string]*reportState{"n3": {at: now}, "": {at: now}}
	old := map[string]*reportState{"n4": {at: t0}}
	serving := map[string]*device.Spec{"n5": {Mode: device.ModeUSB}, "n6": {Mode: device.ModeProbe}}
	for name, tc := range map[string]struct {
		listed  bool
		nodes   []string
		reports map[string]*reportState
		applied map[string]*device.Spec
		want    []string
	}{
		"no lister":                  {nodes: []string{"n1"}, want: []string{""}},
		"listed":                     {listed: true, nodes: []string{"n2", "n1"}, want: []string{"n1", "n2"}},
		"empty list":                 {listed: true, reports: map[string]*reportState{"": {at: now}}, want: []string{""}},
		"reported node":              {listed: true, nodes: []string{"n1"}, reports: fresh, want: []string{"n1", "n3"}},
		"stale report leaves":        {listed: true, nodes: []string{"n1"}, reports: old, want: []string{"n1"}},
		"serving spec stays":         {listed: true, applied: serving, want: []string{"n5"}},
		"empty list with a reporter": {listed: true, reports: fresh, want: []string{"n3"}},
	} {
		if got := workloadSet(tc.listed, tc.nodes, tc.reports, tc.applied, now, nil); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	// Only an unlisted reporter's stay records a deadline.
	var w wakeAt
	workloadSet(true, []string{"n3"}, fresh, nil, now, &w)
	if !w.t.IsZero() {
		t.Errorf("a listed node recorded %v", w.t)
	}
	workloadSet(true, nil, fresh, nil, now, &w)
	if !w.t.Equal(now.Add(pendingWait)) {
		t.Errorf("deadline %v, want the unlisted report's pendingWait", w.t)
	}
}
