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

// QueueSpec is the PAPPL queue the agent reconciles on every cycle.
type QueueSpec struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	Driver   string `json:"driver"` // PAPPL driver name, or "auto" (needs a device ID from Find-Devices)
	// DeviceURI is an optional full PAPPL device URI as the app lists it, e.g.
	// "cups:ipp://127.0.0.1:18631/ipp/print"; when set the agent skips
	// Find-Devices (used by the integration test).
	DeviceURI string `json:"deviceUri"`
	// Schemes are the preferred CUPS/PAPPL device schemes in order, without the
	// "cups:" prefix, e.g. ["hp","usb"].
	Schemes   []string `json:"schemes"`
	Media     string   `json:"media"`
	Sides     string   `json:"sides"`
	ColorMode string   `json:"colorMode"`
	Quality   string   `json:"quality"`
}

// QueueResult is the agent's last PAPPL reconcile outcome.
type QueueResult struct {
	State      string `json:"state"` // QueueOK | QueuePending | QueueError
	Message    string `json:"message"`
	PrinterURI string `json:"printerUri"` // loopback printer URI the agent used
	DeviceURI  string `json:"deviceUri"`  // chosen device URI
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
	PodUID        string         `json:"podUid"`
	AppliedHash   string         `json:"appliedHash"` // DesiredState.Hash the agent last received
	Inventory     Inventory      `json:"inventory"`
	DevicePresent bool           `json:"devicePresent"`
	Services      []DNSSDService `json:"services"`
	Queue         *QueueResult   `json:"queue"`
	Blobs         []BlobStatus   `json:"blobs"` // in k8s read from --blob-status
	EULAs         []EULAText     `json:"eulas"` // one per DesiredState.StageEULAs ref
}

// DesiredState is the 200 response to an AgentReport (401 bad token, 400 malformed).
type DesiredState struct {
	Hash string `json:"hash"` // the current Spec.Hash ("" when the spec is nil)
	// Queue is built from the current config on every report; nil unless the
	// printer family is a PAPPL app. The agent reconciles it every cycle, so
	// queue changes need no pod roll.
	Queue  *QueueSpec `json:"queue"`
	Browse []string   `json:"browse"` // service types to browse; the controller always sends BrowseTypes
	// StageEULAs are the EULAs whose text the agent stages and reports, sorted
	// by id so an unchanged list compares equal. TextSHA256 is always "".
	StageEULAs []EULARef `json:"stageEulas"`
}

// ReportFunc is the controller's agent-report handler, which the sim backend
// calls in-process, only from its agent goroutine.
type ReportFunc func(ctx context.Context, r AgentReport) (DesiredState, error)
