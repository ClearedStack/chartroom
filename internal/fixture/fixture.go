// Package fixture reads the saved examples under testdata. It is used only
// by tests.
package fixture

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Root is the testdata directory, relative to a package two levels down
// from the repository root.
const Root = "../../testdata"

// Updating reports whether the tests were asked to rewrite the expected
// outputs instead of checking them: UPDATE_GOLDEN=1 go test ./...
func Updating() bool { return os.Getenv("UPDATE_GOLDEN") == "1" }

// Dirs returns the case directories under Root/group, in name order.
func Dirs(t testing.TB, group string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(Root, group))
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(Root, group, e.Name()))
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatalf("no fixtures under %s", group)
	}
	return out
}

// Read returns a fixture file's bytes.
func Read(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

// Exists reports whether a fixture file is there.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Golden checks got against the saved file at path, byte for byte. When
// updating it writes the file instead.
func Golden(t testing.TB, path string, got []byte) {
	t.Helper()
	if Updating() {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run with UPDATE_GOLDEN=1 to create it)", path)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not match.\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// Absent checks that no saved file exists at path. When updating it removes
// a stale one.
func Absent(t testing.TB, path string) {
	t.Helper()
	if Updating() {
		_ = os.Remove(path)
		return
	}
	if Exists(path) {
		t.Errorf("%s exists and should not", path)
	}
}

// ErrorLine splits a saved error line "CODE reason [field]: message" into
// its parts.
func ErrorLine(t testing.TB, line []byte) (code, reason, field string) {
	t.Helper()
	head, _, ok := strings.Cut(strings.TrimSpace(string(line)), ": ")
	parts := strings.Fields(head)
	if !ok || len(parts) < 2 || len(parts) > 3 {
		t.Fatalf("not an error line: %q", line)
	}
	if len(parts) == 3 {
		field = parts[2]
	}
	return parts[0], parts[1], field
}
