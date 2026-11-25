// Package pappl manages the queue of a PAPPL printer application through its
// IPP system service (ipp://127.0.0.1:<port>/ipp/system). PAPPL accepts
// Create-Printer and Delete-Printer only from localhost, so the device-pod
// agent runs this code.
package pappl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/OpenPrinting/goipp"

	"github.com/ThePhaseless/pagewire/internal/device"
)

// PAPPL vendor operations, from pappl/base.h at v1.4.12.
const (
	OpFindDevices goipp.Op = 0x402b // IPP_OP_PAPPL_FIND_DEVICES
	OpFindDrivers goipp.Op = 0x402c // IPP_OP_PAPPL_FIND_DRIVERS
)

// PAPPL vendor attributes, from pappl/system-ipp.c (Create-Printer,
// Find-Devices, Find-Drivers) and pappl/printer.c (printer attributes) at v1.4.12.
const (
	AttrDeviceURI  = "smi55357-device-uri"
	AttrDeviceID   = "smi55357-device-id"
	AttrDeviceInfo = "smi55357-device-info"
	AttrDeviceCol  = "smi55357-device-col"
	AttrDeviceType = "smi55357-device-type"
	AttrDriver     = "smi55357-driver"
	AttrDriverInfo = "smi55357-driver-info"
	AttrDriverCol  = "smi55357-driver-col"
)

// DeviceTypeCustomLocal is the smi55357-device-type keyword for
// PAPPL_DEVTYPE_CUSTOM_LOCAL. PAPPL lists the schemes whose registered type
// shares a bit with it: pappl-retrofit registers "cups" (the CUPS backends)
// with PAPPL_DEVTYPE_ALL, so it is listed, while PAPPL's own usb, snmp and
// dnssd schemes are not.
const DeviceTypeCustomLocal = "other-local"

// Qualities maps print-quality keywords to their IPP enum values.
var Qualities = map[string]goipp.Integer{"draft": 3, "normal": 4, "high": 5}

// IPPError is an IPP response with an unsuccessful status.
type IPPError struct {
	Status  goipp.Status
	Message string
}

func (e *IPPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("ipp: %s: %s", e.Status, e.Message)
	}
	return "ipp: " + e.Status.String()
}

// Printer is one queue as Get-Printers reports it.
type Printer struct {
	ID        int
	Name      string
	URI       string // the ipp:// value of printer-uri-supported
	DeviceURI string
	Driver    string // "auto" when it was created with driver auto
	Location  string
	Media     string
	Sides     string
	ColorMode string
	Quality   string // draft | normal | high, "" if unknown
}

// Device is one Find-Devices entry.
type Device struct{ URI, Info, DeviceID string }

// Driver is one Find-Drivers entry.
type Driver struct{ Name, Description string }

// Client talks to one PAPPL system service. It remembers Reconcile's last
// Find-Devices choice per queue, so reuse one Client for the agent's life.
type Client struct {
	uri string

	mu    sync.Mutex
	picks map[string]string // queue key -> device URI; see Reconcile
}

// New returns a client for the system URI, e.g. ipp://127.0.0.1:8001/ipp/system.
// It makes no request.
func New(systemURI string) *Client { return &Client{uri: systemURI} }

// URI returns the system URI the client was made with.
func (c *Client) URI() string { return c.uri }

var requestID atomic.Uint32

func request(op goipp.Op, uriAttr, uri string) *goipp.Message {
	m := goipp.NewRequest(goipp.DefaultVersion, op, requestID.Add(1))
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute(uriAttr, goipp.TagURI, goipp.String(uri)))
	return m
}

func (c *Client) system(op goipp.Op) *goipp.Message { return request(op, "system-uri", c.uri) }

// do posts msg to uri and returns the response; unsuccessful statuses are *IPPError.
func do(ctx context.Context, uri string, msg *goipp.Message) (*goipp.Message, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "ipp" {
		return nil, fmt.Errorf("pappl uri %q: want ipp://host:port/path", uri)
	}
	u.Scheme = "http"
	if u.Port() == "" {
		u.Host += ":631"
	}
	enc, err := msg.EncodeBytes()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(enc))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", goipp.ContentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ipp %s: HTTP %s", goipp.Op(msg.Code), resp.Status)
	}
	rsp := &goipp.Message{}
	if err := rsp.Decode(resp.Body); err != nil {
		return nil, fmt.Errorf("ipp %s: decode response: %w", goipp.Op(msg.Code), err)
	}
	if st := goipp.Status(rsp.Code); st > 0x00ff {
		return nil, &IPPError{Status: st, Message: First(rsp.Operation, "status-message")}
	}
	return rsp, nil
}

var printerAttrs = []string{
	"printer-id", "printer-name", "printer-uri-supported", AttrDeviceURI, AttrDriver, "printer-location",
	"media-default", "sides-default", "print-color-mode-default", "print-quality-default",
}

// Printers lists every queue (Get-Printers).
func (c *Client) Printers(ctx context.Context) ([]Printer, error) {
	m := c.system(goipp.OpGetPrinters)
	m.Operation.Add(keywords("requested-attributes", printerAttrs...))
	rsp, err := do(ctx, c.uri, m)
	if err != nil {
		return nil, fmt.Errorf("get-printers: %w", err)
	}
	var out []Printer
	for _, g := range rsp.AttrGroups() { // one printer group per queue
		if g.Tag != goipp.TagPrinterGroup {
			continue
		}
		a := g.Attrs
		p := Printer{
			Name: First(a, "printer-name"), DeviceURI: First(a, AttrDeviceURI), Driver: First(a, AttrDriver),
			Location: First(a, "printer-location"), Media: First(a, "media-default"), Sides: First(a, "sides-default"),
			ColorMode: First(a, "print-color-mode-default"),
		}
		if id := Ints(a, "printer-id"); len(id) > 0 {
			p.ID = id[0]
		}
		if q := Ints(a, "print-quality-default"); len(q) > 0 {
			for k, v := range Qualities {
				if int(v) == q[0] {
					p.Quality = k
				}
			}
		}
		for _, u := range all(a, "printer-uri-supported") {
			if strings.HasPrefix(u, "ipp://") {
				p.URI = u
				break
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// FindDevices lists the devices the application's CUPS backends can see (PAPPL
// Find-Devices). PAPPL's own usb, dnssd and snmp schemes stay out: the
// retrofit apps print through the CUPS backends only, and a second entry for
// the same printer would compete with the "cups:" one.
func (c *Client) FindDevices(ctx context.Context) ([]Device, error) {
	m := c.system(OpFindDevices)
	m.Operation.Add(goipp.MakeAttribute(AttrDeviceType, goipp.TagKeyword, goipp.String(DeviceTypeCustomLocal)))
	rsp, err := do(ctx, c.uri, m)
	if notFound(err) { // PAPPL answers not-found when it sees no device
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find-devices: %w", err)
	}
	var out []Device
	for _, col := range collections(rsp.System, AttrDeviceCol) {
		out = append(out, Device{URI: First(col, AttrDeviceURI), Info: First(col, AttrDeviceInfo), DeviceID: First(col, AttrDeviceID)})
	}
	return out, nil
}

// FindDrivers lists the drivers matching deviceID, or every driver when
// deviceID is empty (PAPPL Find-Drivers).
func (c *Client) FindDrivers(ctx context.Context, deviceID string) ([]Driver, error) {
	m := c.system(OpFindDrivers)
	if deviceID != "" {
		m.Operation.Add(goipp.MakeAttribute(AttrDeviceID, goipp.TagText, goipp.String(deviceID)))
	}
	rsp, err := do(ctx, c.uri, m)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find-drivers: %w", err)
	}
	var out []Driver
	for _, col := range collections(rsp.System, AttrDriverCol) {
		out = append(out, Driver{Name: First(col, AttrDriver), Description: First(col, AttrDriverInfo)})
	}
	return out, nil
}

// CreatePrinter adds a queue and returns its printer-id. deviceID (optional)
// is sent as printer-device-id so driver "auto" works without PAPPL opening
// the device. Location and defaults are set afterwards with SetDefaults:
// PAPPL applies extra printer attributes only after the queue exists, so a
// rejected one would leave a half-configured queue behind.
func (c *Client) CreatePrinter(ctx context.Context, name, deviceURI, driver, deviceID string) (int, error) {
	m := c.system(goipp.OpCreatePrinter)
	m.Operation.Add(goipp.MakeAttribute("printer-service-type", goipp.TagKeyword, goipp.String("print")))
	m.Operation.Add(goipp.MakeAttribute(AttrDeviceURI, goipp.TagURI, goipp.String(deviceURI)))
	m.Operation.Add(goipp.MakeAttribute(AttrDriver, goipp.TagKeyword, goipp.String(driver)))
	m.Printer.Add(goipp.MakeAttribute("printer-name", goipp.TagName, goipp.String(name)))
	if deviceID != "" {
		m.Printer.Add(goipp.MakeAttribute("printer-device-id", goipp.TagText, goipp.String(deviceID)))
	}
	rsp, err := do(ctx, c.uri, m)
	if err != nil {
		return 0, fmt.Errorf("create-printer %q: %w", name, err)
	}
	id := Ints(rsp.Printer, "printer-id")
	if len(id) == 0 {
		return 0, fmt.Errorf("create-printer %q: response has no printer-id", name)
	}
	return id[0], nil
}

// DeletePrinter removes the queue with printer-id id.
func (c *Client) DeletePrinter(ctx context.Context, id int) error {
	m := c.system(goipp.OpDeletePrinter)
	m.Operation.Add(goipp.MakeAttribute("printer-id", goipp.TagInteger, goipp.Integer(int32(id)))) //nolint:gosec // id came from PAPPL as an IPP integer
	if _, err := do(ctx, c.uri, m); err != nil {
		return fmt.Errorf("delete-printer %d: %w", id, err)
	}
	return nil
}

// SetDefaults sets printer-location and the non-empty job defaults of q on
// the queue at printerURI (Set-Printer-Attributes).
func (c *Client) SetDefaults(ctx context.Context, printerURI string, q device.QueueSpec) error {
	m := request(goipp.OpSetPrinterAttributes, "printer-uri", printerURI)
	m.Printer.Add(goipp.MakeAttribute("printer-location", goipp.TagText, goipp.String(q.Location)))
	for _, kw := range []struct{ name, value string }{
		{"media-default", q.Media}, {"sides-default", q.Sides}, {"print-color-mode-default", q.ColorMode},
	} {
		if kw.value != "" {
			m.Printer.Add(goipp.MakeAttribute(kw.name, goipp.TagKeyword, goipp.String(kw.value)))
		}
	}
	if v, ok := Qualities[q.Quality]; ok {
		m.Printer.Add(goipp.MakeAttribute("print-quality-default", goipp.TagEnum, v))
	}
	if _, err := do(ctx, printerURI, m); err != nil {
		return fmt.Errorf("set-printer-attributes: %w", err)
	}
	return nil
}

func notFound(err error) bool {
	var ie *IPPError
	return errors.As(err, &ie) && ie.Status == goipp.StatusErrorNotFound
}

func collections(attrs goipp.Attributes, name string) []goipp.Attributes {
	var out []goipp.Attributes
	for _, a := range attrs {
		if a.Name != name {
			continue
		}
		for _, v := range a.Values {
			if col, ok := v.V.(goipp.Collection); ok {
				out = append(out, goipp.Attributes(col))
			}
		}
	}
	return out
}

// keywords builds a 1setOf keyword attribute.
func keywords(name string, values ...string) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range values {
		a.Values.Add(goipp.TagKeyword, goipp.String(v))
	}
	return a
}

// all returns every value of the named attribute as strings.
func all(attrs goipp.Attributes, name string) []string {
	var out []string
	for _, a := range attrs {
		if a.Name == name {
			for _, v := range a.Values {
				out = append(out, v.V.String())
			}
		}
	}
	return out
}

// First returns the first value of the named attribute, or "".
func First(attrs goipp.Attributes, name string) string {
	if v := all(attrs, name); len(v) > 0 {
		return v[0]
	}
	return ""
}

// Ints returns the integer and enum values of the named attribute.
func Ints(attrs goipp.Attributes, name string) []int {
	var out []int
	for _, a := range attrs {
		if a.Name == name {
			for _, v := range a.Values {
				if i, ok := v.V.(goipp.Integer); ok {
					out = append(out, int(i))
				}
			}
		}
	}
	return out
}
