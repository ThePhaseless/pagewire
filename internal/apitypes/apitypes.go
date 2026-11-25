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

	ModeNone = "none" // Status device.mode while the spec is nil; otherwise the spec's device.Mode*

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
	Required              bool              `json:"required"` // true only while the config file is missing
	Backend               string            `json:"backend" jsonschema:"enum=k8s,enum=docker,enum=sim"`
	USBResourceConfigured bool              `json:"usbResourceConfigured"`
	Inventory             *device.Inventory `json:"inventory" jsonschema:"nullable"`
	Decision              *device.Decision  `json:"decision" jsonschema:"nullable"`
	EULAs                 []SetupEULA       `json:"eulas"`
	Suggested             config.Config     `json:"suggested"`
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
	Config    StatusConfig    `json:"config"`
	Device    StatusDevice    `json:"device"`
	Printer   StatusPrinter   `json:"printer"`
	Scanner   StatusScanner   `json:"scanner"`
	Reconcile []ReconcileItem `json:"reconcile"`
	Connect   Connect         `json:"connect"`
}

// StatusConfig is the config file's state.
type StatusConfig struct {
	State       string              `json:"state" jsonschema:"enum=missing,enum=applied,enum=invalid"`
	Etag        string              `json:"etag"`
	AppliedEtag string              `json:"appliedEtag"`
	Errors      []config.FieldError `json:"errors"`
	ChangedAt   time.Time           `json:"changedAt"`
}

// StatusDevice is the device workload's state.
type StatusDevice struct {
	Phase      string              `json:"phase" jsonschema:"enum=none,enum=probing,enum=deploying,enum=ready,enum=degraded,enum=error"`
	Mode       string              `json:"mode" jsonschema:"enum=none,enum=usb,enum=simulated,enum=probe"`
	Decision   *device.Decision    `json:"decision" jsonschema:"nullable"`
	Inventory  *device.Inventory   `json:"inventory" jsonschema:"nullable"`
	Message    string              `json:"message"`
	Advertised []AdvertisedService `json:"advertised"`
	Blobs      []device.BlobStatus `json:"blobs"`
}

// AdvertisedService is one DNS-SD instance the device pod advertises.
type AdvertisedService struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

// StatusPrinter is the printer's state.
type StatusPrinter struct {
	Available bool     `json:"available"`
	URI       string   `json:"uri"`
	State     string   `json:"state" jsonschema:"enum=idle,enum=processing,enum=stopped,enum=unknown"`
	Reasons   []string `json:"reasons"`
	Formats   []string `json:"formats"` // the printer's own document-format-supported
	// ConvertedFormats are the other inputs pagewire converts for this printer.
	ConvertedFormats []FormatConversion `json:"convertedFormats"`
	Media            []string           `json:"media"`
	Markers          []Marker           `json:"markers"`
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

// StatusScanner is the scanner's state.
type StatusScanner struct {
	Available bool   `json:"available"`
	URL       string `json:"url"`
	State     string `json:"state" jsonschema:"enum=idle,enum=processing,enum=stopped,enum=unknown"`
}

// ReconcileItem is the outcome of one reconcile step (device, data-dir, config, ...).
type ReconcileItem struct {
	Item    string    `json:"item"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Connect is what a client needs to add the printer and scanner by hand.
type Connect struct {
	DNSSDName string   `json:"dnssdName"`
	IPPURIs   []string `json:"ippUris"`
	ESCLURLs  []string `json:"esclUrls"`
}

// PrintOptions is the options part of POST /print.
type PrintOptions struct {
	Copies    int    `json:"copies" jsonschema:"minimum=1,maximum=99"`
	Sides     string `json:"sides" jsonschema:"enum=one-sided,enum=two-sided-long-edge,enum=two-sided-short-edge"`
	ColorMode string `json:"colorMode" jsonschema:"enum=auto,enum=color,enum=monochrome"`
	Media     string `json:"media"`
	Quality   string `json:"quality" jsonschema:"enum=draft,enum=normal,enum=high"`
}

// Job is one print job.
type Job struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state" jsonschema:"enum=pending,enum=processing,enum=completed,enum=canceled,enum=aborted,enum=held,enum=stopped"`
	Reasons   []string  `json:"reasons"`
	CreatedAt time.Time `json:"createdAt"`
}

// ScannerCaps is the body of GET /scanner.
type ScannerCaps struct {
	Sources     []string `json:"sources"`
	Resolutions []int    `json:"resolutions"`
	ColorModes  []string `json:"colorModes"`
	ADF         bool     `json:"adf"`
	Duplex      bool     `json:"duplex"`
}

// ScanSettings is the body of POST /scans and Scan.settings.
type ScanSettings struct {
	Source     string `json:"source" jsonschema:"enum=platen,enum=adf,enum=adf-duplex"`
	Resolution int    `json:"resolution" jsonschema:"minimum=75,maximum=1200"`
	ColorMode  string `json:"colorMode" jsonschema:"enum=color,enum=grayscale,enum=blackandwhite"`
	Format     string `json:"format" jsonschema:"enum=pdf,enum=jpeg"`
}

// Scan is one scan job.
type Scan struct {
	ID        string       `json:"id"`
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
