// Package device holds the device contract shared by the control plane, the
// backends and the agent: USB inventory, the dispatcher's decision, the desired
// device workload (Spec), the Backend interface and the agent protocol.
//
// Every type carries lowerCamel json tags; the same names are used in fixture
// files, the probe output, the blob status file, the agent protocol and the API.
package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Spec modes.
const (
	ModeUSB       = "usb"
	ModeSimulated = "simulated"
	ModeProbe     = "probe" // probe + agent only, feeds the wizard
)

// IPPUSBMinPort and IPPUSBMaxPort bound the HTTP ports ipp-usb listens on
// (its rendered config) and so the ports its DNS-SD services advertise.
const (
	IPPUSBMinPort = 60000
	IPPUSBMaxPort = 60009
)

// Observation phases.
const (
	PhaseAbsent  = "absent"
	PhasePending = "pending"
	PhaseRunning = "running"
	PhaseFailed  = "failed"
)

// BlobStatus states.
const (
	BlobInstalled = "installed"
	BlobMissing   = "missing"
	BlobFailed    = "failed"
	BlobDisabled  = "disabled"
)

// USBInterface is one interface's class triple.
type USBInterface struct {
	Class    uint8 `json:"class"`
	SubClass uint8 `json:"subClass"`
	Protocol uint8 `json:"protocol"`
}

// USBDevice is one device found on the USB bus.
type USBDevice struct {
	VendorID     string         `json:"vendorId"`  // 4 lowercase hex
	ProductID    string         `json:"productId"` // 4 lowercase hex
	Serial       string         `json:"serial"`
	Manufacturer string         `json:"manufacturer"`
	Product      string         `json:"product"`
	Bus          int            `json:"bus"`     // /dev/bus/usb/BBB/DDD
	Address      int            `json:"address"` // /dev/bus/usb/BBB/DDD
	Interfaces   []USBInterface `json:"interfaces"`
	DeviceID     string         `json:"deviceId"` // raw IEEE-1284 string, "" if unreadable
}

// Inventory is what the probe found.
type Inventory struct {
	Devices   []USBDevice `json:"devices"`
	ProbedAt  time.Time   `json:"probedAt"`
	Simulated bool        `json:"simulated"`
}

var deviceIDAliases = map[string]string{
	"MANUFACTURER": "MFG",
	"MODEL":        "MDL",
	"COMMAND SET":  "CMD",
}

// ParseDeviceID splits an IEEE-1284 device ID into its key/value pairs. Keys
// are upper-cased and MANUFACTURER, MODEL and COMMAND SET are stored as MFG,
// MDL and CMD. The first occurrence of a key wins.
func ParseDeviceID(s string) map[string]string {
	out := map[string]string{}
	for _, field := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if alias, ok := deviceIDAliases[k]; ok {
			k = alias
		}
		if _, seen := out[k]; k == "" || seen {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// Decision is the dispatcher's choice for the selected device.
type Decision struct {
	Printer       string     `json:"printer"` // catalog printer family id, "" none
	Scanner       string     `json:"scanner"` // "ipp-usb" | "airsane" | "sim" | ""
	SaneBackends  []string   `json:"saneBackends"`
	RequiredEULAs []string   `json:"requiredEulas"`                // catalog EULA ids
	OptionalEULAs []string   `json:"optionalEulas"`                // catalog EULA ids
	Rule          string     `json:"rule"`                         // name of the matching rule (shown in UI)
	Device        *USBDevice `json:"device" jsonschema:"nullable"` // matched device; nil in simulated mode
}

// BlobStatus is the install state of one vendor blob.
type BlobStatus struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	State   string `json:"state" jsonschema:"enum=installed,enum=missing,enum=failed,enum=disabled"`
	Message string `json:"message"`
}

// Spec is the desired device workload: exactly what the renderer and sim.Apply
// read. A nil *Spec means remove the workload. The field order is part of the
// hash, so append new fields at the end.
type Spec struct {
	Mode         string    `json:"mode"` // ModeUSB | ModeSimulated | ModeProbe
	Printer      string    `json:"printer"`
	Scanner      string    `json:"scanner"`
	SaneBackends []string  `json:"saneBackends"`
	Blobs        []BlobRef `json:"blobs"`
	Name         string    `json:"name"`     // simulated mode only (ippeveprinter / virtual printer name); "" in usb and probe modes
	Location     string    `json:"location"` // simulated mode only (ippeveprinter / virtual printer location); "" in usb and probe modes
	Hash         string    `json:"hash"`     // SpecHash(spec); written to the pod template, so a change rolls the pod
	// AcceptedEULAs are the config's accepted entries, with their text sha256,
	// for the EULAs of Blobs, sorted by id. Accepting a licence changes the hash.
	AcceptedEULAs []EULARef `json:"acceptedEulas"`
}

// SpecHash returns the hex sha256 of the canonical JSON (encoding/json of the
// struct, fields in declaration order) of a copy of *s with Hash cleared and
// every nil slice replaced by an empty one, so nil and [] hash alike. It never
// modifies *s and returns "" for a nil spec. The PAPPL queue is not part of
// Spec, so queue settings never change the hash. BlobRef.Optional is part of
// the JSON, so flipping it changes the hash.
func SpecHash(s *Spec) string {
	if s == nil {
		return ""
	}
	c := *s
	c.Hash = ""
	if c.SaneBackends == nil {
		c.SaneBackends = []string{}
	}
	if c.Blobs == nil {
		c.Blobs = []BlobRef{}
	}
	if c.AcceptedEULAs == nil {
		c.AcceptedEULAs = []EULARef{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		panic("device: marshal spec: " + err.Error()) // only strings, bools and slices of them
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Endpoints are the device's printer and scanner addresses.
type Endpoints struct {
	PrinterURI string `json:"printerUri"`
	ScannerURL string `json:"scannerUrl"`
}

// Observation is what a backend sees of the device workload.
type Observation struct {
	Phase   string `json:"phase"` // PhaseAbsent | PhasePending | PhaseRunning | PhaseFailed
	HostIP  string `json:"hostIp"`
	Message string `json:"message"`
	// Unschedulable is set for pod condition PodScheduled=False with reason
	// Unschedulable (for example no free USB resource).
	Unschedulable bool `json:"unschedulable"`
	// Endpoints: k8s: always empty (derived from agent reports).
	// sim: ScannerURL set whenever the spec has a scanner; PrinterURI set only
	// for the ipp-usb and sim printer families and empty for PAPPL families,
	// whose URI the controller takes from QueueResult.
	Endpoints Endpoints `json:"endpoints"`
}

// Backend runs the device workload.
type Backend interface {
	Apply(ctx context.Context, spec *Spec) error
	Observe(ctx context.Context) (Observation, error)
}

// PodRestarter is an optional Backend capability, found by type assertion.
// The k8s backend implements it; sim does not.
type PodRestarter interface {
	RestartPod(ctx context.Context) error
}

// AgentTokenSource is an optional Backend capability, found by type assertion.
// The k8s backend implements it; sim does not.
type AgentTokenSource interface {
	AgentToken(ctx context.Context) (string, error)
}
