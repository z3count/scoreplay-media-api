# Multi-stage build for minimal production image.
#
# Stage 1 (builder): compile the Go binary with CGO disabled.
# Stage 2 (runtime): copy the binary into a distroless image.
#
# Why distroless?
#   - No shell, no package manager → minimal attack surface.
#   - ~2MB base image vs ~5MB for Alpine.
#   - OWASP: "Remove all unnecessary functionality and files."
#
# Security:
#   - Runs as non-root user (65534:65534 = nobody).
#   - No OS-level dependencies (static Go binary).
#   - Build args are not baked into the final image.

# ── Stage 1: Build ─────────────────────────────
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache dependencies first (Docker layer caching).
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/media-api ./cmd/api/

# ── Stage 2: Runtime ───────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# Copy the compiled binary from the builder stage.
COPY --from=builder /bin/media-api /media-api

# Create uploads directory (writable by nonroot).
# distroless doesn't have mkdir, so we copy an empty dir from builder.
COPY --from=builder --chown=65534:65534 /tmp /uploads

# Expose the default port.
EXPOSE 8080

# Run as non-root user (OWASP: least privilege).
USER 65534:65534

ENTRYPOINT ["/media-api"]
