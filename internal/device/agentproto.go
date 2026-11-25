package device

import "context"

// Agent protocol constants.
const (
	// AgentReportPath is where the agent POSTs its AgentReport (internal listener only).
	AgentReportPath = "/internal/v1/agent/report"
	// TokenScheme is the Authorization scheme: "Authorization: Bearer <token>".
	TokenScheme = "Bearer"

	InventoryFile  = "/run/pagewire/inventory.json"
	BlobStatusFile = "/run/pagewire/blobs.json"
	AgentTokenFile = "/var/run/pagewire/token" //nolint:gosec // a file path, not a credential
	DBusAddress    = "unix:path=/run/dbus/system_bus_socket"

	ServiceIPP   = "_ipp._tcp"
	ServiceUScan = "_uscan._tcp"

	// AgentWorkloadArg is the agent flag that names its workload (Spec.Node).
	AgentWorkloadArg = "--workload"
)

// BrowseTypes are the DNS-SD service types the controller always asks for.
var BrowseTypes = []string{ServiceIPP, ServiceUScan}

// QueueResult states.
const (
	QueueOK      = "ok"
	QueuePending = "pending"
	QueueError   = "error"
)

// BlobRef names a blob the device pod needs.
type BlobRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Optional is true when the blob's EULA is in the decision's OptionalEULAs;
	// rendered as --optional <id>.
	Optional bool `json:"optional"`
}

// EULAText states.
const (
	EULAPending   = "pending"
	EULAAvailable = "available"
	EULAFailed    = "failed"
)

// EULARef names one EULA version. TextSHA256 is the lowercase hex sha256 of
// the accepted licence text, or "" where no acceptance is meant (staging).
type EULARef struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	TextSHA256 string `json:"textSha256"`
}

// EULAText is the agent's staging result for one EULA: the licence text read
// from the verified vendor archive. Text and TextSHA256 are set only when
// State is EULAAvailable.
type EULAText struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	TextSHA256 string `json:"textSha256"`
	Text       string `json:"text"`
	State      string `json:"state"` // EULAPending | EULAAvailable | EULAFailed
	Message    string `json:"message"`
}

// QueueSpec is one PAPPL queue the agent reconciles on every cycle.
type QueueSpec struct {
	ID       string `json:"id"` // config printer entry id
	Name     string `json:"name"`
	Location string `json:"location"`
	Driver   string `json:"driver"` // PAPPL driver name, or "auto" (needs a device ID from Find-Devices)
	// DriverPinned means the user chose Driver (driverName). An unpinned name
	// comes from the driver index, which can list a driver the running app
	// lacks, so the agent then falls back to the app's own choice.
	DriverPinned bool `json:"driverPinned"`
	// DeviceURI is an optional full PAPPL device URI as the app lists it, e.g.
	// "cups:ipp://127.0.0.1:18631/ipp/print"; when set the agent skips
	// Find-Devices (used by the integration test).
	DeviceURI string `json:"deviceUri"`
	// Schemes are the preferred CUPS/PAPPL device schemes in order, without the
	// "cups:" prefix, e.g. ["hp","usb"].
	Schemes []string `json:"schemes"`
	// SystemURI is the printer application's system URI,
	// PAPPLSystemURI(family port). The agent may translate it through
	// agent.Core.SystemURL (the sim does) before connecting.
	SystemURI string `json:"systemUri"`
	// Device is the USB device the queue must use. Its DeviceID must be set
	// (the IEEE-1284 string): the agent sends it when it creates or recreates
	// a queue.
	Device   USBDevice     `json:"device"`
	Defaults PrintSettings `json:"defaults"` // the entry's defaults; the agent sets only what the queue supports
}

// QueueResult is the agent's last reconcile outcome for one QueueSpec.
type QueueResult struct {
	ID         string   `json:"id"`    // QueueSpec.ID
	State      string   `json:"state"` // QueueOK | QueuePending | QueueError
	Message    string   `json:"message"`
	PrinterURI string   `json:"printerUri"` // loopback printer URI the agent used
	DeviceURI  string   `json:"deviceUri"`  // chosen device URI
	Warnings   []string `json:"warnings"`   // with QueueOK, e.g. the defaults could not be set
	// Refused are the defaults the queue refused in Set-Printer-Attributes,
	// by option kind; Using is the queue's own default. Not resent until the
	// wanted value or the queue changes.
	Refused []Dropped `json:"refused,omitempty"`
	// Unreachable is set when the printer application refused the connection
	// (still starting).
	Unreachable bool `json:"unreachable"`
}

// DNSSDService is one browsed and resolved DNS-SD instance.
type DNSSDService struct {
	Type string            `json:"type"` // ServiceIPP | ServiceUScan
	Name string            `json:"name"`
	Host string            `json:"host"`
	Port int               `json:"port"`
	TXT  map[string]string `json:"txt"`
}

// AgentReport is the body of POST AgentReportPath with
// "Authorization: Bearer <token>".
type AgentReport struct {
	PodUID      string    `json:"podUid"`
	Workload    string    `json:"workload"`    // the agent's --workload (Spec.Node)
	AppliedHash string    `json:"appliedHash"` // DesiredState.Hash the agent last received
	Inventory   Inventory `json:"inventory"`   // the static probe result
	// Attached is this cycle's sysfs device list (DeviceID empty); nil when
	// not reported (old agents, no inventory file).
	Attached []USBDevice `json:"attached"`
	// ProbeNodes are the usbfs nodes (USBDevice.Node form) the probe found
	// under /dev/bus/usb; nil when unknown, [] when there were none.
	ProbeNodes []string       `json:"probeNodes"`
	Services   []DNSSDService `json:"services"`
	Queues     []QueueResult  `json:"queues"` // one per DesiredState.Queues entry, in order
	Blobs      []BlobStatus   `json:"blobs"`  // in k8s read from --blob-status
	EULAs      []EULAText     `json:"eulas"`  // one per DesiredState.StageEULAs ref
}

// DesiredState is the 200 response to an AgentReport (401 bad token, 400 malformed).
type DesiredState struct {
	Hash string `json:"hash"` // the Spec.Hash of the report's workload ("" when it has no spec)
	// Queues are built from the current config on every report: one per PAPPL
	// printer of the workload's spec. The agent reconciles them every cycle,
	// so queue changes need no pod roll.
	Queues []QueueSpec `json:"queues"`
	Browse []string    `json:"browse"` // service types to browse; the controller always sends BrowseTypes
	// StageEULAs are the EULAs whose text the agent stages and reports, sorted
	// by id so an unchanged list compares equal. TextSHA256 is always "".
	StageEULAs []EULARef `json:"stageEulas"`
}

// ReportFunc is the controller's agent-report handler, which the sim backend
// calls in-process, only from its agent goroutine.
type ReportFunc func(ctx context.Context, r AgentReport) (DesiredState, error)
