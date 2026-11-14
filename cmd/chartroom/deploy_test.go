package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	pinnedWorkloadImageRE = regexp.MustCompile(`(?m)^\s*image:\s+lazarette\.clearedstack\.svc\.cluster\.local:5000/chartroom@sha256:[0-9a-f]{64}$`)
	pinnedBaseImageRE     = regexp.MustCompile(`(?m)^defaultBaseImage:\s+gcr\.io/distroless/static:nonroot@sha256:[0-9a-f]{64}$`)
)

// TestManifestKeepsThePodPosture pins the lines of deploy/chartroom.yaml and
// deploy/networkpolicy.yaml that carry the runtime, scheduling, digest, and
// security rules.
func TestManifestKeepsThePodPosture(t *testing.T) {
	workloadBytes, err := os.ReadFile("../../deploy/chartroom.yaml")
	if err != nil {
		t.Fatal(err)
	}
	netpolBytes, err := os.ReadFile("../../deploy/networkpolicy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	workload := string(workloadBytes)
	netpol := string(netpolBytes)

	if !pinnedWorkloadImageRE.MatchString(workload) {
		t.Error("deploy/chartroom.yaml must pin image to lazarette.clearedstack.svc.cluster.local:5000/chartroom@sha256:<64 hex>")
	}
	for _, line := range []string{
		"namespace: cs-workers",
		"serviceAccountName: chartroom",
		"automountServiceAccountToken: true",
		"key: kubernetes.io/arch",
		"operator: In",
		`values: ["arm64"]`,
		"key: kubernetes.io/hostname",
		"operator: NotIn",
		`values: ["cm3588-plus"]`,
		"runAsNonRoot: true",
		"runAsUser: 65532",
		"runAsGroup: 65532",
		"fsGroup: 65532",
		"type: RuntimeDefault",
		"imagePullPolicy: IfNotPresent",
		"name: CONTRACT_VERSION",
		"value: v1",
		"name: GOMEMLIMIT",
		"value: 24MiB",
		"name: MUSTER_URL",
		"value: http://muster.clearedstack.svc.cluster.local:8080",
		"cpu: 10m",
		"memory: 30Mi",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"privileged: false",
		`drop: ["ALL"]`,
		"mountPath: /var/spool/cs",
		"sizeLimit: 16Mi",
	} {
		if !strings.Contains(workload, line) {
			t.Errorf("deploy/chartroom.yaml is missing %q", line)
		}
	}
	if n := strings.Count(workload, "memory: 30Mi"); n != 2 {
		t.Errorf("memory request and limit should both be 30Mi; found %d", n)
	}

	for _, line := range []string{
		"kind: NetworkPolicy",
		"name: default-deny",
		"namespace: cs-workers",
		"podSelector: {}",
		"- Ingress",
		"- Egress",
		"name: chartroom",
		"ingress: []",
		"kubernetes.io/metadata.name: clearedstack",
		"app.kubernetes.io/name: muster",
		"port: 8080",
		"app.kubernetes.io/name: forgejo",
		"app.kubernetes.io/instance: plainforge",
		"port: 3000",
		"kubernetes.io/metadata.name: kube-system",
		"port: 53",
	} {
		if !strings.Contains(netpol, line) {
			t.Errorf("deploy/networkpolicy.yaml is missing %q", line)
		}
	}
	if strings.Contains(netpol, "namespaceSelector: {}") {
		t.Error("deploy/networkpolicy.yaml must not allow an open namespaceSelector")
	}

	combined := strings.ToLower(workload + "\n" + netpol)
	for _, banned := range []string{
		"hostnetwork", "hostpath", "hostpid", "hostipc", "add:",
		"logbook", "ghcr.io", "docker.io", "quay.io", "ko://", ":latest",
		"secretkeyref", "kind: secret",
	} {
		if strings.Contains(combined, banned) {
			t.Errorf("deploy/ contains %q", banned)
		}
	}
	// Lazarette may appear only in the container image reference and comments,
	// never as a network target or environment setting.
	if strings.Contains(strings.ToLower(netpol), "lazarette") {
		t.Error("deploy/networkpolicy.yaml must not open access to Lazarette")
	}
}

// TestBuiltWithKoAndNoDockerfile checks the repository builds its image
// with ko from a digest-pinned base image and holds no Dockerfile.
func TestBuiltWithKoAndNoDockerfile(t *testing.T) {
	koBytes, err := os.ReadFile("../../.ko.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ko := string(koBytes)
	if !pinnedBaseImageRE.MatchString(ko) {
		t.Error(".ko.yaml must pin defaultBaseImage by sha256 digest")
	}
	if !strings.Contains(ko, "linux/arm64") {
		t.Error(".ko.yaml must target linux/arm64")
	}
	if !strings.Contains(ko, "main: ./cmd/chartroom") || !strings.Contains(ko, "CGO_ENABLED=0") {
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

// TestNoEmDashes checks that no file in the repository carries an em dash.
func TestNoEmDashes(t *testing.T) {
	emDash := []byte("\xe2\x80\x94")
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip this test file's own escaped literal check by matching raw UTF-8 bytes
		// and constructed escape sequences.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, emDash) ||
			bytes.Contains(data, []byte(`\u`+`2014`)) ||
			bytes.Contains(data, []byte(`&`+`mdash;`)) {
			t.Errorf("%s contains an em dash", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
