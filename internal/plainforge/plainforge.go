// Package plainforge connects to the hub repository in Plainforge over Git
// smart HTTP when configured, and provides an in-memory stand-in for tests
// and stand-alone runs.
//
// By default Chartroom talks only to Muster and leaves catalog syncing to
// Muster. Passing -plainforge-url (or PLAINFORGE_URL) gives the worker a
// smart HTTP Hub so it can check whole-catalog invariants against main and
// push catalog/tools.json to a catalog/<slug> branch before completing a
// lease. The owner merges that branch; Chartroom never writes main.
package plainforge

import (
	"context"
	"errors"
	"sync"
)

const (
	// DefaultRepo is the bare hub repository name Plainforge serves.
	DefaultRepo = "clearedstack.com.git"
	// MainRef is the branch Chartroom reads from. It never writes it.
	MainRef = "refs/heads/main"
	// BranchPrefix starts the name of every branch Chartroom pushes; the
	// slug follows.
	BranchPrefix = "refs/heads/catalog/"
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

// Hub is the hub repository: read a file from main, or commit a new version
// of it on the branch catalog/<slug>.
type Hub interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, slug, path string, data []byte, message string) error
}

// Commit is one write to the stand-in.
type Commit struct {
	Slug    string
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

// WriteFile stores data at path and records the commit with its slug. The
// stand-in keeps one tree and has no branches, so a later ReadFile sees the
// write (the real hub would show it on main only after the owner's merge).
// It refuses a slug the real hub would refuse.
func (m *Memory) WriteFile(_ context.Context, slug, path string, data []byte, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ValidSlug(slug) {
		return ErrRefused
	}
	if m.down {
		return ErrUnavailable
	}
	cp := append([]byte{}, data...)
	m.files[path] = cp
	m.commits = append(m.commits, Commit{Slug: slug, Path: path, Message: message, Data: cp})
	return nil
}

// Commits returns every commit made so far, oldest first.
func (m *Memory) Commits() []Commit {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Commit{}, m.commits...)
}

// ValidSlug reports whether s matches ^[a-z0-9][a-z0-9-]{0,62}$: the only
// slugs Chartroom turns into a branch name.
func ValidSlug(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0 {
			continue
		}
		return false
	}
	return true
}
