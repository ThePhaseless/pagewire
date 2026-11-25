package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/scanning"
)

// scanError maps a scan service error to its response.
func scanError(w http.ResponseWriter, err error) {
	var se *scanning.StorageError
	switch {
	case errors.Is(err, scanning.ErrInvalidSettings):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-settings", err.Error())
	case errors.Is(err, scanning.ErrBusy):
		writeProblem(w, http.StatusConflict, "scanner-busy", err.Error())
	case errors.Is(err, scanning.ErrRunning):
		writeProblem(w, http.StatusConflict, "scan-running", err.Error())
	case errors.Is(err, scanning.ErrNotFinished):
		writeProblem(w, http.StatusConflict, "scan-not-finished", err.Error())
	case errors.Is(err, scanning.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not-found", err.Error())
	case errors.Is(err, scanning.ErrStorageFull):
		writeProblem(w, http.StatusInsufficientStorage, "storage-full", err.Error())
	case errors.As(err, &se):
		slog.Error("scan storage", "err", err)
		writeProblem(w, http.StatusInternalServerError, "storage-error", "the scan could not be stored; the server log has the details")
	case errors.Is(err, ErrUnknownScanner):
		writeProblem(w, http.StatusNotFound, "scanner-not-found", "no enabled scanner has this id")
	case errors.Is(err, scanning.ErrNoScanner):
		writeProblem(w, http.StatusServiceUnavailable, "scanner-unavailable", err.Error())
	default:
		slog.Warn("scanner request failed", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "scanner-unavailable", err.Error())
	}
}

func (s *server) getScannerCaps(w http.ResponseWriter, r *http.Request) {
	caps, err := s.d.Scans.Caps(r.Context(), r.PathValue("id"))
	if err != nil {
		scanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, caps)
}

func (s *server) postScan(w http.ResponseWriter, r *http.Request) {
	readDeadline(w, s.d.readTimeout)
	var set apitypes.ScanSettings
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxOptsSize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	if err := scanning.Validate(set); err != nil {
		scanError(w, err)
		return
	}
	caps, err := s.d.Scans.Caps(r.Context(), set.Scanner)
	switch {
	case errors.Is(err, ErrUnknownScanner):
		scanError(w, err)
		return
	case err == nil:
		if errs := unsupportedSettings(set, caps); len(errs) > 0 {
			writeProblem(w, http.StatusUnprocessableEntity, "invalid-settings", "the scanner does not offer these settings", errs...)
			return
		}
	default:
		// Caps unreadable: the scan itself checks its input against the scanner.
		slog.Debug("scan caps unavailable; starting without the caps check", "scanner", set.Scanner, "err", err)
	}
	sc, err := s.d.Scans.Start(set)
	if err != nil {
		scanError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sc)
}

// unsupportedSettings are the settings caps does not offer. A resolution or
// colour mode is checked only when the scanner lists any, as the scan itself
// does not check them.
func unsupportedSettings(set apitypes.ScanSettings, caps apitypes.ScannerCaps) []config.FieldError {
	var errs []config.FieldError
	if !slices.Contains(caps.Sources, set.Source) {
		errs = append(errs, config.FieldError{Path: "/source", Message: "the scanner has no " + set.Source + " input"})
	}
	if len(caps.Resolutions) > 0 && !slices.Contains(caps.Resolutions, set.Resolution) {
		errs = append(errs, config.FieldError{Path: "/resolution", Message: fmt.Sprintf("the scanner does not offer %d dpi", set.Resolution)})
	}
	if len(caps.ColorModes) > 0 && !slices.Contains(caps.ColorModes, set.ColorMode) {
		errs = append(errs, config.FieldError{Path: "/colorMode", Message: "the scanner does not offer " + set.ColorMode})
	}
	if set.Intent != "" && !slices.Contains(caps.Intents, set.Intent) {
		errs = append(errs, config.FieldError{Path: "/intent", Message: "the scanner does not offer the " + set.Intent + " intent"})
	}
	return errs
}

func (s *server) getScans(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.d.Scans.List())
}

func (s *server) getScan(w http.ResponseWriter, r *http.Request) {
	sc, err := s.d.Scans.Get(r.PathValue("id"))
	if err != nil {
		scanError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *server) getScanPage(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		scanError(w, scanning.ErrNotFound)
		return
	}
	f, err := s.d.Scans.Page(r.PathValue("id"), n)
	if err != nil {
		scanError(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	noWriteDeadline(w)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := io.Copy(w, f); err != nil {
		slog.Debug("scan page write", "err", err)
	}
}

func (s *server) getScanDownload(w http.ResponseWriter, r *http.Request) {
	d, err := s.d.Scans.Download(r.PathValue("id"))
	if err != nil {
		scanError(w, err)
		return
	}
	noWriteDeadline(w)
	w.Header().Set("Content-Type", d.ContentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+d.Name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := d.Write(w); err != nil {
		slog.Warn("scan download write", "err", err)
	}
}

// deleteScan cancels a running scan (202 with the scan as it is now; a scan
// SSE event carries its canceled state) and deletes a finished one (204).
func (s *server) deleteScan(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sc, err := s.d.Scans.Cancel(id)
	if err == nil {
		writeJSON(w, http.StatusAccepted, sc)
		return
	}
	if errors.Is(err, scanning.ErrNotRunning) {
		err = s.d.Scans.Delete(id)
	}
	if err != nil {
		scanError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
