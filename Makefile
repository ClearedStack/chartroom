# Where the image is pushed: the in-cluster registry, never GitHub.
KO_DOCKER_REPO ?= lazarette.clearedstack.svc.cluster.local:5000

.PHONY: test check golden image manifests

test:
	go test ./...

check:
	gofmt -l . | (! grep .)
	go vet ./...
	go test ./...

# Rewrite the expected outputs under testdata from what the code does now.
# Read the diff before committing it: a changed file is a changed behaviour.
golden:
	UPDATE_GOLDEN=1 go test ./...
	go test ./...

# Build and push the image as $(KO_DOCKER_REPO)/chartroom.
image:
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko build --base-import-paths ./cmd/chartroom

# Print the manifests with the image reference filled in.
manifests:
	KO_DOCKER_REPO=$(KO_DOCKER_REPO) ko resolve --base-import-paths -f deploy/
