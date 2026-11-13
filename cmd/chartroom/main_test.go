package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/fixture"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/plainforge"
)

func noEnv(string) string { return "" }

// chartroom runs the command in process and returns what it printed and
// its exit status.
func chartroom(env func(string) string, args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	code = run(args, env, &out, &errOut)
	return out.String(), errOut.String(), code
}

// TestApplyFixtures runs "chartroom apply -write" over every saved example
// and checks what it prints, how it exits and what it leaves in the
// catalog file.
func TestApplyFixtures(t *testing.T) {
	for _, dir := range fixture.Dirs(t, "apply") {
		name := filepath.Base(dir)
		t.Run(name, func(t *testing.T) {
			original := fixture.Read(t, filepath.Join(dir, "catalog.json"))
			work := filepath.Join(t.TempDir(), "catalog.json")
			if err := os.WriteFile(work, original, 0o640); err != nil {
				t.Fatal(err)
			}
			decisionPath := filepath.Join(dir, "decision.json")

			// Without -write the file is never touched.
			dryOut, dryErr, dryCode := chartroom(noEnv, "apply", "-catalog", work, "-decision", decisionPath)
			if got, _ := os.ReadFile(work); !bytes.Equal(got, original) {
				t.Fatal("apply without -write changed the catalog file")
			}

			stdout, stderr, code := chartroom(noEnv, "apply", "-catalog", work, "-decision", decisionPath, "-write")
			if stdout != dryOut || stderr != dryErr || code != dryCode {
				t.Errorf("-write changed what the command prints or how it exits")
			}
			after, _ := os.ReadFile(work)

			if errPath := filepath.Join(dir, "want.error.txt"); fixture.Exists(errPath) {
				want := fixture.Read(t, errPath)
				wantCode := exitParse
				if strings.HasPrefix(string(want), "E_NEEDS_SUBTOOL ") {
					wantCode = exitNeedsSubtool
				}
				if stderr != string(want) || stdout != "" || code != wantCode {
					t.Errorf("stdout %q, stderr %q, exit %d; want stderr %q, exit %d", stdout, stderr, code, want, wantCode)
				}
				if !bytes.Equal(after, original) {
					t.Error("a refused decision changed the catalog file")
				}
				return
			}
			if code != exitOK || stderr != "" {
				t.Errorf("exit %d, stderr %q", code, stderr)
			}
			fixture.Golden(t, filepath.Join(dir, "want.change.json"), []byte(stdout))
			if strings.HasSuffix(name, "-unchanged") {
				if !bytes.Equal(after, original) {
					t.Error("an unchanged decision rewrote the catalog file")
				}
				return
			}
			fixture.Golden(t, filepath.Join(dir, "want.catalog.json"), after)
			if info, _ := os.Stat(work); info.Mode().Perm() != 0o640 {
				t.Errorf("the catalog file's mode became %v", info.Mode().Perm())
			}
			if entries, _ := os.ReadDir(filepath.Dir(work)); len(entries) != 1 {
				t.Errorf("apply left %d files beside the catalog", len(entries)-1)
			}
		})
	}
}

func TestUsageAndInputErrors(t *testing.T) {
	dir := filepath.Join(fixture.Root, "apply", "ok-publish")
	catalogPath, decisionPath := filepath.Join(dir, "catalog.json"), filepath.Join(dir, "decision.json")
	missing := filepath.Join(t.TempDir(), "a-private-name.json")
	cases := [][]string{
		{},
		{"publish"},
		{"apply"},
		{"apply", "-catalog", catalogPath},
		{"apply", "-catalog", catalogPath, "-decision", decisionPath, "extra"},
		{"apply", "-catalog", catalogPath, "-decision", decisionPath, "-unknown"},
		{"apply", "-catalog", missing, "-decision", decisionPath},
		{"apply", "-catalog", catalogPath, "-decision", missing},
		{"run", "-spool", t.TempDir()},
		{"run", "-spool", t.TempDir(), "-muster-url", "not an address"},
		{"run", "-spool", t.TempDir(), "-standalone"},
		{"run", "-spool", t.TempDir(), "-standalone", "-catalog", catalogPath, "-items", t.TempDir()},
		{"run", "-spool", t.TempDir(), "extra"},
	}
	for _, args := range cases {
		stdout, stderr, code := chartroom(noEnv, args...)
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, "usage:") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
		if strings.Contains(stderr, "a-private-name") || strings.Contains(stderr, "not an address") {
			t.Errorf("%v: the message repeats its input: %q", args, stderr)
		}
	}
	if _, stderr, code := chartroom(noEnv, "run", "-spool", missing, "-muster-url", "http://muster.example"); code != exitInternal || strings.Contains(stderr, "a-private-name") {
		t.Errorf("unusable spool: exit %d, stderr %q", code, stderr)
	}
	if stdout, _, code := chartroom(noEnv, "help"); code != exitOK || !strings.Contains(stdout, "usage:") {
		t.Errorf("help: exit %d, stdout %q", code, stdout)
	}
}

func TestVersionAndContractVersion(t *testing.T) {
	stdout, _, code := chartroom(noEnv, "version")
	if code != exitOK || stdout != "chartroom v0.1.0-draft contract v1\n" {
		t.Errorf("version: exit %d, %q", code, stdout)
	}
	var component struct {
		Slug    string `json:"slug"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(fixture.Read(t, "../../component.json"), &component); err != nil {
		t.Fatal(err)
	}
	if component.Version != Version || component.Slug != muster.Worker {
		t.Errorf("component.json says %s %s; the code says %s %s", component.Slug, component.Version, muster.Worker, Version)
	}

	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "CONTRACT_VERSION" {
				return v
			}
			return ""
		}
	}
	if _, _, code := chartroom(env("v1"), "version"); code != exitOK {
		t.Errorf("CONTRACT_VERSION=v1: exit %d", code)
	}
	_, stderr, code := chartroom(env("v2"), "version")
	if code != exitNeedsSubtool || stderr != "E_NEEDS_SUBTOOL schema_version_mismatch contract_version: the schema version is not supported\n" {
		t.Errorf("CONTRACT_VERSION=v2: exit %d, stderr %q", code, stderr)
	}
}

// TestStandalone runs the whole worker with no other component and checks
// the report against the saved one: results in priority order, and the
// catalog that comes out.
func TestStandalone(t *testing.T) {
	dir := filepath.Join(fixture.Root, "standalone")
	spoolDir := t.TempDir()
	args := []string{"run", "-standalone", "-catalog", filepath.Join(dir, "catalog.json"), "-items", filepath.Join(dir, "items"), "-spool", spoolDir}
	stdout, stderr, code := chartroom(noEnv, args...)
	if code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	fixture.Golden(t, filepath.Join(dir, "want.json"), []byte(stdout))
	if entries, _ := os.ReadDir(spoolDir); len(entries) != 0 {
		t.Errorf("the spool was left with %d files", len(entries))
	}
	again, _, _ := chartroom(noEnv, args...)
	if again != stdout {
		t.Error("a second run printed something different")
	}
	var report struct {
		Items []muster.Outcome `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, it := range report.Items {
		order = append(order, it.Priority)
	}
	if got := strings.Join(order, " "); got != "urgent urgent normal normal normal backfill" {
		t.Errorf("results arrived in order %q", got)
	}
}

// --- the built binary ---

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "chartroom-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "chartroom")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the command: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// bigItem is a valid publish item close to the largest the contract allows.
func bigItem(n int) []byte {
	links := make([]string, catalog.MaxProvLinks)
	for i := range links {
		links[i] = "https://rules.example/" + strings.Repeat("a", catalog.MaxURLBytes-40) + fmt.Sprint(i)
	}
	conds := make([]map[string]any, 5)
	for i := range conds {
		conds[i] = map[string]any{"name": fmt.Sprintf("made-up-condition-%d", i), "passed": true}
	}
	slug := fmt.Sprintf("sample-tool-%04d", n)
	data, _ := json.Marshal(map[string]any{
		"contract_version": "v1", "item_id": fmt.Sprintf("itm-%04d", n), "state": "building", "action": "publish", "slug": slug,
		"entry":       map[string]any{"title": strings.Repeat("T", catalog.MaxTitleRunes), "summary": strings.Repeat("s", catalog.MaxSummaryRunes)},
		"publish":     map[string]any{"repository": "https://forge.example/tools/" + slug},
		"provenance":  map[string]any{"note": strings.Repeat("n", catalog.MaxProvNoteBytes), "links": links},
		"attestation": map[string]any{"conditions": conds},
	})
	return data
}

// TestBinaryStaysInsideItsBudget runs the real binary as the cluster does,
// against the Muster stand-in over HTTP, with over a hundred items of the
// largest size the contract allows. It checks three of the specification's
// limits: peak memory under 30 MiB, no file written outside the spool
// directory, and nothing but the three error codes in what it logs.
func TestBinaryStaysInsideItsBudget(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("peak memory is read from the Linux process accounting")
	}
	if testing.Short() {
		t.Skip("skipped with -short")
	}
	const items = 120
	hub := plainforge.NewMemory()
	m := muster.NewMemory(hub, time.Now, time.Minute)
	for i := 0; i < items; i++ {
		m.Seed(fmt.Sprintf("itm-%04d", i), []string{"normal", "urgent", "backfill"}[i%3], bigItem(i))
	}
	m.Seed("broken", "normal", []byte(`{"contract_version": "v1", "action": "feature"}`))
	srv := httptest.NewServer(muster.Handler(m, "made-up-token"))
	defer srv.Close()

	root := t.TempDir()
	spoolDir, cwd, tmp := filepath.Join(root, "spool"), filepath.Join(root, "cwd"), filepath.Join(root, "tmp")
	for _, d := range []string{spoolDir, cwd, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	tokenPath := filepath.Join(root, "token")
	if err := os.WriteFile(tokenPath, []byte("made-up-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "run", "-once", "-muster-url", srv.URL, "-token-file", tokenPath, "-spool", spoolDir)
	cmd.Dir = cwd
	cmd.Env = []string{"TMPDIR=" + tmp, "HOME=" + tmp, "CONTRACT_VERSION=v1"}
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Run(); err != nil {
		t.Fatalf("the worker failed: %v\n%s", err, logs.String())
	}

	completed := 0
	for _, o := range m.Outcomes() {
		if o.Status == muster.StatusCompleted {
			completed++
		}
	}
	if completed != items {
		t.Fatalf("%d of %d items completed\n%s", completed, items, tail(logs.String()))
	}
	final, _ := hub.ReadFile(ctx(), muster.CatalogPath)
	t.Logf("published %d entries, catalog is %d KiB", items, len(final)>>10)

	usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok {
		t.Fatal("no resource usage for the worker process")
	}
	peakKiB := int64(usage.Maxrss)
	t.Logf("peak memory %.1f MiB", float64(peakKiB)/1024)
	if peakKiB > 30*1024 {
		t.Errorf("peak memory %.1f MiB is over the 30 MiB budget", float64(peakKiB)/1024)
	}

	for _, d := range []string{spoolDir, cwd, tmp} {
		if entries, _ := os.ReadDir(d); len(entries) != 0 {
			t.Errorf("%s was left with %d files", filepath.Base(d), len(entries))
		}
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, banned := range []string{host, "127.0.0.1", "made-up-token", root} {
		if strings.Contains(logs.String(), banned) {
			t.Errorf("the log contains %q", banned)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if code, ok := entry["code"].(string); ok && code != "E_TIMEOUT" && code != "E_PARSE" && code != "E_NEEDS_SUBTOOL" {
			t.Errorf("log line carries code %q", code)
		}
	}
}

func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 10 {
		lines = lines[len(lines)-10:]
	}
	return strings.Join(lines, "\n")
}

// TestBinaryApply checks the built binary gives the saved output too, with
// the exit status a shell would see.
func TestBinaryApply(t *testing.T) {
	dir := filepath.Join(fixture.Root, "apply", "ok-retire")
	cmd := exec.Command(binary, "apply", "-catalog", filepath.Join(dir, "catalog.json"), "-decision", filepath.Join(dir, "decision.json"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	fixture.Golden(t, filepath.Join(dir, "want.change.json"), out)

	dir = filepath.Join(fixture.Root, "apply", "subtool-unsupported-entry-type")
	cmd = exec.Command(binary, "apply", "-catalog", filepath.Join(dir, "catalog.json"), "-decision", filepath.Join(dir, "decision.json"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != exitNeedsSubtool {
		t.Errorf("exit = %v, want %d", err, exitNeedsSubtool)
	}
	fixture.Golden(t, filepath.Join(dir, "want.error.txt"), stderr.Bytes())
}

func ctx() context.Context { return context.Background() }
