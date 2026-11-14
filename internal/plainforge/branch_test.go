package plainforge_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/plainforge"
)

const (
	zeroOID   = "0000000000000000000000000000000000000000"
	testUser  = "chartroom-bot"
	testToken = "fake-token-0123456789abcdef"
)

// refUpdate is one command of a git-receive-pack request.
type refUpdate struct{ old, new, ref string }

// recorder is a smart HTTP stand-in: real git-http-backend behind a wrapper
// that records every request's Authorization header and every ref command.
type recorder struct {
	mu      sync.Mutex
	auths   []string
	pushes  []refUpdate
	bareDir string
	srv     *httptest.Server
}

func (r *recorder) requests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.auths)
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auths = append(r.auths, req.Header.Get("Authorization"))
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/git-receive-pack") {
		return
	}
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	for len(body) >= 4 {
		n, err := strconv.ParseUint(string(body[:4]), 16, 16)
		if err != nil || n < 4 {
			return
		}
		if n == 0 { // flush: the pack follows
			return
		}
		line := string(body[4:n])
		body = body[n:]
		line, _, _ = strings.Cut(line, "\x00")
		f := strings.Fields(line)
		if len(f) == 3 {
			r.pushes = append(r.pushes, refUpdate{f[0], f[1], f[2]})
		}
	}
}

func newRecorder(t *testing.T) (*recorder, string) {
	t.Helper()
	backend := findGitBackend(t)
	rootDir := t.TempDir()
	rec := &recorder{bareDir: filepath.Join(rootDir, plainforge.DefaultRepo)}
	runGit(t, rootDir, "init", "--bare", "--initial-branch=main", rec.bareDir)
	runGit(t, rec.bareDir, "config", "http.receivepack", "true")
	cgiH := &cgi.Handler{
		Path:   backend,
		Env:    []string{"GIT_PROJECT_ROOT=" + rootDir, "GIT_HTTP_EXPORT_ALL=1"},
		Stderr: io.Discard,
	}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		cgiH.ServeHTTP(w, r)
	}))
	t.Cleanup(rec.srv.Close)
	return rec, rootDir
}

// seedMain puts a commit with catalog/tools.json and a sibling file on main.
func seedMain(t *testing.T, rec *recorder, rootDir, catalog string) {
	t.Helper()
	work := filepath.Join(rootDir, "work")
	runGit(t, rootDir, "clone", rec.bareDir, work)
	for rel, c := range map[string]string{"catalog/tools.json": catalog, "app.yaml": "name: site\n"} {
		full := filepath.Join(work, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "push", "origin", "main")
	rec.mu.Lock()
	rec.auths, rec.pushes = nil, nil // forget the setup traffic
	rec.mu.Unlock()
}

func TestWriteFilePushesNewBranchFromMainWithZeroOldOID(t *testing.T) {
	rec, root := newRecorder(t)
	seedMain(t, rec, root, "{\"seed\": true}\n")
	mainBefore := runGit(t, rec.bareDir, "rev-parse", "refs/heads/main")

	hub, _ := plainforge.NewHTTP(rec.srv.URL, 5*time.Second)
	data := []byte("{\"entries\": [\"alpha\"]}\n")
	if err := hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", data, "catalog: live alpha"); err != nil {
		t.Fatal(err)
	}
	if len(rec.pushes) != 1 || rec.pushes[0].ref != "refs/heads/catalog/alpha" || rec.pushes[0].old != zeroOID {
		t.Fatalf("pushes = %+v, want one create of refs/heads/catalog/alpha", rec.pushes)
	}
	if got := runGit(t, rec.bareDir, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Fatalf("main moved from %s to %s", mainBefore, got)
	}
	if got := runGit(t, rec.bareDir, "rev-parse", "refs/heads/catalog/alpha^"); got != mainBefore {
		t.Fatalf("branch parent = %s, want main's tip %s", got, mainBefore)
	}
	if got := runGit(t, rec.bareDir, "show", "refs/heads/catalog/alpha:catalog/tools.json"); got+"\n" != string(data) {
		t.Fatalf("branch file = %q", got)
	}
	if got := runGit(t, rec.bareDir, "show", "refs/heads/catalog/alpha:app.yaml"); got != "name: site" {
		t.Fatalf("sibling file lost: %q", got)
	}
	runGit(t, rec.bareDir, "fsck", "--full", "--strict")
}

func TestWriteFileOnExistingBranchFastForwardsFromItsTip(t *testing.T) {
	rec, root := newRecorder(t)
	seedMain(t, rec, root, "{\"seed\": true}\n")
	mainBefore := runGit(t, rec.bareDir, "rev-parse", "refs/heads/main")

	for i, hub := range []*plainforge.HTTP{mustHub(t, rec), mustHub(t, rec)} { // a warm and then a cold client
		if err := hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", []byte(fmt.Sprintf("{\"v\": %d}\n", i)), "catalog: live alpha"); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.pushes) != 2 {
		t.Fatalf("pushes = %+v, want two", rec.pushes)
	}
	first, second := rec.pushes[0], rec.pushes[1]
	if second.ref != "refs/heads/catalog/alpha" || second.old != first.new || second.old == zeroOID {
		t.Fatalf("second push %+v, want old = first push's new %s", second, first.new)
	}
	runGit(t, rec.bareDir, "merge-base", "--is-ancestor", first.new, second.new)
	if got := runGit(t, rec.bareDir, "rev-parse", "refs/heads/main"); got != mainBefore {
		t.Fatalf("main moved to %s", got)
	}
	if n := runGit(t, rec.bareDir, "rev-list", "--count", "refs/heads/catalog/alpha"); n != "3" {
		t.Fatalf("branch has %s commits, want seed + 2", n)
	}
}

func TestWriteFileOnEmptyRepositoryNeverCreatesMain(t *testing.T) {
	rec, _ := newRecorder(t)
	hub := mustHub(t, rec)
	if err := hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", []byte("{}\n"), "catalog: live alpha"); err != nil {
		t.Fatal(err)
	}
	if len(rec.pushes) != 1 || rec.pushes[0].ref != "refs/heads/catalog/alpha" || rec.pushes[0].old != zeroOID {
		t.Fatalf("pushes = %+v", rec.pushes)
	}
	if out := runGit(t, rec.bareDir, "for-each-ref", "--format=%(refname)"); out != "refs/heads/catalog/alpha" {
		t.Fatalf("refs = %q, want only the catalog branch", out)
	}
}

func TestReadFileReadsMainNotTheBranch(t *testing.T) {
	rec, root := newRecorder(t)
	seedMain(t, rec, root, "main-version\n")
	hub := mustHub(t, rec)
	if err := hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", []byte("branch-version\n"), "m"); err != nil {
		t.Fatal(err)
	}
	got, err := hub.ReadFile(context.Background(), "catalog/tools.json")
	if err != nil || string(got) != "main-version\n" {
		t.Fatalf("ReadFile = %q, %v; want main's content", got, err)
	}
}

func TestBadSlugFailsClosedWithNoRequest(t *testing.T) {
	rec, _ := newRecorder(t)
	hub := mustHub(t, rec)
	for _, slug := range []string{"", "Alpha", "-alpha", "a b", "a/b", "a..b", "main/../x", "a\x00b", "alpha.lock", "é", strings.Repeat("a", catalog.MaxSlugBytes+1), "../main", "a-", "a--b"} {
		err := hub.WriteFile(context.Background(), slug, "catalog/tools.json", []byte("{}\n"), "m")
		if !errors.Is(err, plainforge.ErrRefused) {
			t.Errorf("slug %q: err = %v, want ErrRefused", slug, err)
		}
	}
	if n := rec.requests(); n != 0 {
		t.Fatalf("%d requests were sent for bad slugs, want none", n)
	}
	// The longest and shortest good slugs work.
	for _, slug := range []string{"a", "0", strings.Repeat("a", catalog.MaxSlugBytes), "a-b-0"} {
		if err := hub.WriteFile(context.Background(), slug, "catalog/tools.json", []byte("{}\n"), "m"); err != nil {
			t.Errorf("slug %q: %v", slug, err)
		}
	}
	for _, p := range rec.pushes {
		if p.ref == plainforge.MainRef || !strings.HasPrefix(p.ref, "refs/heads/catalog/") {
			t.Errorf("push named %q", p.ref)
		}
	}
}

func TestBasicAuthOnEveryRequestWhenConfiguredAndAbsentWhenNot(t *testing.T) {
	rec, root := newRecorder(t)
	seedMain(t, rec, root, "{}\n")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testToken))

	hub := mustHub(t, rec)
	hub.SetBasicAuth(testUser, testToken)
	if err := hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", []byte("{\"a\":1}\n"), "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.ReadFile(context.Background(), "catalog/tools.json"); err != nil {
		t.Fatal(err)
	}
	if len(rec.auths) < 4 { // info/refs for receive and upload, upload-pack POST, receive-pack POST, read
		t.Fatalf("only %d requests seen", len(rec.auths))
	}
	for i, a := range rec.auths {
		if a != want {
			t.Fatalf("request %d Authorization = %q, want Basic auth", i, a)
		}
	}

	rec.mu.Lock()
	rec.auths = nil
	rec.mu.Unlock()
	plain := mustHub(t, rec)
	if err := plain.WriteFile(context.Background(), "beta", "catalog/tools.json", []byte("{\"b\":1}\n"), "m"); err != nil {
		t.Fatal(err)
	}
	if len(rec.auths) == 0 {
		t.Fatal("no requests seen")
	}
	for i, a := range rec.auths {
		if a != "" {
			t.Fatalf("request %d sent Authorization %q without credentials", i, a)
		}
	}
}

func TestUnauthorizedAndForbiddenAreRefusedWithAFixedMessage(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server says "+testToken+" "+r.URL.Path, status)
		}))
		hub, _ := plainforge.NewHTTP(srv.URL, time.Second)
		hub.SetBasicAuth(testUser, testToken)
		for name, err := range map[string]error{
			"write": hub.WriteFile(context.Background(), "alpha", "catalog/tools.json", []byte("{}\n"), "m"),
			"read":  func() error { _, e := hub.ReadFile(context.Background(), "catalog/tools.json"); return e }(),
		} {
			if !errors.Is(err, plainforge.ErrRefused) || err.Error() != plainforge.ErrRefused.Error() {
				t.Errorf("%d %s: err = %v, want the fixed ErrRefused", status, name, err)
			}
			for _, leak := range []string{testToken, testUser, "server says", "127.0.0.1"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("%d %s: error leaks %q", status, name, leak)
				}
			}
		}
		srv.Close()
	}
}

func TestSlugRuleIsTheCatalogRule(t *testing.T) {
	max := strings.Repeat("a", catalog.MaxSlugBytes)
	over := max + "a"
	for _, s := range []string{"a", "a-", "-a", "ab-c", "a--b", "A", "a_b", "", max, over, "a\n", "é"} {
		if got := plainforge.ValidSlug(s); got != catalog.ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = %v, catalog says %v", s, got, catalog.ValidSlug(s))
		}
	}
	if !plainforge.ValidSlug(max) || !catalog.ValidSlug(max) {
		t.Error("a slug of the catalog maximum must pass both checks")
	}
	if plainforge.ValidSlug(over) || catalog.ValidSlug(over) {
		t.Error("a slug one byte over the maximum must fail both checks")
	}
}

// FuzzValidSlug checks the plainforge and catalog slug checks agree, and that
// a stand-in write is refused exactly when the catalog refuses the slug.
func FuzzValidSlug(f *testing.F) {
	for _, s := range []string{"", "a", "a-b", "-a", "a-", "a--b", "A", "a\n", strings.Repeat("a", 64), strings.Repeat("a", 65), "a\x00", "é", "refs/heads/main"} {
		f.Add(s)
	}
	mem := plainforge.NewMemory()
	f.Fuzz(func(t *testing.T, s string) {
		want := catalog.ValidSlug(s)
		if got := plainforge.ValidSlug(s); got != want {
			t.Fatalf("ValidSlug(%q) = %v, catalog says %v", s, got, want)
		}
		err := mem.WriteFile(context.Background(), s, "catalog/tools.json", []byte("{}\n"), "m")
		if (err == nil) != want {
			t.Fatalf("Memory.WriteFile(%q) = %v, slug valid = %v", s, err, want)
		}
	})
}

func mustHub(t *testing.T, rec *recorder) *plainforge.HTTP {
	t.Helper()
	hub, err := plainforge.NewHTTP(rec.srv.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return hub
}
