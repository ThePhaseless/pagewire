package api

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ThePhaseless/printscan/internal/device"
)

const maxReportBody = 1 << 20

// agentReport is POST /internal/v1/agent/report: the agent's report in, the
// desired state out.
func (s *server) agentReport(w http.ResponseWriter, r *http.Request) {
	if !s.agentAuthorized(r) {
		writeProblem(w, http.StatusUnauthorized, "unauthorized", "missing or wrong agent token")
		return
	}
	var rep device.AgentReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReportBody)).Decode(&rep); err != nil {
		writeProblem(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	ds, err := s.d.Agent.Report(r.Context(), rep)
	if err != nil {
		slog.Error("agent report", "err", err)
		writeProblem(w, http.StatusInternalServerError, "internal-error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

// agentAuthorized compares the bearer token in constant time. No token
// source, a failing one or an empty token refuses every request.
func (s *server) agentAuthorized(r *http.Request) bool {
	if s.d.Agent.Tokens == nil || s.d.Agent.Report == nil {
		return false
	}
	// The header first: a request without a bearer token costs no token read.
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || got == "" || !strings.EqualFold(scheme, device.TokenScheme) {
		return false
	}
	want, err := s.d.Agent.Tokens.AgentToken(r.Context())
	if err != nil {
		slog.Warn("agent token unavailable", "err", err)
		return false
	}
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
