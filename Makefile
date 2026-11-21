BINARY   := docklet
PKG      := github.com/monirujjaman-dev/docklet
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)
GOFLAGS  := -trimpath
DIST     := internal/server/dist

export CGO_ENABLED := 0

.PHONY: all build build-go web web-deps dev dev-go dev-web test lint fmt sqlc clean release

all: build

## build: build dashboard + single static binary into ./bin
build: web build-go

build-go:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/docklet

web-deps:
	cd web && npm ci

web: web-deps
	cd web && npm run build
	@touch $(DIST)/.gitkeep

## dev: run Go server (:3000) and Vite dev server (:5173) together
dev:
	@$(MAKE) -j2 dev-go dev-web

dev-go:
	go run ./cmd/docklet serve -addr 127.0.0.1:3000 -data-dir ./data -log-level debug

dev-web:
	cd web && npm run dev

## test: Go tests + frontend type-check
test:
	go test ./...
	cd web && npm run check

## lint: go vet + golangci-lint (if installed)
lint:
	go vet ./...
	@command -v golangci-lint >/dev/null && golangci-lint run || echo "golangci-lint not installed, skipped"

fmt:
	gofmt -s -w .

## sqlc: regenerate typed queries from migrations/ + internal/store/queries/
sqlc:
	sqlc generate

## release: cross-compile linux amd64 + arm64 into ./bin
release: web
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-amd64 ./cmd/docklet
	GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/$(BINARY)-linux-arm64 ./cmd/docklet
	cd bin && sha256sum $(BINARY)-linux-amd64 $(BINARY)-linux-arm64 > checksums.txt

clean:
	rm -rf bin
	find $(DIST) -mindepth 1 ! -name .gitkeep -delete
