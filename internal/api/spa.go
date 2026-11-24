package api

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

const indexHTML = "index.html"

// hashedAsset matches the build's content-hashed files (assets/<name>-<hash>.<ext>).
var hashedAsset = regexp.MustCompile(`^assets/[^/]+-[a-z0-9]{8}\.[a-z0-9.]+$`)

// spa serves the built UI: existing files as they are, every other path as
// index.html (history routing). index.html is no-cache, hashed assets immutable.
func (s *server) spa(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || !fs.ValidPath(name) || !isFile(s.d.UI, name) {
		name = indexHTML
	}
	if !isFile(s.d.UI, indexHTML) {
		http.Error(w, "UI not built", http.StatusServiceUnavailable)
		return
	}
	if hashedAsset.MatchString(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.d.UI, name) //nolint:gosec // fs.ValidPath-checked name inside an fs.FS, which cannot escape its root
}

func isFile(fsys fs.FS, name string) bool {
	if fsys == nil {
		return false
	}
	st, err := fs.Stat(fsys, name)
	return err == nil && st.Mode().IsRegular()
}
