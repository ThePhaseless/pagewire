package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"github.com/ThePhaseless/pagewire/internal/blobs"
	"github.com/ThePhaseless/pagewire/internal/catalog"
	"github.com/ThePhaseless/pagewire/internal/device"
)

// FetchConfig holds the `agent fetch` flags.
type FetchConfig struct {
	Dir       string       // --dir: the blob volume
	Blobs     []string     // --blob <id>@<version>, repeated
	Accept    []string     // --accept <eula id>@<version>@<text sha256>, repeated
	Optional  []string     // --optional <id>, repeated: sets Optional on the --blob ref with that id
	StatusOut string       // --status-out; "" means device.BlobStatusFile
	Client    *http.Client // nil: a client with a 10 min timeout, so a stalled vendor cannot hang the init container

	// Test seams; nil means the embedded catalog.
	blob func(id string) (catalog.Blob, bool)
	key  func(id string) (openpgp.EntityList, error)
}

// Fetch installs every --blob ref, writes all their statuses as a JSON array
// to StatusOut (also when some fail), and returns an error, meaning exit 1,
// when a non-optional blob failed, a flag is malformed or the status file
// cannot be written. A failed optional blob only shows in the status file.
func Fetch(ctx context.Context, cfg FetchConfig) error {
	refs, err := parseRefs(cfg.Blobs)
	if err != nil {
		return err
	}
	accepted, err := parseAccepted(cfg.Accept)
	if err != nil {
		return err
	}
	for _, id := range cfg.Optional {
		i := slices.IndexFunc(refs, func(r device.BlobRef) bool { return r.ID == id })
		if i < 0 {
			return fmt.Errorf("--optional %s names no --blob", id)
		}
		refs[i].Optional = true
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	blob, key := cfg.blob, cfg.key
	if blob == nil {
		blob = catalog.Default().Blob
	}
	if key == nil {
		key = catalog.BlobKey
	}

	statuses := make([]device.BlobStatus, 0, len(refs))
	var failed []string
	for _, ref := range refs {
		st := fetchOne(ctx, ref, accepted, cfg, blob, key)
		statuses = append(statuses, st)
		slog.Info("blob", "id", st.ID, "version", st.Version, "state", st.State, "message", st.Message, "optional", ref.Optional)
		if st.State == device.BlobFailed && !ref.Optional {
			failed = append(failed, ref.ID+": "+st.Message)
		}
	}
	out := cfg.StatusOut
	if out == "" {
		out = device.BlobStatusFile
	}
	if err := WriteJSON(out, statuses); err != nil {
		return fmt.Errorf("write blob status: %w", err)
	}
	if len(failed) > 0 {
		return errors.New("required blob failed: " + strings.Join(failed, "; "))
	}
	return nil
}

func fetchOne(ctx context.Context, ref device.BlobRef, accepted []device.EULARef, cfg FetchConfig,
	blob func(string) (catalog.Blob, bool), key func(string) (openpgp.EntityList, error),
) device.BlobStatus {
	b, ok := blob(ref.ID)
	if !ok {
		return device.BlobStatus{ID: ref.ID, Version: ref.Version, State: device.BlobFailed, Message: "unknown blob " + ref.ID}
	}
	opt := blobs.Options{Client: cfg.Client}
	if b.Enabled && b.Verify.PGPKey != "" {
		keys, err := key(ref.ID)
		if err != nil {
			return device.BlobStatus{ID: ref.ID, Version: ref.Version, State: device.BlobFailed, Message: err.Error()}
		}
		opt.Keys = keys
	}
	return blobs.Fetch(ctx, b, accepted, cfg.Dir, opt)
}

func parseRefs(ss []string) ([]device.BlobRef, error) {
	refs := make([]device.BlobRef, 0, len(ss))
	for _, s := range ss {
		id, version, ok := strings.Cut(s, "@")
		if !ok || id == "" || version == "" {
			return nil, fmt.Errorf("blob ref %q: want <id>@<version>", s)
		}
		refs = append(refs, device.BlobRef{ID: id, Version: version})
	}
	return refs, nil
}

var textSHARe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseAccepted reads --accept values: <eula id>@<version>@<64 lowercase hex>.
func parseAccepted(ss []string) ([]device.EULARef, error) {
	refs := make([]device.EULARef, 0, len(ss))
	for _, s := range ss {
		parts := strings.Split(s, "@")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || !textSHARe.MatchString(parts[2]) {
			return nil, fmt.Errorf("--accept %q: want <id>@<version>@<sha256 of the licence text, 64 lowercase hex>", s)
		}
		refs = append(refs, device.EULARef{ID: parts[0], Version: parts[1], TextSHA256: parts[2]})
	}
	return refs, nil
}
