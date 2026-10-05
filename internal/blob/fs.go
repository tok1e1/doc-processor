// Package blob stores rendered documents.
//
// FS keeps files on a local (or shared) volume. The interface is intentionally
// small so an S3-compatible implementation can be dropped in without touching callers.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tok1e1/doc-processor/internal/domain"
)

type FS struct {
	root string
}

func NewFS(root string) (*FS, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	return &FS{root: root}, nil
}

// Put writes the object atomically: readers never observe a partially written file.
func (s *FS) Put(_ context.Context, key string, r io.Reader) (err error) {
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err = io.Copy(tmp, r); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *FS) Open(_ context.Context, key string) (io.ReadSeekCloser, error) {
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // path is validated in s.path
	if errors.Is(err, os.ErrNotExist) {
		return nil, domain.ErrNotFound
	}
	return f, err
}

func (s *FS) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if key == "" || strings.Contains(key, "..") || clean == "/" {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return filepath.Join(s.root, clean), nil
}
