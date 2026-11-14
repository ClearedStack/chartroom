package plainforge_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/chartroom/internal/plainforge"
)

func findGitBackend(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not installed")
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	bin := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("git-http-backend not at %s", bin)
	}
	return bin
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@clearedstack.internal",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@clearedstack.internal",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func startGitHTTPServer(t *testing.T) (rootDir string, bareDir string, srv *httptest.Server) {
	t.Helper()
	backend := findGitBackend(t)
	rootDir = t.TempDir()
	bareDir = filepath.Join(rootDir, plainforge.DefaultRepo)
	runGit(t, rootDir, "init", "--bare", "--initial-branch=main", bareDir)
	runGit(t, bareDir, "config", "http.receivepack", "true")

	handler := &cgi.Handler{
		Path: backend,
		Env: []string{
			"GIT_PROJECT_ROOT=" + rootDir,
			"GIT_HTTP_EXPORT_ALL=1",
		},
		Stderr: io.Discard,
	}
	srv = httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return rootDir, bareDir, srv
}

func TestHTTPSmartGitEmptyInitIdempotentAndExternalCommits(t *testing.T) {
	rootDir, bareDir, srv := startGitHTTPServer(t)
	ctx := context.Background()

	// Root URL resolves automatically to /clearedstack.com.git.
	hub, err := plainforge.NewHTTP(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Empty repository has no refs/heads/main yet: ReadFile returns ErrNotFound.
	if _, err := hub.ReadFile(ctx, "catalog/tools.json"); !errors.Is(err, plainforge.ErrNotFound) {
		t.Fatalf("empty repo ReadFile = %v, want ErrNotFound", err)
	}

	// 2. First WriteFile creates a root commit on refs/heads/catalog/alpha and
	// leaves main unborn.
	v1 := []byte("{\n  \"contract\": \"v1\",\n  \"entries\": []\n}\n")
	if err := hub.WriteFile(ctx, "alpha", "catalog/tools.json", v1, "catalog: seed"); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}
	runGit(t, bareDir, "fsck", "--full", "--strict")
	if got := runGit(t, bareDir, "show", "refs/heads/catalog/alpha:catalog/tools.json"); got+"\n" != string(v1) {
		t.Fatalf("branch file = %q, want %q", got, v1)
	}
	if _, err := hub.ReadFile(ctx, "catalog/tools.json"); !errors.Is(err, plainforge.ErrNotFound) {
		t.Fatalf("ReadFile with main unborn = %v, want ErrNotFound", err)
	}
	if n := runGit(t, bareDir, "rev-list", "--count", "refs/heads/catalog/alpha"); n != "1" {
		t.Fatalf("commit count = %s, want 1", n)
	}

	// 3. Writing identical bytes is a no-op and makes no duplicate commit.
	if err := hub.WriteFile(ctx, "alpha", "catalog/tools.json", v1, "catalog: seed again"); err != nil {
		t.Fatalf("idempotent WriteFile: %v", err)
	}
	if n := runGit(t, bareDir, "rev-list", "--count", "refs/heads/catalog/alpha"); n != "1" {
		t.Fatalf("commit count after no-op = %s, want 1", n)
	}

	// 4. Simulate an external push (for example Keelworks updating app.yaml,
	// executable scripts, and sibling files whose names bracket "catalog/"
	// in Git's tree sort order).
	workDir := filepath.Join(rootDir, "work")
	runGit(t, rootDir, "clone", bareDir, workDir)
	runGit(t, workDir, "checkout", "--orphan", "main")
	for rel, content := range map[string]string{
		"app.yaml":               "name: clearedstack-site\n",
		"catalog/tools.json":     "main-catalog\n",
		"catalog-note.txt":       "sorts before catalog/ because '-' < '/'\n",
		"catalog0.txt":           "sorts after catalog/ because '/' < '0'\n",
		"src/app/tools/page.tsx": "export default function Tools() { return null }\n",
	} {
		full := filepath.Join(workDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scriptPath := filepath.Join(workDir, "scripts", "verify.sh")
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, workDir, "add", ".")
	runGit(t, workDir, "commit", "-m", "chore: pin app.yaml [skip-ci]")
	runGit(t, workDir, "push", "origin", "main")

	// 5. The warm HTTP client builds a new branch from main's tip, splices only
	// catalog/tools.json onto it, and preserves every sibling file,
	// directory sort order, and 100755 executable mode.
	v2 := []byte("{\n  \"contract\": \"v1\",\n  \"entries\": [\n    {\"slug\": \"alpha\"}\n  ]\n}\n")
	if err := hub.WriteFile(ctx, "beta", "catalog/tools.json", v2, "catalog: live beta"); err != nil {
		t.Fatalf("WriteFile after external push: %v", err)
	}
	runGit(t, bareDir, "fsck", "--full", "--strict")

	// 6. Repack with delta compression and verify a cold-start client unpacks
	// deltified packs cleanly.
	for i := 0; i < 4; i++ {
		next := []byte(fmt.Sprintf("{\n  \"contract\": \"v1\",\n  \"entries\": [{\"slug\": \"alpha-%d\", \"pad\": %q}]\n}\n", i, strings.Repeat("same-prefix-", 80)))
		if err := hub.WriteFile(ctx, "beta", "catalog/tools.json", next, fmt.Sprintf("catalog: live alpha-%d", i)); err != nil {
			t.Fatalf("WriteFile %d: %v", i, err)
		}
	}
	runGit(t, bareDir, "repack", "-a", "-d", "--depth=10", "--window=10")
	runGit(t, bareDir, "fsck", "--full", "--strict")

	cold, err := plainforge.NewHTTP(srv.URL+"/clearedstack.com", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := cold.ReadFile(ctx, "catalog/tools.json")
	if err != nil {
		t.Fatalf("cold ReadFile after delta repack: %v", err)
	}
	if string(latest) != "main-catalog\n" {
		t.Fatalf("cold ReadFile = %q, want main's file", latest)
	}
	if got := runGit(t, bareDir, "show", "refs/heads/catalog/beta:catalog/tools.json"); !strings.Contains(got, `"alpha-3"`) {
		t.Fatalf("unexpected branch content: %s", got)
	}

	// Verify sibling files and 100755 mode are intact in refs/heads/catalog/beta.
	treeListing := runGit(t, bareDir, "ls-tree", "-r", "refs/heads/catalog/beta")
	for _, want := range []string{
		"100644 blob ",
		"\tapp.yaml",
		"\tcatalog-note.txt",
		"\tcatalog/tools.json",
		"\tcatalog0.txt",
		"\tsrc/app/tools/page.tsx",
		"100755 blob ",
		"\tscripts/verify.sh",
	} {
		if !strings.Contains(treeListing, want) {
			t.Errorf("ls-tree missing %q:\n%s", want, treeListing)
		}
	}
}

func TestPlainforgeCapabilityAndRefusalRules(t *testing.T) {
	for _, bad := range []string{
		"",
		"not a url",
		"ftp://plainforge.example/clearedstack.com.git",
		"http://user:pass@plainforge.example/clearedstack.com.git",
		"http://plainforge.example/clearedstack.com.git?foo=1",
		"http://plainforge.example/../secret.git",
	} {
		if _, err := plainforge.NewHTTP(bad, time.Second); !errors.Is(err, plainforge.ErrBadAddress) {
			t.Errorf("NewHTTP(%q) = %v, want ErrBadAddress", bad, err)
		}
	}

	// Verify Chartroom never asks for capabilities outside Plainforge's
	// uploadCaps ("multi_ack_detailed") and receiveCaps ("report-status", "ofs-delta"),
	// and classifies concurrent ref updates as ErrUnavailable while keeping all
	// errors free of hostnames and server text.
	const zero = "0000000000000000000000000000000000000000"
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

	var sawPushCaps string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/clearedstack.com.git/info/refs" && r.URL.Query().Get("service") == "git-receive-pack":
			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			_, _ = io.WriteString(w, pkt("# service=git-receive-pack\n")+"0000"+
				pkt(zero+" capabilities^{}\x00report-status ofs-delta agent=plainforge/v0.1.0-draft\n")+"0000")
		case r.URL.Path == "/clearedstack.com.git/git-receive-pack":
			body, _ := io.ReadAll(r.Body)
			if idx := bytes.IndexByte(body, 0); idx >= 0 {
				end := bytes.IndexByte(body[idx+1:], '\n')
				if end >= 0 {
					sawPushCaps = string(body[idx+1 : idx+1+end])
				}
			}
			w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
			_, _ = io.WriteString(w, pkt("unpack ok\n")+
				pkt("ng refs/heads/catalog/alpha E_NEEDS_SUBTOOL: the ref changed since it was read; fetch and try again\n")+"0000")
		default:
			http.Error(w, "private-server-detail", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	hub, err := plainforge.NewHTTP(srv.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", []byte("{}\n"), "catalog: test")
	if !errors.Is(err, plainforge.ErrUnavailable) {
		t.Fatalf("concurrent ref race = %v, want ErrUnavailable", err)
	}
	if sawPushCaps != "report-status" {
		t.Fatalf("push capabilities = %q, want %q", sawPushCaps, "report-status")
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "changed since") {
		t.Fatalf("error leaked server text: %q", err.Error())
	}

	// 503 maps to ErrUnavailable and repeats nothing from the response.
	_, err = hub.ReadFile(context.Background(), "catalog/tools.json")
	if !errors.Is(err, plainforge.ErrUnavailable) {
		t.Fatalf("503 ReadFile = %v, want ErrUnavailable", err)
	}

	// Redirects are never followed.
	redir := httptest.NewServer(http.RedirectHandler("http://127.0.0.1:1/clearedstack.com.git", http.StatusFound))
	defer redir.Close()
	rhub, _ := plainforge.NewHTTP(redir.URL, time.Second)
	if _, err := rhub.ReadFile(context.Background(), "catalog/tools.json"); !errors.Is(err, plainforge.ErrRefused) {
		t.Fatalf("redirect ReadFile = %v, want ErrRefused", err)
	}
}
