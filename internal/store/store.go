// Package store keeps the clipboard and file registries on disk.
//
// Each registry is a single data file plus a small JSON metadata file. Writes
// go to a temporary file first and are renamed into place, so a reader either
// sees the previous item or the new one. Metadata is repaired at startup if a
// crash left it missing or stale.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"central-portal/internal/model"
)

var (
	// ErrEmpty is returned when a registry has no content yet.
	ErrEmpty = errors.New("registry is empty")
	// ErrTooLarge is returned when an upload exceeds the size limit.
	ErrTooLarge = errors.New("upload too large")
)

var kinds = []string{model.KindClipboard, model.KindFile}

// Store holds one item per registry.
type Store struct {
	dir string
	mu  sync.RWMutex
}

// Open prepares dir for the registries and repairs metadata left behind by an
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
	e, ok := s.entry(kind)
	if !ok {
		return nil, model.Entry{}, ErrEmpty
	}
	f, err := os.Open(s.dataPath(kind))
	if errors.Is(err, os.ErrNotExist) {
		return nil, model.Entry{}, ErrEmpty
	}
	if err != nil {
		return nil, model.Entry{}, err
	}
	return f, e, nil
}

// Put replaces the content of a registry. The name and contentType are stored
// as metadata only and are never used as paths. maxBytes must be positive.
func (s *Store) Put(kind, name, contentType string, r io.Reader, maxBytes int64) (model.Entry, error) {
	if !valid(kind) {
		return model.Entry{}, fmt.Errorf("unknown registry %q", kind)
	}
	if maxBytes <= 0 {
		return model.Entry{}, errors.New("size limit must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, err := os.CreateTemp(s.dir, kind+".tmp-*")
	if err != nil {
		return model.Entry{}, err
	}
	defer os.Remove(tmp.Name())

	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(r, maxBytes+1))
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

	e := model.Entry{
		Name:        name,
		Size:        n,
		SHA256:      hex.EncodeToString(hash.Sum(nil)),
		ContentType: contentType,
		UpdatedAt:   time.Now().UTC(),
	}
	if err := os.Rename(tmp.Name(), s.dataPath(kind)); err != nil {
		return model.Entry{}, err
	}
	if err := writeAtomic(s.metaPath(kind), mustJSON(e), 0o600); err != nil {
		return model.Entry{}, err
	}
	if err := syncDir(s.dir); err != nil {
		return model.Entry{}, err
	}
	return e, nil
}

func (s *Store) entry(kind string) (model.Entry, bool) {
	b, err := os.ReadFile(s.metaPath(kind))
	if err != nil {
		return model.Entry{}, false
	}
	var e model.Entry
	if err := json.Unmarshal(b, &e); err != nil || e.Size < 0 || e.UpdatedAt.IsZero() {
		return model.Entry{}, false
	}
	if kind == model.KindClipboard && e.ContentType == "" {
		e.ContentType = model.ContentTypeText
	}
	return e, true
}

func (s *Store) dataPath(kind string) string { return filepath.Join(s.dir, kind) }
func (s *Store) metaPath(kind string) string { return filepath.Join(s.dir, kind+".json") }

// repair makes the metadata agree with the data file at startup.
func (s *Store) repair(kind string) error {
	fi, err := os.Stat(s.dataPath(kind))
	if errors.Is(err, os.ErrNotExist) {
		os.Remove(s.metaPath(kind))
		return nil
	}
	if err != nil {
		return err
	}
	if e, ok := s.entry(kind); ok && e.Size == fi.Size() {
		return nil
	}
	e := model.Entry{Size: fi.Size(), UpdatedAt: fi.ModTime().UTC()}
	if kind == model.KindClipboard {
		e.SHA256, _ = hashFile(s.dataPath(kind))
		e.ContentType = model.ContentTypeText
	}
	if kind == model.KindFile {
		e.Name = "file"
	}
	return writeAtomic(s.metaPath(kind), mustJSON(e), 0o600)
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
