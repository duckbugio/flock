FROM golang:1.26.4

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates git curl && \
    rm -rf /var/lib/apt/lists/*

ENV GOLANGCI_LINT_VERSION=2.11.4 \
    GOBIN=/usr/local/bin

RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v${GOLANGCI_LINT_VERSION}
