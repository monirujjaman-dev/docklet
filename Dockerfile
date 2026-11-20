# syntax=docker/dockerfile:1

# --- Dashboard ---------------------------------------------------------------
# Build stages run on the builder's native platform and cross-compile, so
# multi-arch images build fast without emulation.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# vite.config.ts writes to ../internal/server/dist
RUN npm run build

# --- Binary ------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/server/dist ./internal/server/dist
ARG VERSION=dev
ARG COMMIT=none
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w \
      -X github.com/monirujjaman-dev/docklet/internal/version.Version=${VERSION} \
      -X github.com/monirujjaman-dev/docklet/internal/version.Commit=${COMMIT} \
      -X github.com/monirujjaman-dev/docklet/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -o /out/docklet ./cmd/docklet

# --- Runtime -----------------------------------------------------------------
FROM alpine:3.21
LABEL org.opencontainers.image.source="https://github.com/monirujjaman-dev/docklet" \
      org.opencontainers.image.description="Docklet: lightweight self-hosted PaaS on Docker and Caddy" \
      org.opencontainers.image.licenses="NOASSERTION"
# git, ssh and the docker CLI (with buildx) are used for Git builds.
RUN apk add --no-cache ca-certificates tzdata git openssh-client docker-cli docker-cli-buildx
COPY --from=build /out/docklet /usr/local/bin/docklet
ENV DOCKLET_DATA_DIR=/var/lib/docklet
VOLUME /var/lib/docklet
EXPOSE 3000
# Runs as root: it needs the Docker socket, which is root-equivalent anyway.
ENTRYPOINT ["docklet"]
CMD ["serve"]
