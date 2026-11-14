package plainforge_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/chartroom/internal/plainforge"
)

func TestLoadCredentials(t *testing.T) {
	write := func(files map[string]string) string {
		dir := t.TempDir()
		for n, c := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	dir := write(map[string]string{"user": testUser + "\n", "token": "  " + testToken + "\n"})
	user, token, set, err := plainforge.LoadCredentials(dir)
	if err != nil || !set || user != testUser || token != testToken {
		t.Fatalf("both present: %q %q %v %v", user, token, set, err)
	}

	for name, d := range map[string]string{
		"unset":   "",
		"missing": filepath.Join(t.TempDir(), "no-such-dir"),
		"empty":   write(nil),
	} {
		if _, _, set, err := plainforge.LoadCredentials(d); set || err != nil {
			t.Fatalf("%s dir: set=%v err=%v, want no credentials and no error", name, set, err)
		}
	}

	for name, d := range map[string]string{
		"only user":   write(map[string]string{"user": testUser}),
		"only token":  write(map[string]string{"token": testToken}),
		"empty user":  write(map[string]string{"user": "\n", "token": testToken}),
		"empty token": write(map[string]string{"user": testUser, "token": " "}),
		"big token":   write(map[string]string{"user": testUser, "token": strings.Repeat("t", 4097)}),
	} {
		_, _, set, err := plainforge.LoadCredentials(d)
		if err == nil || set {
			t.Fatalf("%s: want an error", name)
		}
		if strings.Contains(err.Error(), d) || strings.Contains(err.Error(), testToken) {
			t.Fatalf("%s: error leaks a path or the token: %v", name, err)
		}
	}

	// Exactly the bound is accepted.
	ok := write(map[string]string{"user": testUser, "token": strings.Repeat("t", 4096)})
	if _, _, set, err := plainforge.LoadCredentials(ok); !set || err != nil {
		t.Fatalf("4096-byte token: set=%v err=%v", set, err)
	}
}
