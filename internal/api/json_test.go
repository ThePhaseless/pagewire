package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type jsonInner struct {
	Tags []string  `json:"tags"`
	At   time.Time `json:"at"`
}

type rawMarshaler struct{}

func (r rawMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"raw"`), nil }

type ptrMarshaler struct{}

func (r *ptrMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"ptr"`), nil }

type jsonOuter struct {
	List   []int        `json:"list"`
	Inner  jsonInner    `json:"inner"`
	Ptr    *jsonInner   `json:"ptr"`
	NilPtr *jsonInner   `json:"nilPtr"`
	Items  []jsonInner  `json:"items"`
	PItems []*jsonInner `json:"pItems"`
	At     time.Time    `json:"at"`
	Raw    rawMarshaler `json:"rawM"`
	PRaw   ptrMarshaler `json:"pRaw"`
}

func TestEncodeJSONNilSlices(t *testing.T) {
	b, err := encodeJSON(jsonOuter{
		Ptr:    &jsonInner{},
		Items:  []jsonInner{{}},
		PItems: []*jsonInner{{}, nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`"list":[]`,
		`"inner":{"tags":[]`,
		`"ptr":{"tags":[]`,
		`"nilPtr":null`,
		`"items":[{"tags":[]`,
		`"pItems":[{"tags":[],"at":"0001-01-01T00:00:00Z"},null]`,
		`"rawM":"raw"`,
		`"pRaw":"ptr"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if strings.Count(s, "null") != 2 { // nilPtr and the nil element of pItems
		t.Errorf("unexpected null in %s", s)
	}
}

func TestEncodeJSONTopLevelSlice(t *testing.T) {
	var jobs []jsonInner
	b, err := encodeJSON(jobs)
	if err != nil || string(b) != "[]" {
		t.Fatalf("got %s, %v", b, err)
	}
}

func TestEncodeJSONKeepsOffset(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	at := time.Date(2026, 11, 24, 17, 0, 0, 0, cet)
	b, err := encodeJSON(jsonOuter{At: at, Ptr: &jsonInner{At: at}})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"2026-11-24T17:00:00+01:00"`); n != 2 {
		t.Fatalf("want 2 offset timestamps, got %d in %s", n, b)
	}
}

func TestNormalizeDoesNotModifyInput(t *testing.T) {
	in := jsonOuter{
		Ptr:    &jsonInner{},
		Items:  []jsonInner{{}},
		PItems: []*jsonInner{{}},
		Raw:    rawMarshaler{},
	}
	before := jsonOuter{
		Ptr:    &jsonInner{},
		Items:  []jsonInner{{}},
		PItems: []*jsonInner{{}},
	}
	ptr, item := in.Ptr, in.PItems[0]

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 100 {
			// Concurrent reads of everything normalize could touch.
			_ = in.Ptr.Tags == nil && in.Items[0].Tags == nil && in.PItems[0].Tags == nil
		}
	}()
	for range 100 {
		if _, err := encodeJSON(in); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()

	if !reflect.DeepEqual(in, before) {
		t.Fatalf("input changed: %+v", in)
	}
	if in.Ptr != ptr || in.PItems[0] != item {
		t.Fatal("pointers in the input were replaced")
	}
	if in.Ptr.Tags != nil || in.Items[0].Tags != nil || in.PItems[0].Tags != nil {
		t.Fatal("nil slices in the input were filled")
	}
}

func TestEncodeJSONMatchesStdlibShape(t *testing.T) {
	// Non-nil values encode exactly as encoding/json does.
	v := jsonInner{Tags: []string{"a"}, At: time.Unix(0, 0).UTC()}
	got, err := encodeJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(v)
	if string(got) != string(want) {
		t.Fatalf("got %s want %s", got, want)
	}
}
