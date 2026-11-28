ARG GO_IMAGE=golang:1.27-trixie

# Installs the Claude CLI with the official installer, which checks the download's checksum.
FROM debian:trixie-slim AS claude
ARG CLAUDE_VERSION=2.1.332
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}" \
    && cp "$(readlink -f /root/.local/bin/claude)" /usr/local/bin/claude

# Dev image: source and Go toolchain, used with `go run` and compose watch.
FROM ${GO_IMAGE} AS dev
ENV DISABLE_AUTOUPDATER=1 CLAUDE_CONFIG_DIR=/claude-data
COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./cmd/... && mkdir /out/checkouts

FROM gcr.io/distroless/static-debian13:nonroot AS api
COPY --from=build /out/api /api
# The checkouts volume starts as a copy of this folder, so the nonroot user (65532) can write to it.
COPY --from=build --chown=65532:65532 /out/checkouts /checkouts
EXPOSE 8080 8081
ENTRYPOINT ["/api"]

FROM debian:trixie-slim AS worker
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 worker \
    && mkdir /claude-data && chown worker:worker /claude-data
# claude keeps its settings and session transcripts here, in a volume, so reviews can be joined later.
ENV DISABLE_AUTOUPDATER=1 CLAUDE_CONFIG_DIR=/claude-data
COPY --from=claude /usr/local/bin/claude /usr/local/bin/claude
COPY --from=build /out/worker /usr/local/bin/worker
USER worker
ENTRYPOINT ["/usr/local/bin/worker"]
