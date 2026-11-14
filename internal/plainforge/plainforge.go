// Package plainforge connects to the hub repository in Plainforge over Git
// smart HTTP when configured, and provides an in-memory stand-in for tests
// and stand-alone runs.
//
// By default Chartroom talks only to Muster and leaves catalog syncing to
// Muster. Passing -plainforge-url (or PLAINFORGE_URL) gives the worker a
// smart HTTP Hub so it can check whole-catalog invariants and commit
// catalog/tools.json directly to refs/heads/main before completing a lease.
package plainforge

import (
	"context"
	"errors"
	"sync"
)

const (
	// DefaultRepo is the bare hub repository name Plainforge serves.
	DefaultRepo = "clearedstack.com.git"
	// MainRef is the branch Chartroom reads from and commits to.
	MainRef = "refs/heads/main"
)

// Fixed errors returned by a Hub. None carries an address, path, token or
// text from the network.
var (
	// ErrNotFound means the hub repository has no file at that path.
	ErrNotFound = errors.New("file not found in the hub repository")
	// ErrBadAddress means the Plainforge address given at start-up cannot be used.
	ErrBadAddress = errors.New("the Plainforge address is not a usable http or https address")
	// ErrUnavailable means Plainforge did not answer, was busy, or moved main concurrently.
	ErrUnavailable = errors.New("plainforge did not acknowledge the request")
	// ErrRefused means Plainforge refused the request in a way retrying will not cure.
	ErrRefused = errors.New("plainforge refused the request")
	// ErrCorrupt means Plainforge returned a malformed Git packet, pack or object.
	ErrCorrupt = errors.New("plainforge returned an unreadable git object")
)

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
	down    bool
}

// NewMemory returns an empty hub repository.
func NewMemory() *Memory { return &Memory{files: make(map[string][]byte)} }

// SetDown makes every call return ErrUnavailable until cleared.
func (m *Memory) SetDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

// ReadFile returns a copy of the file at path.
func (m *Memory) ReadFile(_ context.Context, path string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, ErrUnavailable
	}
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
	if m.down {
		return ErrUnavailable
	}
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
