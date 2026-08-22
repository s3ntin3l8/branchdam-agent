# syntax=docker/dockerfile:1

# --- Stage 1: build the Go binary ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN VERSION="$(sed -n 's/.*: *"\([0-9][^"]*\)".*/\1/p' .release-please-manifest.json)"; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION:-dev}" \
      -o /out/server ./cmd/server

# --- Stage 2: minimal runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/server", "-config", "/config/config.yaml", "-healthcheck"]
ENTRYPOINT ["/server"]
CMD ["-config", "/config/config.yaml"]
