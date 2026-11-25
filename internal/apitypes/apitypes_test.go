package apitypes

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// The device and config defaults structs must stay field-identical: callers
// convert with device.PrintSettings(entry.Defaults).
var _ = device.PrintSettings(config.PrintDefaults{})

func TestWarningOptionEnum(t *testing.T) {
	f, _ := reflect.TypeFor[Warning]().FieldByName("Option")
	var got []string
	for _, kw := range strings.Split(f.Tag.Get("jsonschema"), ",") {
		if v, ok := strings.CutPrefix(kw, "enum="); ok {
			got = append(got, v)
		}
	}
	if !reflect.DeepEqual(got, device.OptionKinds) {
		t.Errorf("Warning.option enum\n got  %v\n want %v (device.OptionKinds)", got, device.OptionKinds)
	}
}

func TestPrintOptionsFlat(t *testing.T) {
	b, err := json.Marshal(PrintOptions{Copies: 2, PrintSettings: device.PrintSettings{Sides: "one-sided"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"copies":2,"sides":"one-sided"}` {
		t.Errorf("PrintOptions JSON = %s", got)
	}
	if b, _ := json.Marshal(PrintOptions{}); string(b) != `{}` {
		t.Errorf("empty PrintOptions JSON = %s", b)
	}
}
