package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/config"
)

// encodeJSON is the one JSON encoder of every response and SSE payload: it
// marshals normalize(v), so arrays are never null.
func encodeJSON(v any) ([]byte, error) {
	return json.Marshal(normalize(v))
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
)

// normalize returns a copy of v in which every nil slice reachable through
// exported fields, pointers, slices and interfaces is an empty slice. It never
// writes through a pointer or into a slice it received: pointed-to values and
// slices that need changes are copied. time.Time and json.Marshaler types are
// leaves and encode as they are.
func normalize(v any) any {
	if v == nil {
		return nil
	}
	n := normalizeValue(reflect.ValueOf(v))
	if n.Kind() == reflect.Struct && n.CanAddr() {
		// Addressable, so pointer-receiver MarshalJSON methods of fields run.
		return n.Addr().Interface()
	}
	return n.Interface()
}

func isLeaf(t reflect.Type) bool {
	return t == timeType || t.Implements(marshalerType) || reflect.PointerTo(t).Implements(marshalerType)
}

func normalizeValue(v reflect.Value) reflect.Value {
	t := v.Type()
	if isLeaf(t) {
		return v
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		n := reflect.New(t.Elem())
		n.Elem().Set(normalizeValue(v.Elem()))
		return n
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		n := reflect.New(t).Elem()
		n.Set(normalizeValue(v.Elem()))
		return n
	case reflect.Struct:
		n := reflect.New(t).Elem()
		n.Set(v)
		for i := range t.NumField() {
			if t.Field(i).IsExported() {
				n.Field(i).Set(normalizeValue(v.Field(i)))
			}
		}
		return n
	case reflect.Slice:
		if v.IsNil() {
			return reflect.MakeSlice(t, 0, 0)
		}
		if !composite(t.Elem()) {
			return v
		}
		n := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			n.Index(i).Set(normalizeValue(v.Index(i)))
		}
		return n
	case reflect.Array:
		n := reflect.New(t).Elem()
		for i := range v.Len() {
			n.Index(i).Set(normalizeValue(v.Index(i)))
		}
		return n
	default:
		return v
	}
}

// composite reports whether values of t can hold a nil slice normalize must fill.
func composite(t reflect.Type) bool {
	if isLeaf(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

// writeJSON writes v as an application/json response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	writeEncoded(w, status, "application/json", v)
}

func writeEncoded(w http.ResponseWriter, status int, contentType string, v any) {
	b, err := encodeJSON(v)
	if err != nil {
		slog.Error("encode response", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b) //nolint:gosec // JSON body with a JSON content type and nosniff, not HTML
}

// writeProblem writes an RFC 9457 application/problem+json body.
func writeProblem(w http.ResponseWriter, status int, typ, detail string, errs ...config.FieldError) {
	writeEncoded(w, status, "application/problem+json", apitypes.Problem{
		Type:   typ,
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
		Errors: errs,
	})
}
