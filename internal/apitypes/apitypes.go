// Package apitypes holds the REST and SSE DTOs of /api/v1. They are reflected
// into schema/api.schema.json, from which the UI's Zod types are generated.
//
// Rules: no map fields; slices have no omitempty (the API encoder turns nil
// into []); every pointer is jsonschema:"nullable" without omitempty, and the
// nullable fields are exactly those the REST contract lists.
package apitypes

import (
	"time"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// SSE event names.
const (
	EventStatus = "status" // Status
	EventConfig = "config" // ConfigEvent
	EventJob    = "job"    // Job
	EventScan   = "scan"   // Scan
	EventEULA   = "eula"   // EULA
)

// Status enum values.
const (
	ConfigMissing = "missing"
	ConfigApplied = "applied"
	ConfigInvalid = "invalid"

	PhaseNone      = "none"
	PhaseProbing   = "probing"
	PhaseDeploying = "deploying"
	PhaseReady     = "ready"
	PhaseDegraded  = "degraded"
	PhaseError     = "error"

	ModeNone = "none" // StatusWorkload.mode while the workload has no spec; otherwise the spec's device.Mode*

	StateIdle       = "idle"
	StateProcessing = "processing"
	StateStopped    = "stopped"
	StateUnknown    = "unknown"
)

// ConfigResponse is the body of GET, PUT and PATCH /config and of their 412.
// An invalid file gives Config nil, State invalid and the errors.
type ConfigResponse struct {
	Etag     string              `json:"etag"`
	Config   *config.Config      `json:"config" jsonschema:"nullable"`
	State    string              `json:"state" jsonschema:"enum=applied,enum=invalid"`
	Errors   []config.FieldError `json:"errors"`
	LastGood *config.Config      `json:"lastGood" jsonschema:"nullable"` // last-known-good config, or null
}

// ConfigEvent is the payload of the SSE config event.
type ConfigEvent struct {
	Etag string `json:"etag"`
}

// Setup is the body of GET /setup (wizard data).
type Setup struct {
	Required              bool             `json:"required"` // true only while the config file is missing
	Backend               string           `json:"backend" jsonschema:"enum=k8s,enum=docker,enum=sim"`
	USBResourceConfigured bool             `json:"usbResourceConfigured"`
	Devices               []DetectedDevice `json:"devices"`
	EULAs                 []SetupEULA      `json:"eulas"`
	// Suggested is the config a first run starts from: the name and empty lists.
	Suggested config.Config `json:"suggested"`
}

// DetectedDevice is one USB device seen in any workload's inventory or
// attached list, with what it can be and how it is used.
type DetectedDevice struct {
	Device    device.USBDevice `json:"device"`
	Node      string           `json:"node"` // the workload's node; "" when node-less
	CanPrint  bool             `json:"canPrint"`
	CanScan   bool             `json:"canScan"`
	PrinterID string           `json:"printerId"`                                      // the configured printer entry bound to it, "" none
	ScannerID string           `json:"scannerId"`                                      // the configured scanner entry bound to it, "" none
	Ignored   []string         `json:"ignored" jsonschema:"enum=printer,enum=scanner"` // roles listed in ignoredDevices
	// SuggestedPrinter and SuggestedScanner are the entries the wizard adds
	// for this device; null when it cannot take that role.
	SuggestedPrinter *config.Printer         `json:"suggestedPrinter" jsonschema:"nullable"`
	SuggestedScanner *config.Scanner         `json:"suggestedScanner" jsonschema:"nullable"`
	PrinterDecision  *device.PrinterDecision `json:"printerDecision" jsonschema:"nullable"`
	ScannerDecision  *device.ScannerDecision `json:"scannerDecision" jsonschema:"nullable"`
}

// EULA text states: pending until the device agent has staged the vendor
// archive and reported the text, then available or failed.
const (
	EULAPending   = "pending"
	EULAAvailable = "available"
	EULAFailed    = "failed"
)

// SetupEULA is one EULA the wizard shows. Its text is GET /eulas/{id}.
type SetupEULA struct {
	ID       string `json:"id"`
	Version  string `json:"version"`
	Title    string `json:"title"`
	Vendor   string `json:"vendor"`
	Summary  string `json:"summary"`
	Required bool   `json:"required"`
	State    string `json:"state" jsonschema:"enum=pending,enum=available,enum=failed"`
	// TextSHA256 is the lowercase hex sha256 of the current licence text, the
	// value an acceptance records; null unless state is available.
	TextSHA256 *string `json:"textSha256" jsonschema:"nullable"`
}

// EULA is the body of GET /eulas/{id}: the licence text read from the
// verified vendor archive. Text and TextSHA256 are null unless state is
// available; Message says why when state is failed.
type EULA struct {
	ID         string  `json:"id"`
	Version    string  `json:"version"`
	Title      string  `json:"title"`
	Vendor     string  `json:"vendor"`
	Summary    string  `json:"summary"`
	State      string  `json:"state" jsonschema:"enum=pending,enum=available,enum=failed"`
	Message    string  `json:"message"`
	TextSHA256 *string `json:"textSha256" jsonschema:"nullable"`
	Text       *string `json:"text" jsonschema:"nullable"`
}

// Status is the body of GET /status and the SSE status event.
type Status struct {
	Config    StatusConfig     `json:"config"`
	Phase     string           `json:"phase" jsonschema:"enum=none,enum=probing,enum=deploying,enum=ready,enum=degraded,enum=error"`
	Message   string           `json:"message"`
	Printers  []StatusPrinter  `json:"printers"`
	Scanners  []StatusScanner  `json:"scanners"`
	Workloads []StatusWorkload `json:"workloads"`
	Devices   []DetectedDevice `json:"devices"`
	Reconcile []ReconcileItem  `json:"reconcile"`
}

// StatusConfig is the config file's state.
type StatusConfig struct {
	State       string              `json:"state" jsonschema:"enum=missing,enum=applied,enum=invalid"`
	Etag        string              `json:"etag"`
	AppliedEtag string              `json:"appliedEtag"`
	Errors      []config.FieldError `json:"errors"`
	ChangedAt   time.Time           `json:"changedAt"`
}

// StatusWorkload is one device workload's state.
type StatusWorkload struct {
	Node       string              `json:"node"` // "" for the node-less workload
	Phase      string              `json:"phase" jsonschema:"enum=absent,enum=pending,enum=running,enum=failed"`
	Mode       string              `json:"mode" jsonschema:"enum=none,enum=usb,enum=simulated,enum=probe"`
	Message    string              `json:"message"`
	Advertised []AdvertisedService `json:"advertised"`
	Blobs      []device.BlobStatus `json:"blobs"`
}

// AdvertisedService is one DNS-SD instance a device workload advertises.
type AdvertisedService struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

// Warning is one configured or requested option the device does not apply.
// Value is the wanted value and Using what the device uses instead (its
// default, "" when it has none); both are raw keywords or decimal strings, for
// the UI to label. Message is English text for logs and for option other.
type Warning struct {
	Option  string `json:"option" jsonschema:"enum=media,enum=mediaSource,enum=mediaType,enum=sides,enum=colorMode,enum=quality,enum=resolution,enum=contentOptimize,enum=orientation,enum=numberUp,enum=printScaling,enum=outputBin,enum=finishings,enum=copies,enum=source,enum=intent,enum=format,enum=other"`
	Value   string `json:"value"`
	Using   string `json:"using"`
	Message string `json:"message"`
}

// StatusPrinter is one printer entry's state.
type StatusPrinter struct {
	ID        string                  `json:"id"`
	Name      string                  `json:"name"`
	Location  string                  `json:"location"`
	Enabled   bool                    `json:"enabled"`
	Phase     string                  `json:"phase" jsonschema:"enum=none,enum=probing,enum=deploying,enum=ready,enum=degraded,enum=error"`
	Message   string                  `json:"message"`
	Warnings  []Warning               `json:"warnings"`
	Decision  *device.PrinterDecision `json:"decision" jsonschema:"nullable"`
	Device    *device.USBDevice       `json:"device" jsonschema:"nullable"` // bound device; null when unbound or simulated
	Available bool                    `json:"available"`
	URI       string                  `json:"uri"`
	State     string                  `json:"state" jsonschema:"enum=idle,enum=processing,enum=stopped,enum=unknown"`
	Reasons   []string                `json:"reasons"`
	Formats   []string                `json:"formats"` // the printer's own document-format-supported
	// ConvertedFormats are the other inputs pagewire converts for this printer.
	ConvertedFormats []FormatConversion `json:"convertedFormats"`
	Media            []string           `json:"media"`
	Markers          []Marker           `json:"markers"`
	Capabilities     device.PrinterCaps `json:"capabilities"`
	Connect          PrinterConnect     `json:"connect"`
}

// FormatConversion is one upload format pagewire converts before printing.
type FormatConversion struct {
	From string `json:"from"` // detected MIME type, e.g. image/png
	To   string `json:"to"`   // MIME type sent to the printer
}

// Marker is one supply level (IPP marker-*).
type Marker struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Level int    `json:"level"` // 0..100, or IPP's negative "unknown" values
}

// StatusScanner is one scanner entry's state.
type StatusScanner struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Location     string                  `json:"location"`
	Enabled      bool                    `json:"enabled"`
	Phase        string                  `json:"phase" jsonschema:"enum=none,enum=probing,enum=deploying,enum=ready,enum=degraded,enum=error"`
	Message      string                  `json:"message"`
	Warnings     []Warning               `json:"warnings"`
	Decision     *device.ScannerDecision `json:"decision" jsonschema:"nullable"`
	Device       *device.USBDevice       `json:"device" jsonschema:"nullable"` // bound device; null when unbound or simulated
	Available    bool                    `json:"available"`
	URL          string                  `json:"url"`
	State        string                  `json:"state" jsonschema:"enum=idle,enum=processing,enum=stopped,enum=unknown"`
	Capabilities ScannerCaps             `json:"capabilities"`
	Connect      ScannerConnect          `json:"connect"`
}

// ReconcileItem is the outcome of one reconcile step (device, data-dir, config, ...).
type ReconcileItem struct {
	Item    string    `json:"item"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// PrinterConnect is what a client needs to add one printer by hand.
type PrinterConnect struct {
	DNSSDName string   `json:"dnssdName"` // the matched DNS-SD instance name, "" none
	IPPURIs   []string `json:"ippUris"`
}

// ScannerConnect is what a client needs to add one scanner by hand.
type ScannerConnect struct {
	DNSSDName string   `json:"dnssdName"` // the matched DNS-SD instance name, "" none
	ESCLURLs  []string `json:"esclUrls"`
}

// PrintOptions is the options part of POST /printers/{id}/print. Every field
// is optional: an absent one means the printer's default. The embed carries no
// tag, so JSON and the schema inline its fields.
type PrintOptions struct {
	Copies int `json:"copies,omitempty" jsonschema:"minimum=0,maximum=99"` // 0 or absent: 1
	device.PrintSettings
}

// PrintResult is the body of the POST /printers/{id}/print response. Warnings
// are the options left out because the printer does not offer them.
type PrintResult struct {
	Job      Job       `json:"job"`
	Warnings []Warning `json:"warnings"`
}

// Job is one print job.
type Job struct {
	ID        int       `json:"id"`
	Printer   string    `json:"printer"` // printer entry id
	Name      string    `json:"name"`
	State     string    `json:"state" jsonschema:"enum=pending,enum=processing,enum=completed,enum=canceled,enum=aborted,enum=held,enum=stopped"`
	Reasons   []string  `json:"reasons"`
	CreatedAt time.Time `json:"createdAt"`
}

// ScannerCaps is the body of GET /scanners/{id}/capabilities.
type ScannerCaps struct {
	Sources     []string `json:"sources"`
	Resolutions []int    `json:"resolutions"`
	ColorModes  []string `json:"colorModes"`
	ADF         bool     `json:"adf"`
	Duplex      bool     `json:"duplex"`
	Intents     []string `json:"intents"` // intent keywords, in eSCL order
}

// ScanSettings is the body of POST /scans and Scan.settings.
type ScanSettings struct {
	Scanner    string `json:"scanner"` // scanner entry id
	Source     string `json:"source" jsonschema:"enum=platen,enum=adf,enum=adf-duplex"`
	Resolution int    `json:"resolution" jsonschema:"minimum=75,maximum=1200"`
	ColorMode  string `json:"colorMode" jsonschema:"enum=color,enum=grayscale,enum=blackandwhite"`
	Format     string `json:"format" jsonschema:"enum=pdf,enum=jpeg"`
	Intent     string `json:"intent,omitempty" jsonschema:"enum=document,enum=text-and-graphic,enum=photo,enum=preview,enum=object,enum=business-card"`
}

// Scan is one scan job.
type Scan struct {
	ID        string       `json:"id"`
	Scanner   string       `json:"scanner"` // scanner entry id
	State     string       `json:"state" jsonschema:"enum=scanning,enum=completed,enum=failed,enum=canceled"`
	CreatedAt time.Time    `json:"createdAt"`
	Pages     int          `json:"pages"`
	Format    string       `json:"format" jsonschema:"enum=pdf,enum=jpeg"`
	Settings  ScanSettings `json:"settings"`
	Error     *string      `json:"error" jsonschema:"nullable"` // set when State is failed
}

// Problem is an RFC 9457 application/problem+json body. A 415 lists the
// accepted formats in Detail.
type Problem struct {
	Type   string              `json:"type"` // short problem type, e.g. storage-error, config-missing, multipart-order
	Title  string              `json:"title"`
	Status int                 `json:"status"`
	Detail string              `json:"detail"`
	Errors []config.FieldError `json:"errors"`
}
