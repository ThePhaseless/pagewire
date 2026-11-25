package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/config/store"
	"github.com/ThePhaseless/pagewire/internal/config/yamledit"
	"github.com/ThePhaseless/pagewire/internal/schema"
)

const maxConfigBody = 1 << 20

// yamlHeader starts every file the API writes from a JSON config.
const yamlHeader = "# yaml-language-server: $schema=" + schema.ConfigID + "\n"

// configResponse builds the one ConfigResponse shape (GET, PUT, PATCH and
// their 412) from the file on disk now. A missing file gives store.ErrNotExist.
func (s *server) configResponse() (apitypes.ConfigResponse, error) {
	raw, etag, err := s.d.Store.Read()
	if err != nil {
		return apitypes.ConfigResponse{}, err
	}
	resp := apitypes.ConfigResponse{Etag: etag, State: apitypes.ConfigApplied, LastGood: s.d.LastGood()}
	cfg, err := s.d.Check(raw)
	if err != nil {
		resp.State = apitypes.ConfigInvalid
		resp.Errors = fieldErrors(err)
	} else {
		resp.Config = cfg
	}
	return resp, nil
}

func fieldErrors(err error) []config.FieldError {
	var el *config.ErrorList
	if errors.As(err, &el) {
		return el.Errors
	}
	return []config.FieldError{{Message: err.Error()}}
}

// writeConfigResponse answers status with the current ConfigResponse and its
// ETag; a file that is gone answers 404 config-missing.
func (s *server) writeConfigResponse(w http.ResponseWriter, status int) {
	resp, err := s.configResponse()
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", quote(resp.Etag))
	writeJSON(w, status, resp)
}

// writeStoreError maps a store error to its response.
func (s *server) writeStoreError(w http.ResponseWriter, err error) {
	var pre *store.ErrPreconditionFailed
	var el *config.ErrorList
	switch {
	case errors.As(err, &pre), errors.Is(err, store.ErrExists):
		s.writeConfigResponse(w, http.StatusPreconditionFailed)
	case errors.Is(err, store.ErrNotExist):
		writeProblem(w, http.StatusNotFound, "config-missing", "the config file does not exist")
	case errors.As(err, &el):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-config", "the config is not valid", el.Errors...)
	case errors.Is(err, yamledit.ErrInvalidPatch):
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
	case errors.Is(err, yamledit.ErrUnsupported):
		writeProblem(w, http.StatusUnprocessableEntity, "unsupported-yaml", err.Error())
	default:
		slog.Error("config storage", "err", err)
		writeProblem(w, http.StatusInternalServerError, "storage-error", "the config file could not be read or written; the server log has the details")
	}
}

func quote(etag string) string { return `"` + etag + `"` }

// parseEtag takes the first entity tag of an If-Match header, without W/ and
// quotes. "*" is returned as is.
func parseEtag(h string) string {
	h, _, _ = strings.Cut(h, ",")
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(h, "W/")
	return strings.Trim(h, `"`)
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	s.writeConfigResponse(w, http.StatusOK)
}

func (s *server) getConfigRaw(w http.ResponseWriter, _ *http.Request) {
	raw, etag, err := s.d.Store.Read()
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	noWriteDeadline(w)
	h := w.Header()
	h.Set("Content-Type", "text/yaml; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="pagewire.yaml"`)
	h.Set("Cache-Control", "no-store")
	h.Set("ETag", quote(etag))
	_, _ = w.Write(raw)
}

// putConfig creates the file (If-None-Match: *) or replaces it (If-Match).
// The body is a config object as JSON; the file is written as YAML.
// A pagewire/v1 body is accepted: the store migrates it to pagewire/v2 before
// writing, so the ETag answered is that of the v2 file.
func (s *server) putConfig(w http.ResponseWriter, r *http.Request) {
	readDeadline(w, s.d.readTimeout)
	inm, im := r.Header.Get("If-None-Match"), r.Header.Get("If-Match")
	if inm == "" && im == "" {
		writeProblem(w, http.StatusPreconditionRequired, "precondition-required", "send If-None-Match: * to create or If-Match to replace")
		return
	}
	if inm != "" && strings.TrimSpace(inm) != "*" {
		writeProblem(w, http.StatusBadRequest, "bad-request", "If-None-Match must be *")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	raw, err := jsonToYAML(body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	status := http.StatusOK
	if inm != "" {
		_, err = s.d.Store.Create(raw)
		status = http.StatusCreated
	} else {
		etag := parseEtag(im)
		if etag == "*" {
			_, etag, err = s.d.Store.Read()
		}
		if err == nil {
			_, err = s.d.Store.Replace(etag, raw)
		}
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeConfigResponse(w, status)
}

// jsonToYAML converts a JSON object to YAML, keeping its key order. The store's
// check then reports unknown keys and bad values with positions.
func jsonToYAML(body []byte) ([]byte, error) {
	if !json.Valid(body) || !bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
		return nil, errors.New("body must be a JSON object")
	}
	y, err := yaml.JSONToYAML(body)
	if err != nil {
		return nil, err
	}
	return append([]byte(yamlHeader), y...), nil
}

// patchConfig applies an RFC 7396 merge patch to the file's YAML.
func (s *server) patchConfig(w http.ResponseWriter, r *http.Request) {
	readDeadline(w, s.d.readTimeout)
	im := r.Header.Get("If-Match")
	if im == "" {
		writeProblem(w, http.StatusPreconditionRequired, "precondition-required", "send If-Match with the file's ETag")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/merge-patch+json" {
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported-media-type", "Content-Type must be application/merge-patch+json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	etag := parseEtag(im)
	if etag == "*" {
		if _, etag, err = s.d.Store.Read(); err != nil {
			s.writeStoreError(w, err)
			return
		}
	}
	if _, _, err := s.d.Store.Patch(etag, body); err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.writeConfigResponse(w, http.StatusOK)
}
