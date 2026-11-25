package controller

import (
	"testing"

	"github.com/ThePhaseless/printscan/internal/catalog"
	"github.com/ThePhaseless/printscan/internal/device"
)

// Docker reports endpoints like k8s (agent-browsed services plus HostIP);
// only sim takes them from the observation.
func TestEndpointsByBackend(t *testing.T) {
	cat := catalog.Default()
	ippusb, _ := cat.Printer("ipp-usb")
	obs := device.Observation{
		HostIP:    "192.168.1.20",
		Endpoints: device.Endpoints{PrinterURI: "ipp://sim/printer", ScannerURL: "http://sim/eSCL"},
	}
	r := &device.AgentReport{Services: []device.DNSSDService{
		{Type: device.ServiceIPP, Name: "p", Port: device.IPPUSBMinPort, TXT: map[string]string{"rp": "ipp/print"}},
		{Type: device.ServiceUScan, Name: "s", Port: device.IPPUSBMaxPort, TXT: map[string]string{"rs": "eSCL"}},
	}}
	spec := &device.Spec{Scanner: "ipp-usb"}
	browsedIPP := "ipp://192.168.1.20:60000/ipp/print"
	browsedESCL := "http://192.168.1.20:60009/eSCL"
	for _, tc := range []struct{ backend, ipp, escl string }{
		{BackendK8s, browsedIPP, browsedESCL},
		{BackendDocker, browsedIPP, browsedESCL},
		{BackendSim, "ipp://sim/printer", "http://sim/eSCL"},
	} {
		if got := printerURI(tc.backend, ippusb, obs, r, ""); got != tc.ipp {
			t.Errorf("%s printer URI %q, want %q", tc.backend, got, tc.ipp)
		}
		if got := scannerURL(cat, tc.backend, spec, obs, r); got != tc.escl {
			t.Errorf("%s scanner URL %q, want %q", tc.backend, got, tc.escl)
		}
	}
}
