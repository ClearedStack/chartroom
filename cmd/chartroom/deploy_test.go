package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestManifestKeepsThePodPosture pins the lines of the manifest that carry
// the specification's runtime and security rows.
func TestManifestKeepsThePodPosture(t *testing.T) {
	data, err := os.ReadFile("../../deploy/chartroom.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	for _, line := range []string{
		"namespace: cs-workers",
		"serviceAccountName: chartroom",
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"privileged: false",
		`drop: ["ALL"]`,
		"memory: 30Mi",
		"mountPath: /var/spool/cs",
		"image: ko://github.com/ClearedStack/chartroom/cmd/chartroom",
		"kind: NetworkPolicy",
		`policyTypes: ["Ingress", "Egress"]`,
		"ingress: []",
		"app.kubernetes.io/name: muster",
		"name: CONTRACT_VERSION",
	} {
		if !strings.Contains(manifest, line) {
			t.Errorf("the manifest no longer has %q", line)
		}
	}
	lower := strings.ToLower(manifest)
	for _, banned := range []string{"hostnetwork", "hostpath", "hostpid", "add:", "plainforge", "logbook", "lazarette", "ghcr.io"} {
		if strings.Contains(lower, banned) {
			t.Errorf("the manifest contains %q", banned)
		}
	}
	if n := strings.Count(manifest, "memory: 30Mi"); n != 2 {
		t.Errorf("memory request and limit should both be 30Mi; found %d", n)
	}
}

// TestBuiltWithKoAndNoDockerfile checks the repository builds its image
// with ko and holds no Dockerfile.
func TestBuiltWithKoAndNoDockerfile(t *testing.T) {
	ko, err := os.ReadFile("../../.ko.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ko), "main: ./cmd/chartroom") || !strings.Contains(string(ko), "CGO_ENABLED=0") {
		t.Error(".ko.yaml does not build ./cmd/chartroom as a static binary")
	}
	err = filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		name := strings.ToLower(d.Name())
		if strings.HasPrefix(name, "dockerfile") || strings.HasSuffix(name, ".dockerfile") || name == ".dockerignore" {
			t.Errorf("found %s; the image is built with ko", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("../../LICENSE"); err != nil {
		t.Error("the LICENSE file is gone")
	}
}
