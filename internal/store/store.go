// Package store keeps the clipboard and file registries on disk.
//
// Each registry has a small JSON manifest plus one immutable data blob. A write
// streams the request body to a temporary file, fsyncs it, moves it to a fresh
// generation name, and finally atomically renames a new manifest into place.
// The manifest rename is the commit point: readers only ever see the previous
// item or the complete new one, never data without matching metadata. Orphan
// blobs and temporary files left behind by an interrupted write are removed at
// startup.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"central-portal/internal/model"
)

var (
	// ErrEmpty is returned when a registry has no content yet.
	ErrEmpty = errors.New("registry is empty")
	// ErrTooLarge is returned when an upload exceeds the size limit.
	ErrTooLarge = errors.New("upload too large")

	errInvalidManifest = errors.New("invalid registry metadata")
)

var kinds = []string{model.KindClipboard, model.KindFile}

// manifest is the on-disk metadata. Data names the immutable blob holding the
// content; an empty Data means the legacy directory/<kind> slot is still in
// use. The embedded Entry keeps the on-disk JSON compatible with the public
// metadata format.
type manifest struct {
	model.Entry
	Data string `json:"data,omitempty"`
}

// Store holds one item per registry.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// Open prepares dir for the registries and repairs state left behind by an
// interrupted write.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	s := &Store{dir: dir}
	s.removeTemps()
	for _, kind := range kinds {
		if err := s.repair(kind); err != nil {
			return nil, fmt.Errorf("repair %s registry: %w", kind, err)
		}
	}
	return s, nil
}

// Get returns the metadata of a registry without opening its content.
func (s *Store) Get(kind string) (model.Entry, bool) {
	if !valid(kind) {
		return model.Entry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.entry(kind)
}

// Open opens the content of a registry for reading. The caller must close the
// returned file.
func (s *Store) Open(kind string) (*os.File, model.Entry, error) {
	if !valid(kind) {
		return nil, model.Entry{}, fmt.Errorf("unknown registry %q", kind)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.readManifest(kind)
	if errors.Is(err, ErrEmpty) {
		return nil, model.Entry{}, ErrEmpty
	}
	if err != nil {
		return nil, model.Entry{}, err
	}
	f, err := os.Open(s.contentPath(kind, m))
	if errors.Is(err, os.ErrNotExist) {
		return nil, model.Entry{}, ErrEmpty
	}
	if err != nil {
		return nil, model.Entry{}, err
	}
	return f, m.Entry, nil
}

// Put replaces the content of a registry. The name and contentType are stored
// as metadata only and are never used as paths. maxBytes must be positive.
//
// The request body is streamed outside the store lock, so a slow upload cannot
// block status requests or downloads. Only the final manifest commit is
// serialized.
func (s *Store) Put(kind, name, contentType string, r io.Reader, maxBytes int64) (model.Entry, error) {
	if !valid(kind) {
		return model.Entry{}, fmt.Errorf("unknown registry %q", kind)
	}
	if maxBytes <= 0 {
		return model.Entry{}, errors.New("size limit must be positive")
	}
	// maxBytes+1 overflows for math.MaxInt64. Capping the reader at maxBytes
	// itself is safe because an int64 copy can never yield more than that.
	limit := maxBytes
	if maxBytes < math.MaxInt64 {
		limit = maxBytes + 1
	}

	tmp, err := os.CreateTemp(s.dir, kind+".tmp-*")
	if err != nil {
		return model.Entry{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(r, limit))
	if err != nil {
		tmp.Close()
		return model.Entry{}, err
	}
	if n > maxBytes {
		tmp.Close()
		return model.Entry{}, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return model.Entry{}, err
	}
	if err := tmp.Close(); err != nil {
		return model.Entry{}, err
	}

	gen, err := newGeneration()
	if err != nil {
		return model.Entry{}, err
	}
	blob := kind + "." + gen + ".data"
	blobPath := filepath.Join(s.dir, blob)
	if err := os.Rename(tmpName, blobPath); err != nil {
		return model.Entry{}, err
	}
	committed := false
	defer func() {
		if !committed {
			os.Remove(blobPath)
		}
	}()
	// Make the blob durable before a manifest can reference it.
	if err := syncDir(s.dir); err != nil {
		return model.Entry{}, err
	}

	e := model.Entry{
		Name:        name,
		Size:        n,
		SHA256:      hex.EncodeToString(hash.Sum(nil)),
		ContentType: contentType,
		UpdatedAt:   time.Now().UTC(),
	}
	m := manifest{Entry: e, Data: blob}

	// Serialize commits. Streaming and blob creation above happen without the
	// lock; only the manifest swap and cleanup are exclusive.
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, prevErr := s.readManifest(kind)
	if err := writeAtomic(s.metaPath(kind), mustJSON(m), 0o600); err != nil {
		return model.Entry{}, err
	}
	committed = true
	if err := syncDir(s.dir); err != nil {
		return model.Entry{}, err
	}
	if prevErr == nil && prev.Data != "" && prev.Data != blob {
		// The replaced generation is unreachable now. Readers that already
		// opened it keep their file descriptor; failure to unlink is harmless.
		os.Remove(filepath.Join(s.dir, prev.Data))
	}
	return e, nil
}

func (s *Store) entry(kind string) (model.Entry, bool) {
	m, err := s.readManifest(kind)
	if err != nil {
		return model.Entry{}, false
	}
	return m.Entry, true
}

func (s *Store) dataPath(kind string) string { return filepath.Join(s.dir, kind) }
func (s *Store) metaPath(kind string) string { return filepath.Join(s.dir, kind+".json") }

func (s *Store) contentPath(kind string, m manifest) string {
	if m.Data != "" {
		return filepath.Join(s.dir, m.Data)
	}
	return s.dataPath(kind)
}

// readManifest loads and validates the metadata of one registry. A missing
// manifest reports ErrEmpty. The clipboard content type is defaulted for
// entries written before that field existed.
func (s *Store) readManifest(kind string) (manifest, error) {
	b, err := os.ReadFile(s.metaPath(kind))
	if errors.Is(err, os.ErrNotExist) {
		return manifest{}, ErrEmpty
	}
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return manifest{}, fmt.Errorf("%w: %v", errInvalidManifest, err)
	}
	if m.Size < 0 || m.UpdatedAt.IsZero() {
		return manifest{}, errInvalidManifest
	}
	if m.Data != "" && !validDataName(kind, m.Data) {
		return manifest{}, errInvalidManifest
	}
	if kind == model.KindClipboard && m.ContentType == "" {
		m.ContentType = model.ContentTypeText
	}
	return m, nil
}

// repair makes the manifest agree with the data blob at startup, adopting the
// legacy layout when necessary and removing unreferenced generation blobs.
func (s *Store) repair(kind string) error {
	m, err := s.readManifest(kind)
	switch {
	case err == nil:
		if m.Data == "" {
			return s.repairLegacyManifest(kind, m)
		}
		return s.repairCommitted(kind, m)
	case errors.Is(err, ErrEmpty), errors.Is(err, errInvalidManifest):
		return s.adoptLegacy(kind)
	default:
		return err
	}
}

// repairCommitted checks the blob named by a committed manifest. The manifest
// is the commit point, so a missing blob means the committed content is gone
// and the obsolete legacy slot must not be resurrected.
func (s *Store) repairCommitted(kind string, m manifest) error {
	blobPath := filepath.Join(s.dir, m.Data)
	fi, err := os.Stat(blobPath)
	if errors.Is(err, os.ErrNotExist) {
		return s.dropManifest(kind)
	}
	if err != nil {
		return err
	}
	if fi.Size() != m.Size {
		m.Size = fi.Size()
		m.UpdatedAt = fi.ModTime().UTC()
		if kind == model.KindClipboard {
			m.SHA256, _ = hashFile(blobPath)
			if m.ContentType == "" {
				m.ContentType = model.ContentTypeText
			}
		}
		if err := writeAtomic(s.metaPath(kind), mustJSON(m), 0o600); err != nil {
			return err
		}
	}
	// The committed generation supersedes the legacy data slot.
	if err := removeIfExists(s.dataPath(kind)); err != nil {
		return err
	}
	return s.removeOrphanBlobs(kind, m.Data)
}

// repairLegacyManifest handles a manifest written before generation blobs.
func (s *Store) repairLegacyManifest(kind string, m manifest) error {
	fi, err := os.Stat(s.dataPath(kind))
	if errors.Is(err, os.ErrNotExist) {
		return s.dropManifest(kind)
	}
	if err != nil {
		return err
	}
	if fi.Size() != m.Size {
		if err := s.rebuildLegacy(kind, fi); err != nil {
			return err
		}
	}
	return s.removeOrphanBlobs(kind, "")
}

// adoptLegacy rebuilds metadata from a legacy data file when no usable
// manifest exists.
func (s *Store) adoptLegacy(kind string) error {
	fi, err := os.Stat(s.dataPath(kind))
	if errors.Is(err, os.ErrNotExist) {
		return s.dropManifest(kind)
	}
	if err != nil {
		return err
	}
	if err := s.rebuildLegacy(kind, fi); err != nil {
		return err
	}
	return s.removeOrphanBlobs(kind, "")
}

func (s *Store) dropManifest(kind string) error {
	if err := removeIfExists(s.metaPath(kind)); err != nil {
		return err
	}
	if err := removeIfExists(s.dataPath(kind)); err != nil {
		return err
	}
	return s.removeOrphanBlobs(kind, "")
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) rebuildLegacy(kind string, fi os.FileInfo) error {
	e := model.Entry{Size: fi.Size(), UpdatedAt: fi.ModTime().UTC()}
	if kind == model.KindClipboard {
		e.SHA256, _ = hashFile(s.dataPath(kind))
		e.ContentType = model.ContentTypeText
	}
	if kind == model.KindFile {
		e.Name = "file"
	}
	return writeAtomic(s.metaPath(kind), mustJSON(manifest{Entry: e}), 0o600)
}

// removeOrphanBlobs deletes generation blobs of kind that are not referenced
// by keep. Legacy data files and temporary files are not touched here.
func (s *Store) removeOrphanBlobs(kind, keep string) error {
	matches, err := filepath.Glob(filepath.Join(s.dir, kind+".*.data"))
	if err != nil {
		return err
	}
	for _, name := range matches {
		base := filepath.Base(name)
		if base == keep || !validDataName(kind, base) {
			continue
		}
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) removeTemps() {
	for _, pattern := range []string{"*.tmp-*", "*.meta-*"} {
		matches, _ := filepath.Glob(filepath.Join(s.dir, pattern))
		for _, name := range matches {
			os.Remove(name)
		}
	}
}

func valid(kind string) bool {
	return kind == model.KindClipboard || kind == model.KindFile
}

// validDataName accepts only generation blobs belonging to kind and located
// directly in the data directory.
func validDataName(kind, name string) bool {
	if name == "" || filepath.Base(name) != name {
		return false
	}
	prefix := kind + "."
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".data") {
		return false
	}
	gen := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".data")
	return gen != ""
}

func newGeneration() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func hashFile(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeAtomic(target string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
