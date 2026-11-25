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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Spec modes.
const (
	ModeUSB       = "usb"       // at least one USB device is served
	ModeSimulated = "simulated" // only simulated devices are served
	ModeProbe     = "probe"     // probe + agent only, feeds the wizard
)

// PAPPLSystemURI is the system URI of the printer application listening on
// port: the one builder of it, so the controller's QueueSpec.SystemURI and the
// sim's mapping of it match exactly.
func PAPPLSystemURI(port int) string {
	return "ipp://127.0.0.1:" + strconv.Itoa(port) + "/ipp/system"
}

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

// Node is the device's usbfs node, "BBB/DDD" (/dev/bus/usb/BBB/DDD).
func (d USBDevice) Node() string { return fmt.Sprintf("%03d/%03d", d.Bus, d.Address) }

// Key is the device's identity, without its bus address.
func (d USBDevice) Key() USBKey {
	return USBKey{VendorID: d.VendorID, ProductID: d.ProductID, Serial: d.Serial}
}

// USBKey identifies a USB device by its descriptors. Bus and address are not
// part of it: they change on replug.
type USBKey struct {
	VendorID  string `json:"vendorId"`
	ProductID string `json:"productId"`
	Serial    string `json:"serial"`
}

// String is "vendor:product:serial".
func (k USBKey) String() string { return k.VendorID + ":" + k.ProductID + ":" + k.Serial }

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

// Decision rules: how the dispatcher chose a printer (PrinterDecision.Rule)
// or a scanner (ScannerDecision.Rule).
const (
	RuleIPPOverUSB = "ipp-over-usb" // the device has an IPP-over-USB interface (7/1/4); printer and scanner
	RuleModel      = "model"        // the driver index lists the device's model
	RuleCommandSet = "command-set"  // a generic driver for a language in the device's CMD
	RuleSANEIndex  = "sane-index"   // scanner: the SANE USB index lists the device's vendor:product
	RuleStillImage = "still-image"  // scanner: the device has a class-6 (still image) interface
	RuleOverride   = "override"     // driver, driverName or backend in the config entry
	RuleNone       = "none"         // printer: no driver matches; no printer runs
	RuleSimulated  = "simulated"
)

// PrinterDecision is the dispatcher's choice for one printer entry.
type PrinterDecision struct {
	Family            string     `json:"family"`            // catalog printer family id, "" none
	Driver            string     `json:"driver"`            // PAPPL driver name, "" none (the queue then uses auto)
	DriverDescription string     `json:"driverDescription"` // the driver's description from the index, "" none
	RequiredEULAs     []string   `json:"requiredEulas"`     // catalog EULA ids
	OptionalEULAs     []string   `json:"optionalEulas"`     // catalog EULA ids
	Rule              string     `json:"rule" jsonschema:"enum=ipp-over-usb,enum=model,enum=command-set,enum=override,enum=none,enum=simulated"`
	Device            *USBDevice `json:"device" jsonschema:"nullable"` // bound device; nil for simulated entries
}

// ScannerDecision is the dispatcher's choice for one scanner entry.
type ScannerDecision struct {
	Family       string     `json:"family"` // "ipp-usb" | "airsane" | "sim"
	SaneBackends []string   `json:"saneBackends"`
	Rule         string     `json:"rule" jsonschema:"enum=ipp-over-usb,enum=sane-index,enum=still-image,enum=override,enum=simulated"`
	Device       *USBDevice `json:"device" jsonschema:"nullable"` // bound device; nil for simulated entries
}

// BlobStatus is the install state of one vendor blob.
type BlobStatus struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	State   string `json:"state" jsonschema:"enum=installed,enum=missing,enum=failed,enum=disabled"`
	Message string `json:"message"`
}

// Spec is one desired device workload: exactly what the renderer and sim.Apply
// read. The field order is part of the hash. The controller sorts Printers and
// Scanners by ID, so the hash is stable.
type Spec struct {
	Mode     string        `json:"mode"` // ModeUSB | ModeSimulated | ModeProbe
	Node     string        `json:"node"` // the node the workload is pinned to; "" in single-node, docker and sim
	Printers []PrinterSpec `json:"printers"`
	Scanners []ScannerSpec `json:"scanners"`
	Blobs    []BlobRef     `json:"blobs"`
	Hash     string        `json:"hash"` // SpecHash(spec); written to the pod template, so a change rolls the pod
	// AcceptedEULAs are the config's accepted entries, with their text sha256,
	// for the EULAs of Blobs, sorted by id. Accepting a licence changes the hash.
	AcceptedEULAs []EULARef `json:"acceptedEulas"`
}

// PrinterSpec is one printer the workload serves. The PAPPL driver name is
// not part of it (it travels in QueueSpec), so a driver change rolls no pod.
type PrinterSpec struct {
	ID       string `json:"id"`
	Family   string `json:"family"`   // catalog printer family id
	Name     string `json:"name"`     // sim family only (ippeveprinter / virtual printer name)
	Location string `json:"location"` // sim family only
	Port     int    `json:"port"`     // sim family only
	USB      USBKey `json:"usb"`      // zero for simulated printers
}

// ScannerSpec is one scanner the workload serves.
type ScannerSpec struct {
	ID           string   `json:"id"`
	Family       string   `json:"family"` // "ipp-usb" | "airsane" | "sim"
	SaneBackends []string `json:"saneBackends"`
	USB          USBKey   `json:"usb"` // zero for the simulated scanner
}

// SpecHash returns the hex sha256 of the canonical JSON (encoding/json of the
// struct, fields in declaration order) of a copy of *s with Hash cleared and
// every nil slice, nested ones included, replaced by an empty one, so nil and
// [] hash alike. It never modifies *s and returns "" for a nil spec. The PAPPL
// queues are not part of Spec, so queue settings never change the hash.
// BlobRef.Optional is part of the JSON, so flipping it changes the hash.
func SpecHash(s *Spec) string {
	if s == nil {
		return ""
	}
	c := *s
	c.Hash = ""
	if c.Printers == nil {
		c.Printers = []PrinterSpec{}
	}
	c.Scanners = slices.Clone(c.Scanners) // copy before normalising the nested lists
	if c.Scanners == nil {
		c.Scanners = []ScannerSpec{}
	}
	for i := range c.Scanners {
		if c.Scanners[i].SaneBackends == nil {
			c.Scanners[i].SaneBackends = []string{}
		}
	}
	if c.Blobs == nil {
		c.Blobs = []BlobRef{}
	}
	if c.AcceptedEULAs == nil {
		c.AcceptedEULAs = []EULARef{}
	}
	b, err := json.Marshal(c)
	if err != nil {
		panic("device: marshal spec: " + err.Error()) // only strings, ints, bools and slices of them
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Endpoint is one entry's printer or scanner address.
type Endpoint struct {
	ID         string `json:"id"` // config entry id
	PrinterURI string `json:"printerUri"`
	ScannerURL string `json:"scannerUrl"`
}

// Observation is what a backend sees of one device workload.
type Observation struct {
	Workload string `json:"workload"` // Spec.Node of the workload; "" for the node-less one
	Phase    string `json:"phase"`    // PhaseAbsent | PhasePending | PhaseRunning | PhaseFailed
	HostIP   string `json:"hostIp"`
	Message  string `json:"message"`
	// Unschedulable is set for pod condition PodScheduled=False with reason
	// Unschedulable (for example no free USB resource); docker: no USB printer
	// or scanner on the host.
	Unschedulable bool `json:"unschedulable"`
	// Endpoints: k8s and docker: always empty (derived from agent reports).
	// sim: one per served entry by ID; PrinterURI is set only for the ipp-usb
	// and sim printer families and empty for PAPPL families, whose URI the
	// controller takes from QueueResult.
	Endpoints []Endpoint `json:"endpoints"`
}

// Backend runs the device workloads.
type Backend interface {
	// Apply makes the running workloads exactly specs; empty specs remove
	// them all. Either every spec has Node "" (one spec) or every spec names a
	// node.
	Apply(ctx context.Context, specs []*Spec) error
	// Observe returns one Observation per known workload, or one PhaseAbsent
	// Observation for Workload "" when there is none.
	Observe(ctx context.Context) ([]Observation, error)
}

// PodRestarter is an optional Backend capability, found by type assertion.
// The k8s and docker backends implement it; sim does not.
type PodRestarter interface {
	// RestartPod restarts the workload named workload ("" is the node-less one).
	RestartPod(ctx context.Context, workload string) error
}

// NodeLister is an optional Backend capability, found by type assertion. The
// k8s backend implements it.
type NodeLister interface {
	// USBNodes returns, sorted, the nodes that advertise the USB resource. An
	// error wrapping ErrSingleNode means single-node mode (the nodes cannot be
	// listed at all); any other error is transient, and the caller keeps its
	// previous workload set and applies nothing until a list succeeds.
	USBNodes(ctx context.Context) ([]string, error)
}

// WorkloadErrors is an Apply error that failed only the workloads it names
// (Spec.Node, "" for the node-less one); every other spec was applied. Any
// other Apply error fails every workload.
type WorkloadErrors map[string]error

func (e WorkloadErrors) Error() string {
	msgs := make([]string, 0, len(e))
	for _, w := range slices.Sorted(maps.Keys(e)) {
		msgs = append(msgs, e[w].Error())
	}
	return strings.Join(msgs, "; ")
}

// Unwrap lets errors.Is and errors.As see the workloads' errors.
func (e WorkloadErrors) Unwrap() []error {
	errs := make([]error, 0, len(e))
	for _, w := range slices.Sorted(maps.Keys(e)) {
		errs = append(errs, e[w])
	}
	return errs
}

// ErrSingleNode is wrapped by a NodeLister error that means single-node mode.
var ErrSingleNode = errors.New("single-node mode")

// AgentTokenSource is an optional Backend capability, found by type assertion.
// The k8s and docker backends implement it; sim does not.
type AgentTokenSource interface {
	AgentToken(ctx context.Context) (string, error)
}

// USBDeviceSetter is an optional Backend capability, found by type assertion.
// Every step that may Apply, the controller passes every bound USB device with
// its current address; the docker backend gives each daemon the nodes of the
// devices it serves, and no others. With beforeApply the backend holds devs
// until the next successful Apply takes them with the new spec, so the worker
// never pairs the old workload with the new devices; without, it takes them now.
type USBDeviceSetter interface {
	SetUSBDevices(devs []USBDevice, beforeApply bool)
}

// Option kinds: the user-facing options pagewire reads from devices. Printer
// and scanner share resolution and colorMode; the device role decides which
// caps field, enum and range apply. OptionOther marks a warning that is not
// about an option.
const (
	OptionMedia           = "media"
	OptionMediaSource     = "mediaSource"
	OptionMediaType       = "mediaType"
	OptionSides           = "sides"
	OptionColorMode       = "colorMode"
	OptionQuality         = "quality"
	OptionResolution      = "resolution"
	OptionContentOptimize = "contentOptimize"
	OptionOrientation     = "orientation"
	OptionNumberUp        = "numberUp"
	OptionPrintScaling    = "printScaling"
	OptionOutputBin       = "outputBin"
	OptionFinishings      = "finishings"
	OptionCopies          = "copies" // per job only, no config key
	OptionSource          = "source"
	OptionIntent          = "intent"
	OptionFormat          = "format"
	OptionOther           = "other"
)

// OptionKinds lists every option kind, OptionOther last. apitypes.Warning's
// option enum must equal it (apitypes_test.go checks).
var OptionKinds = []string{
	OptionMedia, OptionMediaSource, OptionMediaType, OptionSides, OptionColorMode, OptionQuality,
	OptionResolution, OptionContentOptimize, OptionOrientation, OptionNumberUp, OptionPrintScaling,
	OptionOutputBin, OptionFinishings, OptionCopies, OptionSource, OptionIntent, OptionFormat, OptionOther,
}

// PrintSettings are the print option values of one job or queue; "" or 0
// means the printer's own default. It has exactly config.PrintDefaults's field
// names, order and types, because callers convert with
// device.PrintSettings(entry.Defaults) (apitypes_test.go guards it). The name
// differs from config.PrintDefaults on purpose: both are reachable from the API
// schema root, whose $defs are named after Go types (TestUniqueDefNames).
type PrintSettings struct {
	Media           string `json:"media,omitempty" jsonschema:"pattern=^[a-z0-9-]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)$"`
	MediaSource     string `json:"mediaSource,omitempty" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$"`
	MediaType       string `json:"mediaType,omitempty" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$"`
	Sides           string `json:"sides,omitempty" jsonschema:"enum=one-sided,enum=two-sided-long-edge,enum=two-sided-short-edge"`
	ColorMode       string `json:"colorMode,omitempty" jsonschema:"enum=auto,enum=color,enum=monochrome"`
	Quality         string `json:"quality,omitempty" jsonschema:"enum=draft,enum=normal,enum=high"`
	Resolution      int    `json:"resolution,omitempty" jsonschema:"minimum=75,maximum=2400"` // dpi
	ContentOptimize string `json:"contentOptimize,omitempty" jsonschema:"enum=auto,enum=graphic,enum=photo,enum=text,enum=text-and-graphic"`
	Orientation     string `json:"orientation,omitempty" jsonschema:"enum=portrait,enum=landscape,enum=reverse-landscape,enum=reverse-portrait"`
	NumberUp        int    `json:"numberUp,omitempty" jsonschema:"minimum=1,maximum=16"` // pages per sheet
	PrintScaling    string `json:"printScaling,omitempty" jsonschema:"enum=auto,enum=auto-fit,enum=fill,enum=fit,enum=none"`
	OutputBin       string `json:"outputBin,omitempty" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$"`
	Finishings      string `json:"finishings,omitempty" jsonschema:"pattern=^[a-z0-9][a-z0-9._-]{0\\,254}$"` // one IANA finishings keyword
}

// PrinterCaps are the print options a printer reports (Get-Printer-Attributes
// *-supported and *-default). An empty list means the kind is not reported; a
// "" or 0 default means none is reported. Values are raw IPP keywords (quality,
// orientation and finishings enums mapped to keywords), never restricted to
// pagewire's enums, so the fields carry no jsonschema enum or pattern.
type PrinterCaps struct {
	Media           []string `json:"media"`
	MediaSources    []string `json:"mediaSources"`
	MediaTypes      []string `json:"mediaTypes"`
	Sides           []string `json:"sides"`
	ColorModes      []string `json:"colorModes"`
	Qualities       []string `json:"qualities"`
	ContentOptimize []string `json:"contentOptimize"`
	Orientations    []string `json:"orientations"`
	PrintScalings   []string `json:"printScalings"`
	OutputBins      []string `json:"outputBins"`
	Finishings      []string `json:"finishings"`
	Formats         []string `json:"formats"` // document-format-supported
	Resolutions     []int    `json:"resolutions"`
	NumberUp        []int    `json:"numberUp"`
	CopiesMax       int      `json:"copiesMax"` // upper bound of copies-supported; 0 not reported

	MediaDefault           string `json:"mediaDefault"`
	MediaSourceDefault     string `json:"mediaSourceDefault"` // media-col-default.media-source
	MediaTypeDefault       string `json:"mediaTypeDefault"`   // media-col-default.media-type
	SidesDefault           string `json:"sidesDefault"`
	ColorModeDefault       string `json:"colorModeDefault"`
	QualityDefault         string `json:"qualityDefault"`
	ContentOptimizeDefault string `json:"contentOptimizeDefault"`
	OrientationDefault     string `json:"orientationDefault"`
	PrintScalingDefault    string `json:"printScalingDefault"`
	OutputBinDefault       string `json:"outputBinDefault"`
	FinishingsDefault      string `json:"finishingsDefault"`
	ResolutionDefault      int    `json:"resolutionDefault"`
	NumberUpDefault        int    `json:"numberUpDefault"`
}
