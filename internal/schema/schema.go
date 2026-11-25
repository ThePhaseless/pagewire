// Package schema generates the JSON Schemas (Draft 2020-12) committed under
// schema/: the config file schema and the REST API DTO schema the UI's Zod
// types are generated from. Regenerate with UPDATE_SCHEMA=1 go test ./internal/schema.
package schema

import (
	"encoding/json"

	"github.com/invopop/jsonschema"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/config"
)

// Schema $id values.
const (
	ConfigID = "https://raw.githubusercontent.com/ThePhaseless/printscan/main/schema/config.schema.json"
	APIID    = "https://raw.githubusercontent.com/ThePhaseless/printscan/main/schema/api.schema.json"
)

// apiRoot lists every API DTO; each reachable type becomes a $defs entry
// named after its Go type.
type apiRoot struct {
	ConfigResponse apitypes.ConfigResponse
	ConfigEvent    apitypes.ConfigEvent
	Setup          apitypes.Setup
	EULA           apitypes.EULA
	Status         apitypes.Status
	PrintOptions   apitypes.PrintOptions
	Job            apitypes.Job
	ScannerCaps    apitypes.ScannerCaps
	ScanSettings   apitypes.ScanSettings
	Scan           apitypes.Scan
	Problem        apitypes.Problem
}

// Config returns the config file schema: a root $ref to $defs/Config, with
// only apiVersion and name required.
func Config() []byte {
	r := jsonschema.Reflector{RequiredFromJSONSchemaTags: true, Anonymous: true}
	s := r.Reflect(&config.Config{})
	s.ID = ConfigID
	return finish(s)
}

// API returns the API schema: only $defs, one entry per DTO type. Every field
// without omitempty is required.
func API() []byte {
	r := jsonschema.Reflector{Anonymous: true}
	full := r.Reflect(&apiRoot{})
	delete(full.Definitions, "apiRoot")
	return finish(&jsonschema.Schema{Version: full.Version, ID: APIID, Definitions: full.Definitions})
}

func finish(s *jsonschema.Schema) []byte {
	// The jsonschema tag has no const keyword.
	if c, ok := s.Definitions["Config"]; ok {
		if p, ok := c.Properties.Get("apiVersion"); ok {
			p.Const = config.APIVersion
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		panic("schema: " + err.Error()) // static types; cannot fail
	}
	return append(b, '\n')
}
