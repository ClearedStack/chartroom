// Package plainforge is the small interface to the hub repository in
// Plainforge, with an in-memory stand-in.
//
// Chartroom itself never calls this package. Under SPEC.md it talks only to
// Muster, and Muster commits the catalog to the hub repository. The
// stand-in exists so the Muster stand-in has somewhere to commit, and so
// the tests can read back what was published.
package plainforge

import (
	"context"
	"errors"
	"sync"
)

// ErrNotFound means the hub repository has no file at that path.
var ErrNotFound = errors.New("file not found in the hub repository")

// Hub is the hub repository: read a file, or commit a new version of it.
type Hub interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, message string) error
}

// Commit is one write to the stand-in.
type Commit struct {
	Path    string
	Message string
	Data    []byte
}

// Memory is an in-memory hub repository.
type Memory struct {
	mu      sync.Mutex
	files   map[string][]byte
	commits []Commit
}

// NewMemory returns an empty hub repository.
func NewMemory() *Memory { return &Memory{files: make(map[string][]byte)} }

// ReadFile returns a copy of the file at path.
func (m *Memory) ReadFile(_ context.Context, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[path]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte{}, data...), nil
}

// WriteFile stores data at path and records the commit.
func (m *Memory) WriteFile(_ context.Context, path string, data []byte, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := append([]byte{}, data...)
	m.files[path] = cp
	m.commits = append(m.commits, Commit{Path: path, Message: message, Data: cp})
	return nil
}

// Commits returns every commit made so far, oldest first.
func (m *Memory) Commits() []Commit {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Commit{}, m.commits...)
}
