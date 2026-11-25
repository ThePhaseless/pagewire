package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
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
	case errors.Is(err, scanning.ErrNoScanner):
		writeProblem(w, http.StatusServiceUnavailable, "scanner-unavailable", err.Error())
	default:
		slog.Warn("scanner request failed", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "scanner-unavailable", err.Error())
	}
}

func (s *server) getScanner(w http.ResponseWriter, r *http.Request) {
	caps, err := s.d.Scans.Caps(r.Context())
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
	sc, err := s.d.Scans.Start(set)
	if err != nil {
		scanError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, sc)
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
