// Package catalog holds the embedded device catalog (printer families,
// scanners, EULAs, vendor blobs and dispatch rules), the shared device
// fixtures, and the dispatcher Decide.
package catalog

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/goccy/go-yaml"

	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/device"
)

//go:embed catalog.yaml all:keys fixtures/*.json
var files embed.FS

// Catalog is catalog.yaml.
type Catalog struct {
	APIVersion          string     `yaml:"apiVersion"`
	Printers            []Printer  `yaml:"printers"`
	Scanners            []Scanner  `yaml:"scanners"`
	EULAs               []EULAInfo `yaml:"eulas"`
	Blobs               []Blob     `yaml:"blobs"`
	DefaultSaneBackends []string   `yaml:"defaultSaneBackends"`
	Rules               []Rule     `yaml:"rules"`
}

// Printer is a printer family. PAPPL families have an App.
type Printer struct {
	ID            string   `yaml:"id"`
	Role          string   `yaml:"role"`  // ipp-usb | ippeveprinter, for non-PAPPL families
	Image         string   `yaml:"image"` // driver image name, PAPPL families only
	App           string   `yaml:"app"`   // PAPPL printer application
	Port          int      `yaml:"port"`
	DeviceSchemes []string `yaml:"deviceSchemes"` // QueueSpec.Schemes, PAPPL families only
}

// Scanner is a scanner family.
type Scanner struct {
	ID   string `yaml:"id"`
	Port int    `yaml:"port"`
}

// EULAInfo is the metadata of one vendor licence.
type EULAInfo struct {
	ID      string `yaml:"id"`
	Version string `yaml:"version"`
	Vendor  string `yaml:"vendor"`
	Title   string `yaml:"title"`
	URL     string `yaml:"url"`
	Summary string `yaml:"summary"`
}

// Blob is a proprietary vendor download, fetched only by agent fetch.
type Blob struct {
	ID              string `yaml:"id"`
	EULA            string `yaml:"eula"`
	Enabled         bool   `yaml:"enabled"`
	DisabledMessage string `yaml:"disabledMessage"`
	URL             string `yaml:"url"`
	Verify          Verify `yaml:"verify"`
	Format          string `yaml:"format"`
	MaxBytes        int64  `yaml:"maxBytes"`
}

// Verify says how a blob download is checked.
type Verify struct {
	PGPSignatureURL string `yaml:"pgpSignatureURL"`
	PGPKey          string `yaml:"pgpKey"` // path inside the embedded FS
	SHA256          string `yaml:"sha256"`
}

// Rule maps a matching device to printer and scanner families.
type Rule struct {
	Name          string   `yaml:"name"`
	Match         Match    `yaml:"match"`
	Printer       string   `yaml:"printer"`
	Scanner       string   `yaml:"scanner"`
	SaneBackends  []string `yaml:"saneBackends"` // empty: DefaultSaneBackends
	RequiredEULAs []string `yaml:"requiredEulas"`
	OptionalEULAs []string `yaml:"optionalEulas"`
}

// Match is the AND of the keys present; an empty Match matches every device.
type Match struct {
	VendorID  string     `yaml:"vendorId"`
	Interface *Interface `yaml:"interface"`
	Command   []string   `yaml:"command"`
}

// Interface is a USB interface class triple.
type Interface struct {
	Class    uint8 `yaml:"class"`
	SubClass uint8 `yaml:"subClass"`
	Protocol uint8 `yaml:"protocol"`
}

// Load parses the embedded catalog.yaml. It never reads key files.
func Load() (*Catalog, error) {
	b, err := files.ReadFile("catalog.yaml")
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := yaml.UnmarshalWithOptions(b, &c, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("catalog.yaml: %w", err)
	}
	return &c, nil
}

// The embedded catalog is covered by catalog_test.go, so a parse failure is a
// build defect, not a runtime condition.
var current = sync.OnceValue(func() *Catalog {
	c, err := Load()
	if err != nil {
		panic(err)
	}
	return c
})

// Known lists the EULA versions and printer ids that config.Validate accepts.
func Known() config.Known {
	c := current()
	k := config.Known{EULAs: []config.EULARef{}, Printers: []string{}}
	for _, e := range c.EULAs {
		k.EULAs = append(k.EULAs, config.EULARef{ID: e.ID, Version: e.Version})
	}
	for _, p := range c.Printers {
		k.Printers = append(k.Printers, p.ID)
	}
	return k
}

// Default returns the embedded catalog, parsed once. Callers must treat it as
// read-only.
func Default() *Catalog { return current() }

// Blob returns the blob with that id.
func (c *Catalog) Blob(id string) (Blob, bool) {
	return find(c.Blobs, func(b Blob) bool { return b.ID == id })
}

// Printer returns the printer family with that id.
func (c *Catalog) Printer(id string) (Printer, bool) {
	return find(c.Printers, func(p Printer) bool { return p.ID == id })
}

// EULA returns the EULA with that id.
func (c *Catalog) EULA(id string) (EULAInfo, bool) {
	return find(c.EULAs, func(e EULAInfo) bool { return e.ID == id })
}

// EULA returns the embedded catalog's EULA metadata with that id.
func EULA(id string) (EULAInfo, bool) { return current().EULA(id) }

// BlobKey returns the verification key of an enabled blob from the embedded
// catalog. agent fetch calls it only for enabled blobs.
func BlobKey(id string) (openpgp.EntityList, error) { return current().BlobKey(id) }

// BlobKey reads and parses the armored public key of an enabled blob. A
// disabled blob, or one without verify.pgpKey, is an error and no file is read.
func (c *Catalog) BlobKey(id string) (openpgp.EntityList, error) {
	b, ok := c.Blob(id)
	switch {
	case !ok:
		return nil, fmt.Errorf("catalog: unknown blob %q", id)
	case !b.Enabled:
		return nil, fmt.Errorf("catalog: blob %q is disabled", id)
	case b.Verify.PGPKey == "":
		return nil, fmt.Errorf("catalog: blob %q has no pgpKey", id)
	}
	raw, err := files.ReadFile(b.Verify.PGPKey)
	if err != nil {
		return nil, fmt.Errorf("catalog: blob %q key: %w", id, err)
	}
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("catalog: blob %q key %s: %w", id, b.Verify.PGPKey, err)
	}
	return keys, nil
}

// Fixture returns the embedded device fixture fixtures/<name>.json.
// shortcut: an unknown or malformed name gives an empty inventory (no error),
// as the contract's signature has none; add an error return if callers need to
// reject bad --sim-device values.
func Fixture(name string) device.Inventory {
	inv := device.Inventory{Devices: []device.USBDevice{}}
	b, err := files.ReadFile("fixtures/" + name + ".json")
	if err != nil || json.Unmarshal(b, &inv) != nil {
		return device.Inventory{Devices: []device.USBDevice{}}
	}
	return inv
}

func find[T any](s []T, ok func(T) bool) (T, bool) {
	for _, v := range s {
		if ok(v) {
			return v, true
		}
	}
	var zero T
	return zero, false
}
