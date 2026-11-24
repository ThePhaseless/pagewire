package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ThePhaseless/printscan/internal/apitypes"
	"github.com/ThePhaseless/printscan/internal/config"
	"github.com/ThePhaseless/printscan/internal/printing"
)

const (
	recentJobs  = 20
	maxNameLen  = 255
	maxOptsSize = 64 << 10
)

// limitedBody records whether the upload limit was hit. The error itself may
// come back wrapped (or replaced) by the IPP client's HTTP round trip.
type limitedBody struct {
	r   io.Reader
	hit atomic.Bool
}

func (l *limitedBody) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		l.hit.Store(true)
	}
	return n, err
}

func (l *limitedBody) tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return l.hit.Load() || errors.As(err, &mbe)
}

func badOrder(w http.ResponseWriter) {
	writeProblem(w, http.StatusBadRequest, "multipart-order", "send multipart/form-data with exactly two parts: options, then file")
}

func tooLarge(w http.ResponseWriter, limitMiB int) {
	writeProblem(w, http.StatusRequestEntityTooLarge, "too-large", fmt.Sprintf("the upload limit is %d MiB", limitMiB))
}

// print accepts one document: an options part, then a file part.
func (s *server) print(w http.ResponseWriter, r *http.Request) {
	limitMiB := s.d.Config().Print.MaxUploadMiB
	body := &limitedBody{r: http.MaxBytesReader(w, r.Body, int64(limitMiB)<<20)}

	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		badOrder(w)
		return
	}
	mr := multipart.NewReader(body, params["boundary"])

	part, err := mr.NextPart()
	if err != nil || part.FormName() != "options" {
		if body.tooLarge(err) {
			tooLarge(w, limitMiB)
			return
		}
		badOrder(w)
		return
	}
	var opts apitypes.PrintOptions
	dec := json.NewDecoder(io.LimitReader(part, maxOptsSize))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&opts); err != nil {
		if body.tooLarge(err) {
			tooLarge(w, limitMiB)
			return
		}
		writeProblem(w, http.StatusBadRequest, "bad-request", "options: "+err.Error())
		return
	}
	if errs := validateOptions(opts); len(errs) > 0 {
		writeProblem(w, http.StatusUnprocessableEntity, "invalid-options", "the print options are not valid", errs...)
		return
	}

	file, err := mr.NextPart()
	if err != nil || file.FormName() != "file" {
		if body.tooLarge(err) {
			tooLarge(w, limitMiB)
			return
		}
		badOrder(w)
		return
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		if body.tooLarge(err) {
			tooLarge(w, limitMiB)
			return
		}
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	head = head[:n]
	format := printing.Detect(head, file.FileName())
	formats, err := s.d.Printer.Formats(r.Context())
	if err != nil {
		printerError(w, err)
		return
	}
	if err := printing.Negotiate(format, formats); err != nil {
		printerError(w, err)
		return
	}

	// shortcut: parts after file are never read (the job is already streaming by then);
	// reject them only if a client ever relies on sending more.
	name := jobName(file.FileName())
	id, err := s.d.Printer.Submit(r.Context(), name, userName(r), format, io.MultiReader(bytes.NewReader(head), file), opts)
	if err != nil {
		if body.tooLarge(err) {
			tooLarge(w, limitMiB)
			return
		}
		printerError(w, err)
		return
	}
	job, err := s.d.Printer.Job(r.Context(), id)
	if err != nil {
		job = apitypes.Job{ID: id, Name: name, State: "pending", Reasons: []string{}, CreatedAt: time.Now()}
	}
	s.hub.Publish(apitypes.EventJob, job)
	go s.watchJob(job)
	writeJSON(w, http.StatusAccepted, job)
}

func validateOptions(o apitypes.PrintOptions) []config.FieldError {
	var errs []config.FieldError
	bad := func(p, msg string) { errs = append(errs, config.FieldError{Path: p, Message: msg}) }
	if o.Copies < 1 || o.Copies > 99 {
		bad("/copies", "must be 1 to 99")
	}
	if !slices.Contains([]string{"one-sided", "two-sided-long-edge", "two-sided-short-edge"}, o.Sides) {
		bad("/sides", "must be one-sided, two-sided-long-edge or two-sided-short-edge")
	}
	if !slices.Contains([]string{"auto", "color", "monochrome"}, o.ColorMode) {
		bad("/colorMode", "must be auto, color or monochrome")
	}
	if o.Media == "" {
		bad("/media", "must be a media name from the printer's media-supported list")
	}
	if !slices.Contains([]string{"draft", "normal", "high"}, o.Quality) {
		bad("/quality", "must be draft, normal or high")
	}
	return errs
}

// cleanName removes control characters and trims s to at most 255 runes.
func cleanName(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
	if rs := []rune(s); len(rs) > maxNameLen {
		s = strings.TrimSpace(string(rs[:maxNameLen]))
	}
	return s
}

// jobName is the base name of an uploaded file (Windows paths included).
func jobName(filename string) string {
	n := cleanName(path.Base(strings.ReplaceAll(filename, `\`, "/")))
	if n == "" || n == "." || n == "/" {
		return "document"
	}
	return n
}

// userName is the auth proxy's user, else "web".
func userName(r *http.Request) string {
	if u := cleanName(r.Header.Get("X-Authentik-Username")); u != "" {
		return u
	}
	return "web"
}

func terminalJob(state string) bool {
	return state == "completed" || state == "canceled" || state == "aborted"
}

// watchJob polls a web-submitted job and publishes a job event on every state
// or reasons change, until a terminal state, ErrNotFound, JobWatchTimeout or
// BaseContext's end.
func (s *server) watchJob(last apitypes.Job) {
	if terminalJob(last.State) {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.d.JobWatchTimeout)
	defer cancel()
	t := time.NewTicker(s.d.JobPollInterval)
	defer t.Stop()
	warned := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		job, err := s.d.Printer.Job(ctx, last.ID)
		if errors.Is(err, printing.ErrNotFound) {
			return
		}
		if err != nil {
			if !warned && ctx.Err() == nil {
				slog.Warn("job watcher cannot read job; retrying", "job", last.ID, "err", err)
				warned = true
			}
			continue
		}
		if job.State != last.State || !slices.Equal(job.Reasons, last.Reasons) {
			s.hub.Publish(apitypes.EventJob, job)
			last = job
		}
		if terminalJob(job.State) {
			return
		}
	}
}

// printerError maps a Printer error to its response.
func printerError(w http.ResponseWriter, err error) {
	var unsup *printing.UnsupportedFormatError
	switch {
	case errors.As(err, &unsup):
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported-format",
			fmt.Sprintf("the printer does not accept %s; accepted formats: %s", unsup.Detected, strings.Join(unsup.Supported, ", ")))
	case errors.Is(err, printing.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "not-found", "no such job")
	case errors.Is(err, printing.ErrFinished):
		writeProblem(w, http.StatusConflict, "job-finished", "the job has already finished")
	case errors.Is(err, ErrNoPrinter):
		writeProblem(w, http.StatusServiceUnavailable, "printer-unavailable", "no printer is available")
	default:
		slog.Warn("printer request failed", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "printer-unavailable", err.Error())
	}
}

func jobID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	return id, err == nil && id > 0
}

func (s *server) getJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.d.Printer.Jobs(r.Context(), recentJobs)
	if err != nil {
		printerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(r)
	if !ok {
		printerError(w, printing.ErrNotFound)
		return
	}
	job, err := s.d.Printer.Job(r.Context(), id)
	if err != nil {
		printerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// deleteJob cancels a job and publishes its new state.
func (s *server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(r)
	if !ok {
		printerError(w, printing.ErrNotFound)
		return
	}
	if err := s.d.Printer.Cancel(r.Context(), id); err != nil {
		printerError(w, err)
		return
	}
	if job, err := s.d.Printer.Job(r.Context(), id); err == nil {
		s.hub.Publish(apitypes.EventJob, job)
	}
	w.WriteHeader(http.StatusNoContent)
}
