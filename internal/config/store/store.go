// Package store reads and writes the config file with ETag preconditions.
// Writes are atomic (temporary file in the same directory, fsync, rename,
// fsync of the directory), a mutex serialises writers in this process and the
// ETag catches edits made outside it.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/renameio/v2"

	"github.com/ThePhaseless/pagewire/internal/config/yamledit"
)

var (
	// ErrNotExist means the config file does not exist.
	ErrNotExist = errors.New("config file does not exist")
	// ErrExists means Create found a config file already there.
	ErrExists = errors.New("config file already exists")
)

// ErrPreconditionFailed means the caller's ETag does not match the file. It is
// returned as a pointer: match it with errors.As(err, new(*ErrPreconditionFailed)).
type ErrPreconditionFailed struct {
	CurrentEtag string
}

func (e *ErrPreconditionFailed) Error() string {
	return fmt.Sprintf("config file changed (current etag %s)", e.CurrentEtag)
}

// Store is the config file at one path. validate checks a candidate file; its
// error (a *config.ErrorList for a bad config) is returned unwrapped and
// nothing is written. Any other error from a write method is a write failure.
type Store struct {
	path      string
	validate  func([]byte) error
	normalize func([]byte) ([]byte, error)
	mu        sync.Mutex
}

// Option configures a Store.
type Option func(*Store)

// WithNormalize makes every write normalise its bytes first: Create and
// Replace normalise the bytes they are given, Patch the current file before
// applying the patch. A normalize error is returned unwrapped, like a
// validation error, and nothing is written.
func WithNormalize(normalize func([]byte) ([]byte, error)) Option {
	return func(s *Store) { s.normalize = normalize }
}

// New returns a Store for path.
func New(path string, validate func([]byte) error, opts ...Option) *Store {
	s := &Store{path: path, validate: validate, normalize: func(b []byte) ([]byte, error) { return b, nil }}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Etag is the hex sha256 of the file's bytes.
func Etag(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// Read returns the file and its ETag, or ErrNotExist.
func (s *Store) Read() (raw []byte, etag string, err error) {
	raw, err = os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", ErrNotExist
	}
	if err != nil {
		return nil, "", fmt.Errorf("read config: %w", err)
	}
	return raw, Etag(raw), nil
}

// Create writes raw as a new file and returns its ETag, or ErrExists.
func (s *Store) Create(raw []byte) (etag string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.Read(); !errors.Is(err, ErrNotExist) {
		if err == nil {
			return "", ErrExists
		}
		return "", err
	}
	raw, err = s.normalize(raw)
	if err != nil {
		return "", err
	}
	return s.write(raw)
}

// Replace overwrites the file when etag matches it and returns the new ETag.
func (s *Store) Replace(etag string, raw []byte) (newEtag string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.current(etag); err != nil {
		return "", err
	}
	raw, err = s.normalize(raw)
	if err != nil {
		return "", err
	}
	return s.write(raw)
}

// Patch applies an RFC 7396 merge patch to the file's YAML when etag matches,
// keeping its comments. A patch body that is not a JSON object gives
// yamledit.ErrInvalidPatch, a construct Apply cannot edit gives
// yamledit.ErrUnsupported, and a file that does not parse gives its own
// validation error.
func (s *Store) Patch(etag string, mergePatch []byte) (newRaw []byte, newEtag string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.current(etag)
	if err != nil {
		return nil, "", err
	}
	base, err := s.normalize(cur)
	if err != nil {
		return nil, "", err
	}
	newRaw, err = yamledit.Apply(base, mergePatch)
	if err != nil {
		if !errors.Is(err, yamledit.ErrInvalidPatch) && !errors.Is(err, yamledit.ErrUnsupported) {
			// The current file is not valid YAML: report it with its positions.
			if verr := s.validate(cur); verr != nil {
				return nil, "", verr
			}
		}
		return nil, "", err
	}
	if bytes.Equal(newRaw, cur) {
		// Nothing to write, but the file must still pass validation.
		if err := s.validate(cur); err != nil {
			return nil, "", err
		}
		return cur, etag, nil
	}
	newEtag, err = s.write(newRaw)
	if err != nil {
		return nil, "", err
	}
	return newRaw, newEtag, nil
}

// current reads the file and checks etag against it.
func (s *Store) current(etag string) ([]byte, error) {
	cur, curEtag, err := s.Read()
	if err != nil {
		return nil, err
	}
	if curEtag != etag {
		return nil, &ErrPreconditionFailed{CurrentEtag: curEtag}
	}
	return cur, nil
}

// write validates raw and replaces the file atomically. The caller holds mu.
func (s *Store) write(raw []byte) (string, error) {
	if err := s.validate(raw); err != nil {
		return "", err
	}
	dir := filepath.Dir(s.path)
	pf, err := renameio.NewPendingFile(s.path, renameio.WithTempDir(dir), renameio.WithStaticPermissions(0o644))
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer func() { _ = pf.Cleanup() }()
	if _, err := pf.Write(raw); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	if err := pf.CloseAtomicallyReplace(); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	// renameio does not sync the directory, so the rename could be lost in a crash.
	d, err := os.Open(dir) //nolint:gosec // dir is the configured config directory
	if err != nil {
		return "", fmt.Errorf("sync config directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return "", fmt.Errorf("sync config directory: %w", err)
	}
	return Etag(raw), nil
}
