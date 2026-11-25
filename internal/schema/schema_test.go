package schema

// Recorded from the generated files:
//   - invopop/jsonschema v0.14.0 writes a jsonschema:"nullable" field as
//     {"oneOf": [<the field's schema, e.g. {"$ref": "#/$defs/Config"}>, {"type": "null"}]}.
//     It never emits anyOf or type: [x, "null"] for these DTOs.
//   - It reflects time.Time as {"type": "string", "format": "date-time"}.
//   - The default Reflector requires every field without omitempty;
//     RequiredFromJSONSchemaTags requires only fields tagged jsonschema:"required".

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

type obj = map[string]any

func decode(t *testing.T, b []byte) obj {
	t.Helper()
	var m obj
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// def returns $defs[name], failing when it is missing.
func def(t *testing.T, s obj, name string) obj {
	t.Helper()
	d, ok := s["$defs"].(obj)[name].(obj)
	if !ok {
		t.Fatalf("no $defs entry %q", name)
	}
	return d
}

// deref follows a {"$ref": "#/$defs/X"} schema to its def.
func deref(t *testing.T, s, node obj) obj {
	t.Helper()
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	return def(t, s, strings.TrimPrefix(ref, "#/$defs/"))
}

func prop(t *testing.T, d obj, name string) obj {
	t.Helper()
	p, ok := d["properties"].(obj)[name].(obj)
	if !ok {
		t.Fatalf("no property %q in %v", name, d["properties"])
	}
	return p
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// nullable reports whether a property schema is the oneOf-with-null form above.
func nullable(p obj) bool {
	one, ok := p["oneOf"].([]any)
	if !ok || len(one) != 2 {
		return false
	}
	return reflect.DeepEqual(one[1], obj{"type": "null"})
}

func compile(t *testing.T, id string, b []byte, ptr string) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	if err := c.AddResource(id, doc); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile(id + ptr)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func instance(t *testing.T, b []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUpToDate(t *testing.T) {
	for file, gen := range map[string]func() []byte{"config.schema.json": Config, "api.schema.json": API} {
		path := filepath.Join("..", "..", "schema", file)
		got := gen()
		if os.Getenv("UPDATE_SCHEMA") == "1" {
			if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // committed file, world-readable
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path) //nolint:gosec // fixed repo path
		if err != nil {
			t.Fatalf("%v (run UPDATE_SCHEMA=1 go test ./internal/schema)", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is out of date; run UPDATE_SCHEMA=1 go test ./internal/schema", path)
		}
	}
}

func TestDeterministic(t *testing.T) {
	if !bytes.Equal(Config(), Config()) || !bytes.Equal(API(), API()) {
		t.Error("two runs differ")
	}
}

func TestIDs(t *testing.T) {
	if got := decode(t, Config())["$id"]; got != ConfigID {
		t.Errorf("config $id %v", got)
	}
	if got := decode(t, API())["$id"]; got != APIID {
		t.Errorf("api $id %v", got)
	}
	if got := decode(t, Config())["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema %v", got)
	}
}

func TestExamplesValid(t *testing.T) {
	sch := compile(t, ConfigID, Config(), "")
	files, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.yaml"))
	if err != nil || len(files) < 2 {
		t.Fatalf("examples: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // fixed test inputs
		if err != nil {
			t.Fatal(err)
		}
		js, err := yaml.YAMLToJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(instance(t, js)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
		if _, err := config.Parse(raw); err != nil {
			t.Errorf("%s: config.Parse: %v", f, err)
		}
	}
}

func TestDefaultConfigValid(t *testing.T) {
	usb := &config.USB{VendorID: "03f0", ProductID: "2b17"}
	devices := config.Default("Home")
	devices.Printers = []config.Printer{{ID: "p", Name: "P", Enabled: true, Mode: config.ModeUSB, USB: usb, Driver: config.Auto}}
	devices.Scanners = []config.Scanner{{ID: "s", Name: "S", Enabled: false, Mode: config.ModeUSB, USB: usb, Backend: config.BackendAuto}}
	fileSchema := compile(t, ConfigID, Config(), "")
	apiSchema := compile(t, APIID, API(), "#/$defs/Config")
	for name, c := range map[string]config.Config{"default": config.Default("Printer"), "empty defaults": devices} {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if err := fileSchema.Validate(instance(t, b)); err != nil {
			t.Errorf("%s: file schema: %v", name, err)
		}
		if err := apiSchema.Validate(instance(t, b)); err != nil {
			t.Errorf("%s: api schema: %v", name, err)
		}
	}
}

func TestRejectsInvalid(t *testing.T) {
	sch := compile(t, ConfigID, Config(), "")
	p := func(fields string) string {
		return `{"apiVersion": "pagewire/v2", "name": "x", "printers": [{"id": "p", "name": "P"` + fields + `}]}`
	}
	for _, doc := range []string{
		`{"apiVersion": "pagewire/v2", "name": "x", "colour": "red"}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "device": {"mode": "usb"}}`,
		p(`, "mode": "wifi"`),
		p(`, "usb": {"vendorId": "03F0", "productId": "2b17"}`),
		p(`, "defaults": {"media": "A4"}`),
		p(`, "defaults": {"mediaSource": "Tray 1"}`),
		p(`, "defaults": {"numberUp": 32}`),
		p(`, "defaults": {"sides": ""}`),
		`{"apiVersion": "pagewire/v2", "name": "x", "printers": [{"id": "-bad", "name": "P"}]}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "printers": [{"name": "P"}]}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "scanners": [{"id": "s"}]}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "scanners": [{"id": "s", "name": "S", "defaults": {"intent": "receipt"}}]}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "ignoredDevices": [{"vendorId": "046d", "productId": "c31c", "roles": []}]}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "ignoredDevices": [{"vendorId": "046d", "productId": "c31c", "roles": ["fax"]}]}`,
		`{"apiVersion": "pagewire/v1", "name": "x"}`,
		`{"apiVersion": "pagewire/v2", "name": ""}`,
		`{"apiVersion": "pagewire/v2", "name": "tab\there"}`,
		`{"apiVersion": "pagewire/v2", "name": "-h"}`,
		p(`, "usb": {"vendorId": "03f0", "productId": "2b17", "serial": "a\u0007b"}`),
		p(`, "usb": {"vendorId": "03f0", "productId": "2b17", "serial": "` + strings.Repeat("9", 127) + `"}`),
		`{"apiVersion": "pagewire/v2"}`,
		`{"apiVersion": "pagewire/v2", "name": "x", "licenses": {"acceptedEulas": [{"id": "a", "version": "1"}, {"id": "a", "version": "1"}]}}`,
	} {
		if err := sch.Validate(instance(t, []byte(doc))); err == nil {
			t.Errorf("schema accepted %s", doc)
		}
	}
}

func TestNoMaps(t *testing.T) {
	for name, b := range map[string][]byte{"config": Config(), "api": API()} {
		var walk func(v any, path string)
		walk = func(v any, path string) {
			switch x := v.(type) {
			case obj:
				if ap, ok := x["additionalProperties"]; ok && ap != false {
					t.Errorf("%s schema: schema-valued additionalProperties at %s", name, path)
				}
				if _, ok := x["patternProperties"]; ok {
					t.Errorf("%s schema: patternProperties at %s", name, path)
				}
				for k, c := range x {
					walk(c, path+"/"+k)
				}
			case []any:
				for i, c := range x {
					walk(c, fmt.Sprintf("%s/%d", path, i))
				}
			}
		}
		walk(decode(t, b), "")
	}
}

func TestRequired(t *testing.T) {
	cs := decode(t, Config())
	root := deref(t, cs, cs)
	if got := strs(root["required"]); !reflect.DeepEqual(got, []string{"apiVersion", "name"}) {
		t.Errorf("file schema root required = %v", got)
	}
	wantRequired := map[string][]string{"Config": {"apiVersion", "name"}, "Printer": {"id", "name"}, "Scanner": {"id", "name"}}
	for name, d := range cs["$defs"].(obj) {
		var got []string
		if r := d.(obj)["required"]; r != nil {
			got = strs(r)
		}
		if !reflect.DeepEqual(got, wantRequired[name]) {
			t.Errorf("file schema def %s has required %v, want %v", name, got, wantRequired[name])
		}
	}
	if p := prop(t, root, "apiVersion"); p["const"] != config.APIVersion {
		t.Errorf("apiVersion const = %v", p["const"])
	}
	if d := prop(t, def(t, cs, "Printer"), "name")["description"]; !strings.Contains(fmt.Sprint(d), "ipp-usb and AirSane advertise the device's own names") {
		t.Errorf("Printer.name description = %v", d)
	}

	as := decode(t, API())
	wantStatus := []string{"config", "phase", "message", "printers", "scanners", "workloads", "devices", "reconcile"}
	if got := strs(def(t, as, "Status")["required"]); !reflect.DeepEqual(got, wantStatus) {
		t.Errorf("Status required = %v", got)
	}
	for _, name := range []string{"Printer", "Scanner"} {
		if r := def(t, as, name)["required"]; r != nil && slices.Contains(strs(r), "usb") {
			t.Errorf("%s def requires usb", name)
		}
	}
	for _, name := range []string{"PrintDefaults", "ScanDefaults"} {
		if r := def(t, as, name)["required"]; r != nil {
			t.Errorf("api schema %s requires %v; every defaults key is optional", name, r)
		}
	}
	cr := def(t, as, "ConfigResponse")
	if got := strs(cr["required"]); !reflect.DeepEqual(got, []string{"etag", "config", "state", "errors", "lastGood"}) {
		t.Errorf("ConfigResponse required = %v", got)
	}
	for _, p := range []string{"config", "lastGood"} {
		if !nullable(prop(t, cr, p)) {
			t.Errorf("ConfigResponse.%s not nullable: %v", p, prop(t, cr, p))
		}
	}
	for _, name := range []string{"PrinterDecision", "ScannerDecision"} {
		dec := def(t, as, name)
		if !slices.Contains(strs(dec["required"]), "device") || !nullable(prop(t, dec, "device")) {
			t.Errorf("%s.device must be required and nullable: %v", name, dec)
		}
	}
	po := def(t, as, "PrintOptions")
	if r := po["required"]; r != nil {
		t.Errorf("PrintOptions requires %v; every print option is optional", r)
	}
	for _, p := range []string{"copies", "sides", "finishings"} {
		prop(t, po, p) // the PrintSettings embed is inlined
	}
	defs := as["$defs"].(obj)
	for _, gone := range []string{"PrintSettings", "Connect", "Decision", "StatusDevice"} {
		if _, ok := defs[gone]; ok {
			t.Errorf("api schema still has $defs/%s", gone)
		}
	}
	for name, want := range map[string][]string{"PrinterConnect": {"dnssdName", "ippUris"}, "ScannerConnect": {"dnssdName", "esclUrls"}} {
		if got := strs(def(t, as, name)["required"]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s required = %v, want %v", name, got, want)
		}
	}
	for owner, want := range map[string]string{"StatusPrinter": "PrinterConnect", "StatusScanner": "ScannerConnect"} {
		if got := prop(t, def(t, as, owner), "connect")["$ref"]; got != "#/$defs/"+want {
			t.Errorf("%s.connect refers to %v, want %s", owner, got, want)
		}
	}
	if got := prop(t, def(t, as, "Job"), "createdAt"); !reflect.DeepEqual(got, obj{"type": "string", "format": "date-time"}) {
		t.Errorf("time.Time reflected as %v, want a date-time string", got)
	}
}

// TestEULADef checks the licence text DTO of GET /eulas/{id}: every field
// required, text and textSha256 nullable strings, state an enum.
func TestEULADef(t *testing.T) {
	defs := decode(t, API())["$defs"].(obj)
	d, ok := defs["EULA"].(obj)
	if !ok {
		t.Fatal("api schema has no EULA def")
	}
	want := []string{"id", "version", "title", "vendor", "summary", "state", "message", "textSha256", "text"}
	if got := strs(d["required"]); !reflect.DeepEqual(got, want) {
		t.Errorf("EULA required %v, want %v", got, want)
	}
	for _, p := range []string{"text", "textSha256"} {
		if s := prop(t, d, p); !nullable(s) || !reflect.DeepEqual(s["oneOf"].([]any)[0], obj{"type": "string"}) {
			t.Errorf("EULA.%s is not a nullable string: %v", p, s)
		}
	}
	if got := strs(prop(t, d, "state")["enum"]); !reflect.DeepEqual(got, []string{"pending", "available", "failed"}) {
		t.Errorf("EULA.state enum %v", got)
	}
	setup := defs["SetupEULA"].(obj)
	if _, ok := setup["properties"].(obj)["url"]; ok {
		t.Error("SetupEULA still has url")
	}
}

// TestNullableSet checks that the nullable properties of the API schema are
// exactly the REST contract's list.
func TestNullableSet(t *testing.T) {
	as := decode(t, API())
	var got []string
	for name, d := range as["$defs"].(obj) {
		props, _ := d.(obj)["properties"].(obj)
		for p, s := range props {
			if nullable(s.(obj)) {
				got = append(got, name+"."+p)
			}
		}
	}
	sort.Strings(got)
	want := []string{
		"ConfigResponse.config", "ConfigResponse.lastGood",
		"DetectedDevice.printerDecision", "DetectedDevice.scannerDecision",
		"DetectedDevice.suggestedPrinter", "DetectedDevice.suggestedScanner",
		"EULA.text", "EULA.textSha256",
		"PrinterDecision.device",
		"Scan.error",
		"ScannerDecision.device",
		"SetupEULA.textSha256",
		"StatusPrinter.decision", "StatusPrinter.device",
		"StatusScanner.decision", "StatusScanner.device",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nullable properties\n got  %v\n want %v", got, want)
	}
}

func TestStatusEnums(t *testing.T) {
	as := decode(t, API())
	enum := func(d, field string) []string { return strs(prop(t, def(t, as, d), field)["enum"]) }

	if mode := enum("StatusWorkload", "mode"); !reflect.DeepEqual(mode, []string{"none", "usb", "simulated", "probe"}) {
		t.Errorf("StatusWorkload.mode enum = %v", mode)
	}
	for _, c := range []struct {
		def, field, value string
	}{
		{"StatusWorkload", "phase", "absent"},
		{"Status", "phase", "none"},
		{"StatusPrinter", "phase", "none"},
		{"StatusScanner", "phase", "none"},
		{"StatusPrinter", "state", "unknown"},
		{"StatusScanner", "state", "unknown"},
	} {
		if e := enum(c.def, c.field); !slices.Contains(e, c.value) || slices.Contains(e, "") {
			t.Errorf("%s.%s enum = %v, want it to contain %q and not \"\"", c.def, c.field, e, c.value)
		}
	}
	cfg := deref(t, as, prop(t, def(t, as, "Status"), "config"))
	if e := strs(prop(t, cfg, "state")["enum"]); !slices.Contains(e, "missing") || slices.Contains(e, "") {
		t.Errorf("Status.config.state enum = %v", e)
	}
}

// TestSetupValid checks that Setup suggestions as the controller builds them
// (usb selector set, empty defaults) and a scan request without intent
// validate against the API schema.
func TestSetupValid(t *testing.T) {
	usb := &config.USB{VendorID: "04e8", ProductID: "342e", Serial: "S1"}
	dev := device.USBDevice{
		VendorID: "04e8", ProductID: "342e", Serial: "S1", Bus: 1, Address: 9,
		Interfaces: []device.USBInterface{{Class: 7, SubClass: 1, Protocol: 2}},
	}
	setup := apitypes.Setup{
		Backend: "sim",
		Devices: []apitypes.DetectedDevice{{
			Device: dev, CanPrint: true, CanScan: true, Ignored: []string{},
			SuggestedPrinter: &config.Printer{ID: "scx-4300", Name: "SCX-4300", Enabled: true, Mode: config.ModeUSB, USB: usb, Driver: config.Auto},
			SuggestedScanner: &config.Scanner{ID: "scx-4300-scanner", Name: "SCX-4300 scanner", Enabled: true, Mode: config.ModeUSB, USB: usb, Backend: config.BackendAuto},
			PrinterDecision:  &device.PrinterDecision{Family: "legacy", RequiredEULAs: []string{}, OptionalEULAs: []string{}, Rule: device.RuleModel, Device: &dev},
			ScannerDecision:  &device.ScannerDecision{Family: "airsane", SaneBackends: []string{"xerox_mfp"}, Rule: device.RuleSANEIndex, Device: &dev},
		}},
		EULAs:     []apitypes.SetupEULA{},
		Suggested: config.Default("Home"),
	}
	for ptr, v := range map[string]any{
		"#/$defs/Setup":        setup,
		"#/$defs/ScanSettings": apitypes.ScanSettings{Scanner: "s", Source: "platen", Resolution: 300, ColorMode: "color", Format: "pdf"},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := compile(t, APIID, API(), ptr).Validate(instance(t, b)); err != nil {
			t.Errorf("%s: %v\n%s", ptr, err, b)
		}
	}
}

// TestConfigEnumsParse pushes every enum value of the file schema through
// config.Parse, so the validate and jsonschema tags cannot drift apart. It
// walks object properties and array items; an array step wraps its value in
// a one-element list whose entry gets its required siblings.
func TestConfigEnumsParse(t *testing.T) {
	cs := decode(t, Config())
	siblings := map[string]obj{
		"printers":       {"id": "p", "name": "P"},
		"scanners":       {"id": "s", "name": "S"},
		"ignoredDevices": {"vendorId": "04e8", "productId": "342e", "roles": []any{"printer"}},
	}
	const item = "[]"
	// wrap builds the document that sets path to v.
	wrap := func(path []string, v any) obj {
		doc := v
		for i := len(path) - 1; i >= 0; i-- {
			if path[i] == item {
				if m, ok := doc.(obj); ok && i > 0 {
					for k, sv := range siblings[path[i-1]] {
						if _, set := m[k]; !set {
							m[k] = sv
						}
					}
				}
				doc = []any{doc}
				continue
			}
			doc = obj{path[i]: doc}
		}
		return doc.(obj)
	}
	n := 0
	seen := map[string]bool{}
	var walk func(p obj, path []string)
	walk = func(p obj, path []string) {
		p = deref(t, cs, p)
		switch p["type"] {
		case "object":
			for name, c := range p["properties"].(obj) {
				walk(c.(obj), append(slices.Clone(path), name))
			}
			return
		case "array":
			walk(p["items"].(obj), append(slices.Clone(path), item))
			return
		}
		enum, ok := p["enum"].([]any)
		if !ok {
			return
		}
		seen[path[0]] = true
		for _, v := range enum {
			m := wrap(path, v)
			m["apiVersion"], m["name"] = config.APIVersion, "x"
			b, _ := json.Marshal(m)
			if _, err := config.Parse(b); err != nil {
				t.Errorf("%s = %v: %v (%s)", strings.Join(path, "."), v, err, b)
			}
			n++
		}
	}
	walk(cs, nil)
	if n < 40 {
		t.Errorf("only %d enum values found", n)
	}
	for _, list := range []string{"printers", "scanners", "ignoredDevices"} {
		if !seen[list] {
			t.Errorf("no enum found under %s", list)
		}
	}
}

// TestPatterns checks that patterns with {m,n} survive invopop's split of the
// jsonschema tag on commas.
func TestPatterns(t *testing.T) {
	cs := decode(t, Config())
	printer := def(t, cs, "Printer")
	if got := prop(t, printer, "id")["pattern"]; got != "^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$" {
		t.Errorf("printers.items.id pattern = %v", got)
	}
	defaults := deref(t, cs, prop(t, printer, "defaults"))
	if got := prop(t, defaults, "mediaSource")["pattern"]; got != "^[a-z0-9][a-z0-9._-]{0,254}$" {
		t.Errorf("printers.items.defaults.mediaSource pattern = %v", got)
	}
}

func TestSchemaHeader(t *testing.T) {
	if want := "# yaml-language-server: $schema=" + ConfigID + "\n"; config.SchemaHeader != want {
		t.Errorf("config.SchemaHeader = %q, want %q", config.SchemaHeader, want)
	}
}

func TestPointersNullable(t *testing.T) {
	if v := pointerViolations(reflect.TypeOf(apiRoot{})); len(v) > 0 {
		t.Errorf("pointer fields that are neither nullable nor omitempty: %v", v)
	}

	t.Run("time leaves", func(t *testing.T) {
		type withTime struct {
			At    time.Time  `json:"at"`
			Maybe *time.Time `json:"maybe" jsonschema:"nullable"`
		}
		if v := pointerViolations(reflect.TypeOf(withTime{})); len(v) > 0 {
			t.Errorf("unexpected violations %v", v)
		}
	})

	t.Run("detects violations", func(t *testing.T) {
		type inner struct {
			Bad *int `json:"bad"`
		}
		type outer struct {
			Items    []inner `json:"items"`
			Both     *int    `json:"both,omitempty" jsonschema:"nullable"`
			Optional *int    `json:"optional,omitempty"`
			Skipped  *int    `json:"-"`
			hidden   *int
		}
		_ = outer{hidden: nil}
		got := pointerViolations(reflect.TypeOf(outer{}))
		want := []string{"inner.Bad", "outer.Both"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestUniqueDefNames(t *testing.T) {
	seen := map[string]reflect.Type{}
	walkTypes(reflect.TypeOf(apiRoot{}), func(st reflect.Type) {
		if prev, ok := seen[st.Name()]; ok && prev != st {
			t.Errorf("$defs name %s used by %s and %s", st.Name(), prev, st)
		}
		seen[st.Name()] = st
	})
	if len(seen) < 20 {
		t.Errorf("walked only %d types", len(seen))
	}
}

// walkTypes calls fn once for every named struct type reachable from root
// through exported fields, slices and pointers. time.Time and json.Marshaler
// types are leaves.
func walkTypes(root reflect.Type, fn func(reflect.Type)) {
	marshaler := reflect.TypeFor[json.Marshaler]()
	visited := map[reflect.Type]bool{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct || visited[t] || t == reflect.TypeFor[time.Time]() ||
			t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler) {
			return
		}
		visited[t] = true
		fn(t)
		for i := range t.NumField() {
			if f := t.Field(i); f.IsExported() {
				walk(f.Type)
			}
		}
	}
	walk(root)
}

// pointerViolations names every pointer field (Type.Field) reachable from root
// whose tags are neither jsonschema:"nullable" without omitempty nor
// json:",omitempty" without nullable.
func pointerViolations(root reflect.Type) []string {
	var out []string
	walkTypes(root, func(st reflect.Type) {
		for i := range st.NumField() {
			f := st.Field(i)
			if !f.IsExported() || f.Type.Kind() != reflect.Pointer {
				continue
			}
			jsonTag := strings.Split(f.Tag.Get("json"), ",")
			if jsonTag[0] == "-" {
				continue
			}
			omitempty := slices.Contains(jsonTag[1:], "omitempty")
			isNullable := slices.Contains(strings.Split(f.Tag.Get("jsonschema"), ","), "nullable")
			if omitempty == isNullable {
				out = append(out, st.Name()+"."+f.Name)
			}
		}
	})
	sort.Strings(out)
	return out
}
