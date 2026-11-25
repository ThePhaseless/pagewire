package yamledit

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

const doc = `# yaml-language-server: $schema=https://example.invalid/config.schema.json
apiVersion: pagewire/v1          # required

# Head comment of name,
# two lines.
name: Żółw drukarka

location: Living room             # printer-location

device:
  mode: usb                       # usb | simulated
  driver: auto                    # driver family
  scanner: 'auto'

print:
  defaults:
    media: iso_a4_210x297mm
  maxUploadMiB: 50                # 1 to 500
  # foot comment of print

licenses:
  acceptedEulas: []               # accepted EULAs
`

func TestApplyGolden(t *testing.T) {
	cases := []struct {
		name, src, patch, want string
	}{
		{
			name:  "empty patch is byte-identical",
			src:   doc,
			patch: `{}`,
			want:  doc,
		},
		{
			name:  "no-op patch is byte-identical",
			src:   doc,
			patch: `{"device":{"mode":"usb"},"print":{"maxUploadMiB":50},"missing":null}`,
			want:  doc,
		},
		{
			name:  "scalar replace keeps the line comment at its column",
			src:   doc,
			patch: `{"device":{"mode":"simulated"},"print":{"maxUploadMiB":100}}`,
			want: replace(doc,
				"  mode: usb                       # usb | simulated\n", "  mode: simulated                 # usb | simulated\n",
				"  maxUploadMiB: 50                # 1 to 500\n", "  maxUploadMiB: 100               # 1 to 500\n"),
		},
		{
			name:  "non-ASCII above the patched key",
			src:   doc,
			patch: `{"location":"Kitchen"}`,
			want:  replace(doc, "location: Living room             #", "location: Kitchen                 #"),
		},
		{
			name:  "quoted strings stay quoted",
			src:   "usb:\n  vendorId: \"03f0\"   # vid\n  serial: 'abc'\n",
			patch: `{"usb":{"vendorId":"04b8","serial":"it's"}}`,
			want:  "usb:\n  vendorId: \"04b8\"   # vid\n  serial: 'it''s'\n",
		},
		{
			name:  "strings that would not stay strings get quoted",
			src:   "a: x\nb: x\nc: x\nd: x\ne: x\n",
			patch: `{"a":"true","b":"3.24","c":"","d":"k: v","e":"plain text"}`,
			want:  "a: \"true\"\nb: \"3.24\"\nc: \"\"\nd: \"k: v\"\ne: plain text\n",
		},
		{
			name:  "nested key into a missing mapping",
			src:   doc,
			patch: `{"device":{"usb":{"vendorId":"03f0","productId":"2b17","serial":null}}}`,
			want: replace(doc, "  scanner: 'auto'\n",
				"  scanner: 'auto'\n  usb:\n    vendorId: \"03f0\"\n    productId: \"2b17\"\n"),
		},
		{
			name:  "new key goes after the mapping's foot comment",
			src:   doc,
			patch: `{"print":{"defaults":{"quality":"high"},"extra":true}}`,
			want: replace(doc,
				"    media: iso_a4_210x297mm\n", "    media: iso_a4_210x297mm\n    quality: high\n",
				"  # foot comment of print\n", "  # foot comment of print\n  extra: true\n"),
		},
		{
			name:  "new top-level key at the end of a file without a final newline",
			src:   "a: 1\n\nb: 2",
			patch: `{"c":{"d":[1,"x"]}}`,
			want:  "a: 1\n\nb: 2\nc:\n  d:\n    - 1\n    - x\n",
		},
		{
			name:  "delete removes the key with its head comments and one blank line",
			src:   doc,
			patch: `{"name":null}`,
			want:  replace(doc, "\n# Head comment of name,\n# two lines.\nname: Żółw drukarka\n", ""),
		},
		{
			name:  "delete keeps a comment indented under the previous mapping",
			src:   "a:\n  x: 1\n  # under a\nb: 2\nc: 3\n",
			patch: `{"b":null}`,
			want:  "a:\n  x: 1\n  # under a\nc: 3\n",
		},
		{
			name:  "deleting neighbours takes the comment between them once",
			src:   "a:\n  x: 1\n  # under a\nb: 2\nc: 3\n",
			patch: `{"a":null,"b":null}`,
			want:  "c: 3\n",
		},
		{
			name:  "delete a nested mapping with its line comments",
			src:   doc,
			patch: `{"print":{"defaults":null},"device":{"driver":null}}`,
			want: replace(doc,
				"  driver: auto                    # driver family\n", "",
				"  defaults:\n    media: iso_a4_210x297mm\n", ""),
		},
		{
			name:  "array replace with a two-element EULA list",
			src:   doc,
			patch: `{"licenses":{"acceptedEulas":[{"id":"hplip-plugin","version":"3.24.4"},{"id":"other","version":"1"}]}}`,
			want: replace(doc, "  acceptedEulas: []               # accepted EULAs\n",
				"  acceptedEulas:                  # accepted EULAs\n"+
					"    - id: hplip-plugin\n      version: \"3.24.4\"\n"+
					"    - id: other\n      version: \"1\"\n"),
		},
		{
			name:  "array replace follows the file's sequence indent",
			src:   "x:\n- a   # one\n- b\ny:\n- c\n",
			patch: `{"y":["d","e"]}`,
			want:  "x:\n- a   # one\n- b\ny:\n- d\n- e\n",
		},
		{
			name:  "block list replaced by a scalar and empty array",
			src:   "l:    # keep\n  - a\n  - b\nm:\n  - c\nn: 1\n",
			patch: `{"l":"x","m":[]}`,
			want:  "l: x  # keep\nm: []\nn: 1\n",
		},
		{
			name:  "object patch onto a scalar replaces it",
			src:   "usb: null # sel\nz: 1\n",
			patch: `{"usb":{"vendorId":"03f0","drop":null}}`,
			want:  "usb:      # sel\n  vendorId: \"03f0\"\nz: 1\n",
		},
		{
			name:  "object patch onto an empty flow mapping",
			src:   "a: {}\nb: 1\n",
			patch: `{"a":{"c":1}}`,
			want:  "a:\n  c: 1\nb: 1\n",
		},
		{
			name:  "implicit null value",
			src:   "a:\nb: 2\n",
			patch: `{"a":"x"}`,
			want:  "a: x\nb: 2\n",
		},
		{
			name:  "deleting every key of a mapping leaves an empty mapping",
			src:   "a:\n  b: 1\nc: 2\n",
			patch: `{"a":{"b":null}}`,
			want:  "a: {}\nc: 2\n",
		},
		{
			name:  "four-space indent",
			src:   "a:\n    b: 1\n",
			patch: `{"a":{"c":{"d":1}}}`,
			want:  "a:\n    b: 1\n    c:\n        d: 1\n",
		},
		{
			name:  "empty document",
			src:   "# only a comment\n",
			patch: `{"a":1}`,
			want:  "# only a comment\na: 1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Apply([]byte(tc.src), []byte(tc.patch))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			checkSemantics(t, tc.src, tc.patch, got)
		})
	}
}

func TestApplyErrors(t *testing.T) {
	cases := []struct{ name, src, patch string }{
		{"array patch", "a: 1\n", `[1]`},
		{"scalar patch", "a: 1\n", `"x"`},
		{"null patch", "a: 1\n", `null`},
		{"malformed JSON", "a: 1\n", `{"a":`},
		{"trailing data", "a: 1\n", `{} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Apply([]byte(tc.src), []byte(tc.patch))
			if !errors.Is(err, ErrInvalidPatch) {
				t.Fatalf("err = %v, want ErrInvalidPatch", err)
			}
		})
	}
	unsupported := []struct{ name, src, patch string }{
		{"flow mapping on the path", "a: {b: 1}\n", `{"a":{"c":2}}`},
		{"alias on the path", "x: &x\n  b: 1\na: *x\n", `{"a":{"c":2}}`},
		{"anchored value replaced", "x: &x\n  b: 1\na: *x\n", `{"x":null}`},
		{"root is a list", "- a\n", `{"a":1}`},
		{"two documents", "a: 1\n---\nb: 2\n", `{"a":2}`},
	}
	for _, tc := range unsupported {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Apply([]byte(tc.src), []byte(tc.patch)); err == nil || errors.Is(err, ErrInvalidPatch) {
				t.Fatalf("err = %v, want an unsupported-shape error", err)
			}
		})
	}
	if _, err := Apply([]byte("a: [\n"), []byte(`{}`)); err == nil {
		t.Fatal("syntax error in the source was not reported")
	}
}

// replace applies old/new pairs to s, failing loudly when a pair is missing.
func replace(s string, pairs ...string) string {
	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			panic("replace: not found: " + pairs[i])
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

// checkSemantics compares the decoded output with RFC 7396 applied to the
// decoded input, both normalised through JSON.
func checkSemantics(t *testing.T, src, patch string, got []byte) {
	t.Helper()
	var before, after, p any
	if err := yaml.Unmarshal([]byte(src), &before); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(got, &after); err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	if err := json.Unmarshal([]byte(patch), &p); err != nil {
		t.Fatal(err)
	}
	want := normalise(t, merge(normalise(t, before), p))
	if have := normalise(t, after); !reflect.DeepEqual(have, want) {
		t.Errorf("semantics: got %#v, want %#v", have, want)
	}
}

func normalise(t *testing.T, v any) any {
	t.Helper()
	if v == nil {
		v = map[string]any{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func merge(target, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	m, ok := target.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = merge(m[k], v)
		}
	}
	return m
}
