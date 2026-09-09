package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileStorage stores opaque objects below one private directory. Keys are
// deliberately restricted to a single path segment; callers must never use a
// user-supplied filename as a key.
type FileStorage struct {
	root string
}

// Ping verifies that the mandatory filesystem dependency remains available.
func (s *FileStorage) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(s.root)
	if err != nil || !info.IsDir() {
		return errors.New("blob storage unavailable")
	}
	return nil
}

// Close exists to give all storage drivers a uniform lifecycle.
func (s *FileStorage) Close() error { return nil }

func NewFileStorage(root string) (*FileStorage, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("configure filesystem blob storage: %w", ErrInvalidKey)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve filesystem blob root: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create filesystem blob root: %w", err)
	}
	return &FileStorage{root: absRoot}, nil
}

func (s *FileStorage) Put(ctx context.Context, key string, source io.Reader) (size int64, err error) {
	if source == nil || !validKey(key) {
		return 0, ErrInvalidKey
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	temporary, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("create temporary blob: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()

	size, err = io.Copy(temporary, contextReader{ctx: ctx, reader: source})
	if err != nil {
		return 0, fmt.Errorf("write blob: %w", err)
	}
	if err = temporary.Sync(); err != nil {
		return 0, fmt.Errorf("sync blob: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return 0, fmt.Errorf("close blob: %w", err)
	}

	destination := filepath.Join(s.root, key)
	// A hard link publishes the completed temporary file without the
	// check-then-rename race that could replace an existing object on Unix.
	if err = os.Link(temporaryName, destination); err != nil {
		return 0, fmt.Errorf("commit blob: %w", err)
	}
	return size, nil
}

func (s *FileStorage) Open(ctx context.Context, key string) (Object, error) {
	if !validKey(key) {
		return Object{}, ErrInvalidKey
	}
	if err := ctx.Err(); err != nil {
		return Object{}, err
	}
	file, err := os.Open(filepath.Join(s.root, key))
	if errors.Is(err, os.ErrNotExist) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, fmt.Errorf("open blob: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return Object{}, fmt.Errorf("stat blob: %w", err)
	}
	return Object{Body: &contextReadCloser{ctx: ctx, ReadCloser: file}, Size: info.Size()}, nil
}

func (s *FileStorage) Delete(ctx context.Context, key string) error {
	if !validKey(key) {
		return ErrInvalidKey
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.root, key))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete blob: %w", err)
	}
	return nil
}

func validKey(key string) bool {
	if key == "" || filepath.Base(key) != key {
		return false
	}
	for _, char := range key {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

type contextReadCloser struct {
	ctx context.Context
	io.ReadCloser
}

func (r *contextReadCloser) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReadCloser.Read(buffer)
}

var _ Storage = (*FileStorage)(nil)
