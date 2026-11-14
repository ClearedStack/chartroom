# Where the image is pushed: the in-cluster registry, never GitHub.
KO_DOCKER_REPO ?= lazarette.clearedstack.svc.cluster.local:5000

.PHONY: test check golden image manifests

test:
	go test -race ./...

check:
	gofmt -l . | (! grep .)
	go vet ./...
	go test -race ./...

# Rewrite the expected outputs under testdata from what the code does now.
# Read the diff before committing it: a changed file is a changed behaviour.
golden:
	UPDATE_GOLDEN=1 go test ./...
	go test -race ./...

# Build and push the image as $(KO_DOCKER_REPO)/chartroom, printing its
# @sha256:... reference for deploy/chartroom.yaml.
image:
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build --base-import-paths ./cmd/chartroom

# Print deploy/ manifests with a freshly built digest substituted into image:.
manifests:
	@ref=$$(KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build --base-import-paths ./cmd/chartroom); \
	sed "s|lazarette.clearedstack.svc.cluster.local:5000/chartroom@sha256:[0-9a-f]\{64\}|$$ref|" deploy/chartroom.yaml; \
	printf -- '---\n'; \
	cat deploy/networkpolicy.yaml
