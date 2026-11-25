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

	"github.com/ThePhaseless/pagewire/internal/config"
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
	b, err := json.Marshal(config.Default("Printer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := compile(t, ConfigID, Config(), "").Validate(instance(t, b)); err != nil {
		t.Errorf("file schema: %v", err)
	}
	if err := compile(t, APIID, API(), "#/$defs/Config").Validate(instance(t, b)); err != nil {
		t.Errorf("api schema: %v", err)
	}
}

func TestRejectsInvalid(t *testing.T) {
	sch := compile(t, ConfigID, Config(), "")
	for _, doc := range []string{
		`{"apiVersion": "pagewire/v1", "name": "x", "colour": "red"}`,
		`{"apiVersion": "pagewire/v1", "name": "x", "device": {"mode": "wifi"}}`,
		`{"apiVersion": "pagewire/v1", "name": "x", "device": {"usb": {"vendorId": "03F0", "productId": "2b17"}}}`,
		`{"apiVersion": "pagewire/v1", "name": "x", "print": {"defaults": {"media": "A4"}}}`,
		`{"apiVersion": "pagewire/v2", "name": "x"}`,
		`{"apiVersion": "pagewire/v1", "name": ""}`,
		`{"apiVersion": "pagewire/v1", "name": "tab\there"}`,
		`{"apiVersion": "pagewire/v1", "name": "-h"}`,
		`{"apiVersion": "pagewire/v1", "name": "x", "device": {"usb": {"vendorId": "03f0", "productId": "2b17", "serial": "a\u0007b"}}}`,
		`{"apiVersion": "pagewire/v1", "name": "x", "device": {"usb": {"vendorId": "03f0", "productId": "2b17", "serial": "` + strings.Repeat("9", 127) + `"}}}`,
		`{"apiVersion": "pagewire/v1"}`,
		`{"apiVersion": "pagewire/v1", "name": "x", "licenses": {"acceptedEulas": [{"id": "a", "version": "1"}, {"id": "a", "version": "1"}]}}`,
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
	for name, d := range cs["$defs"].(obj) {
		if name != "Config" && d.(obj)["required"] != nil {
			t.Errorf("file schema def %s has required %v", name, d.(obj)["required"])
		}
	}
	if p := prop(t, root, "apiVersion"); p["const"] != config.APIVersion {
		t.Errorf("apiVersion const = %v", p["const"])
	}
	if d := prop(t, root, "name")["description"]; !strings.Contains(fmt.Sprint(d), "ipp-usb and AirSane advertise the device's own names") {
		t.Errorf("name description = %v", d)
	}

	as := decode(t, API())
	if got := strs(def(t, as, "Status")["required"]); !reflect.DeepEqual(got, []string{"config", "device", "printer", "scanner", "reconcile", "connect"}) {
		t.Errorf("Status required = %v", got)
	}
	if slices.Contains(strs(def(t, as, "Device")["required"]), "usb") {
		t.Error("Device def requires usb")
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
	dec := def(t, as, "Decision")
	if !slices.Contains(strs(dec["required"]), "device") || !nullable(prop(t, dec, "device")) {
		t.Errorf("Decision.device must be required and nullable: %v", dec)
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
		"Decision.device",
		"EULA.text", "EULA.textSha256",
		"Scan.error",
		"Setup.decision", "Setup.inventory",
		"SetupEULA.textSha256",
		"StatusDevice.decision", "StatusDevice.inventory",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nullable properties\n got  %v\n want %v", got, want)
	}
}

func TestStatusEnums(t *testing.T) {
	as := decode(t, API())
	status := def(t, as, "Status")
	sub := func(name string) obj { return deref(t, as, prop(t, status, name)) }

	mode := strs(prop(t, sub("device"), "mode")["enum"])
	if !reflect.DeepEqual(mode, []string{"none", "usb", "simulated", "probe"}) {
		t.Errorf("device.mode enum = %v", mode)
	}
	for _, c := range []struct {
		section, field, value string
	}{
		{"device", "phase", "none"},
		{"printer", "state", "unknown"},
		{"scanner", "state", "unknown"},
		{"config", "state", "missing"},
	} {
		enum := strs(prop(t, sub(c.section), c.field)["enum"])
		if !slices.Contains(enum, c.value) || slices.Contains(enum, "") {
			t.Errorf("%s.%s enum = %v, want it to contain %q and not \"\"", c.section, c.field, enum, c.value)
		}
	}
}

// TestConfigEnumsParse pushes every enum value of the file schema through
// config.Parse, so the validate and jsonschema tags cannot drift apart.
func TestConfigEnumsParse(t *testing.T) {
	cs := decode(t, Config())
	var walk func(d obj, path []string)
	n := 0
	walk = func(d obj, path []string) {
		props, _ := d["properties"].(obj)
		for name, p := range props {
			p := deref(t, cs, p.(obj))
			here := append(slices.Clone(path), name)
			if p["type"] == "object" {
				walk(p, here)
				continue
			}
			enum, ok := p["enum"].([]any)
			if !ok {
				continue
			}
			for _, v := range enum {
				doc := v
				for i := len(here) - 1; i >= 0; i-- {
					doc = obj{here[i]: doc}
				}
				m := doc.(obj)
				m["apiVersion"], m["name"] = config.APIVersion, "x"
				b, _ := json.Marshal(m)
				if _, err := config.Parse(b); err != nil {
					t.Errorf("%s = %v: %v", strings.Join(here, "."), v, err)
				}
				n++
			}
		}
	}
	walk(deref(t, cs, cs), nil)
	if n < 25 {
		t.Errorf("only %d enum values found", n)
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
