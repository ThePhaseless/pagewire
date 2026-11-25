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
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ThePhaseless/pagewire/internal/apitypes"
	"github.com/ThePhaseless/pagewire/internal/config"
	"github.com/ThePhaseless/pagewire/internal/device"
	"github.com/ThePhaseless/pagewire/internal/printing"
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

// print accepts one document for printer {id}: an options part, then a file
// part. Options the printer does not offer are left out and come back as
// warnings; while its capabilities are unknown they are sent as given.
func (s *server) print(w http.ResponseWriter, r *http.Request) {
	readDeadline(w, s.d.printReadTimeout)
	printerID := r.PathValue("id")
	printer, err := s.d.Printers.Printer(printerID)
	if err != nil {
		printerError(w, err)
		return
	}
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
	warnings := []apitypes.Warning{}
	if caps, ok := s.d.PrinterCaps(printerID); ok {
		opts, warnings = fitOptions(opts, caps)
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
	formats, err := printer.Formats(r.Context())
	if err != nil {
		printerError(w, err)
		return
	}
	target, err := printing.Negotiate(format, formats)
	if err != nil {
		printerError(w, err)
		return
	}

	// A slot for the job's watcher, taken before any work and given back
	// unless the watcher starts.
	select {
	case s.watchers <- struct{}{}:
	default:
		writeProblem(w, http.StatusTooManyRequests, "too-many-jobs",
			fmt.Sprintf("pagewire is already following %d print jobs; try again when one finishes", maxJobWatchers))
		return
	}
	watching := false
	defer func() {
		if !watching {
			<-s.watchers
		}
	}()

	// shortcut: parts after file are never read (the job is already streaming by then);
	// reject them only if a client ever relies on sending more.
	doc := io.MultiReader(bytes.NewReader(head), file)
	if target != format {
		converted, cleanup, err := convertUpload(doc, format, target)
		defer cleanup()
		if err != nil {
			if body.tooLarge(err) {
				tooLarge(w, limitMiB)
				return
			}
			conversionError(w, err)
			return
		}
		doc = converted
	}
	name := jobName(file.FileName())
	id, err := printer.Submit(r.Context(), name, userName(r), target, doc, opts)
	if err != nil {
		if body.tooLarge(err) {
			tooLarge(w, limitMiB)
			return
		}
		printerError(w, err)
		return
	}
	job, err := printer.Job(r.Context(), id)
	if err != nil {
		job = apitypes.Job{ID: id, Name: name, State: "pending", Reasons: []string{}, CreatedAt: time.Now()}
	}
	job.Printer = printerID
	s.hub.Publish(apitypes.EventJob, job)
	watching = true
	go func() {
		defer func() { <-s.watchers }()
		s.watchJob(printer, job)
	}()
	writeJSON(w, http.StatusAccepted, apitypes.PrintResult{Job: job, Warnings: warnings})
}

// fitOptions leaves out the options caps does not offer and clamps copies to
// the printer's maximum, with one warning for each.
func fitOptions(o apitypes.PrintOptions, caps device.PrinterCaps) (apitypes.PrintOptions, []apitypes.Warning) {
	var dropped []device.Dropped
	o.PrintSettings, dropped = device.FitPrint(o.PrintSettings, caps)
	if caps.CopiesMax > 0 && o.Copies > caps.CopiesMax {
		dropped = append(dropped, device.Dropped{Option: device.OptionCopies, Value: strconv.Itoa(o.Copies), Using: strconv.Itoa(caps.CopiesMax)})
		o.Copies = caps.CopiesMax
	}
	warnings := []apitypes.Warning{}
	for _, d := range dropped {
		using := d.Using
		if using == "" {
			using = "its own default"
		}
		warnings = append(warnings, apitypes.Warning{
			Option: d.Option, Value: d.Value, Using: d.Using,
			Message: fmt.Sprintf("%s %s is not offered by this printer; it uses %s", d.Option, d.Value, using),
		})
	}
	return o, warnings
}

// convertUpload spools the upload to a temp file (disk, never memory),
// converts it into a second one and returns that, rewound. cleanup removes
// both; call it even on error.
func convertUpload(doc io.Reader, from, to string) (io.Reader, func(), error) {
	var files []*os.File
	cleanup := func() {
		for _, f := range files {
			_ = f.Close()
			if err := os.Remove(f.Name()); err != nil {
				slog.Warn("cannot remove print spool file", "path", f.Name(), "err", err)
			}
		}
	}
	temp := func() (*os.File, error) {
		f, err := os.CreateTemp("", "pagewire-print-*")
		if err == nil {
			files = append(files, f)
		}
		return f, err
	}
	in, err := temp()
	if err != nil {
		return nil, cleanup, err
	}
	if _, err := io.Copy(in, doc); err != nil {
		return nil, cleanup, &uploadError{err}
	}
	out, err := temp()
	if err != nil {
		return nil, cleanup, err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return nil, cleanup, err
	}
	if err := printing.Convert(out, in, from, to); err != nil {
		return nil, cleanup, err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, cleanup, err
	}
	return out, cleanup, nil
}

// uploadError is a failure reading the upload while spooling it.
type uploadError struct{ err error }

func (e *uploadError) Error() string { return "read upload: " + e.err.Error() }
func (e *uploadError) Unwrap() error { return e.err }

// unsupportedJPEGDetail is the 415 detail for a JPEG the PDF writer cannot embed.
const unsupportedJPEGDetail = "This JPEG uses a colour format the printer path cannot embed. Save it as an RGB JPEG or a PDF."

// conversionError maps a convertUpload error to its response.
func conversionError(w http.ResponseWriter, err error) {
	var invalid *printing.InvalidImageError
	var large *printing.ImageTooLargeError
	var upload *uploadError
	switch {
	case errors.As(err, &upload):
		writeProblem(w, http.StatusBadRequest, "bad-request", "the upload could not be read")
	case errors.Is(err, printing.ErrUnsupportedJPEG):
		writeProblem(w, http.StatusUnsupportedMediaType, "unsupported-format", unsupportedJPEGDetail)
	case errors.As(err, &invalid):
		writeProblem(w, http.StatusBadRequest, "invalid-image", "the file is damaged or not the image it claims to be")
	case errors.As(err, &large):
		writeProblem(w, http.StatusRequestEntityTooLarge, "image-too-large",
			fmt.Sprintf("the image is %d x %d pixels, too large to convert; scale it down, or save it as a JPEG or a PDF", large.Width, large.Height))
	case errors.Is(err, printing.ErrBusy):
		writeProblem(w, http.StatusTooManyRequests, "too-many-jobs", "another image is being converted; try again in a moment")
	default:
		slog.Error("print conversion", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal-error", "the file could not be converted; the server log has the details")
	}
}

// Syntactic rules of the print options, the same as the config defaults'.
var (
	pwgMediaRe = regexp.MustCompile(`^[a-z0-9-]+_[a-z0-9.-]+_[0-9.]+x[0-9.]+(mm|in)$`)
	keywordRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
)

// validateOptions checks every option syntactically; an empty string or 0
// is absent and passes. The printer decides later which values it offers.
func validateOptions(o apitypes.PrintOptions) []config.FieldError {
	var errs []config.FieldError
	bad := func(p, msg string) { errs = append(errs, config.FieldError{Path: p, Message: msg}) }
	oneOf := func(p, v string, values ...string) {
		if v != "" && !slices.Contains(values, v) {
			bad(p, "must be "+strings.Join(values[:len(values)-1], ", ")+" or "+values[len(values)-1])
		}
	}
	keyword := func(p, v string) {
		if v != "" && !keywordRe.MatchString(v) {
			bad(p, "must be a lower-case IPP keyword")
		}
	}
	between := func(p string, v, lo, hi int) {
		if v != 0 && (v < lo || v > hi) {
			bad(p, fmt.Sprintf("must be %d to %d", lo, hi))
		}
	}
	if o.Copies < 0 || o.Copies > 99 {
		bad("/copies", "must be 0 to 99")
	}
	if o.Media != "" && !pwgMediaRe.MatchString(o.Media) {
		bad("/media", "must be a PWG media name such as iso_a4_210x297mm")
	}
	keyword("/mediaSource", o.MediaSource)
	keyword("/mediaType", o.MediaType)
	oneOf("/sides", o.Sides, "one-sided", "two-sided-long-edge", "two-sided-short-edge")
	oneOf("/colorMode", o.ColorMode, "auto", "color", "monochrome")
	oneOf("/quality", o.Quality, "draft", "normal", "high")
	between("/resolution", o.Resolution, 75, 2400)
	oneOf("/contentOptimize", o.ContentOptimize, "auto", "graphic", "photo", "text", "text-and-graphic")
	oneOf("/orientation", o.Orientation, "portrait", "landscape", "reverse-landscape", "reverse-portrait")
	between("/numberUp", o.NumberUp, 1, 16)
	oneOf("/printScaling", o.PrintScaling, "auto", "auto-fit", "fill", "fit", "none")
	keyword("/outputBin", o.OutputBin)
	keyword("/finishings", o.Finishings)
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
func (s *server) watchJob(p Printer, last apitypes.Job) {
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
		job, err := p.Job(ctx, last.ID)
		job.Printer = last.Printer
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
	case errors.Is(err, ErrUnknownPrinter):
		writeProblem(w, http.StatusNotFound, "printer-not-found", "no enabled printer has this id")
	case errors.Is(err, ErrNoPrinter):
		writeProblem(w, http.StatusServiceUnavailable, "printer-unavailable", "no printer is available")
	default:
		slog.Warn("printer request failed", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "printer-unavailable", "the printer did not answer; the server log has the details")
	}
}

func jobID(r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("job"))
	return id, err == nil && id > 0
}

// printerJob resolves {id} and {job}; ok false means the error is written.
func (s *server) printerJob(w http.ResponseWriter, r *http.Request) (Printer, int, bool) {
	p, err := s.d.Printers.Printer(r.PathValue("id"))
	if err != nil {
		printerError(w, err)
		return nil, 0, false
	}
	id, ok := jobID(r)
	if !ok {
		printerError(w, printing.ErrNotFound)
	}
	return p, id, ok
}

func (s *server) getJobs(w http.ResponseWriter, r *http.Request) {
	printerID := r.PathValue("id")
	p, err := s.d.Printers.Printer(printerID)
	if err != nil {
		printerError(w, err)
		return
	}
	jobs, err := p.Jobs(r.Context(), recentJobs)
	if err != nil {
		printerError(w, err)
		return
	}
	for i := range jobs {
		jobs[i].Printer = printerID
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	p, id, ok := s.printerJob(w, r)
	if !ok {
		return
	}
	job, err := p.Job(r.Context(), id)
	if err != nil {
		printerError(w, err)
		return
	}
	job.Printer = r.PathValue("id")
	writeJSON(w, http.StatusOK, job)
}

// deleteJob cancels a job and publishes its new state.
func (s *server) deleteJob(w http.ResponseWriter, r *http.Request) {
	p, id, ok := s.printerJob(w, r)
	if !ok {
		return
	}
	if err := p.Cancel(r.Context(), id); err != nil {
		printerError(w, err)
		return
	}
	if job, err := p.Job(r.Context(), id); err == nil {
		job.Printer = r.PathValue("id")
		s.hub.Publish(apitypes.EventJob, job)
	}
	w.WriteHeader(http.StatusNoContent)
}
