GO_IMAGE       ?= golang:1.23
LINT_IMAGE     ?= golangci/golangci-lint:v1.61.0
DOCKER_RUN     = docker run --rm -v "$(CURDIR)":/w -w /w
GO_RUN         = $(DOCKER_RUN) $(GO_IMAGE)
LINT_RUN       = $(DOCKER_RUN) $(LINT_IMAGE)
CHOWN          = $(GO_RUN) chown -R $(shell id -u):$(shell id -g) /w
# The bind mount is root-owned inside the container; mark it safe so `go build`
# can read VCS info for stamping (no effect in CI, which checks out fresh).
GIT_SAFE       = git config --global --add safe.directory /w

.PHONY: build vet test lint tidy chown

build:
	$(GO_RUN) sh -c '$(GIT_SAFE) && go build ./...'
	$(CHOWN)

vet:
	$(GO_RUN) go vet ./...

test:
	$(GO_RUN) go test ./...

lint:
	$(LINT_RUN) golangci-lint run

tidy:
	$(GO_RUN) go mod tidy
	$(CHOWN)

chown:
	$(CHOWN)
