package api

import "net/http"

// getEULA answers GET /api/v1/eulas/{id}: the licence text the device agent
// read from the verified vendor archive, pending until it has reported it.
func (s *server) getEULA(w http.ResponseWriter, r *http.Request) {
	e, ok := s.d.Setup.EULA(r.PathValue("id"))
	if !ok {
		writeProblem(w, http.StatusNotFound, "not-found", "no such EULA in the catalog")
		return
	}
	writeJSON(w, http.StatusOK, e)
}
