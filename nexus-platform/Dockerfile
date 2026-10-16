# Multi-stage build for the Nexus platform server.
# Stage 1: build a static binary; Stage 2: minimal runtime image.

# Go toolchain must satisfy go.mod's `go 1.25.0` directive (GOTOOLCHAIN=local in the image).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/server ./cmd/server
# bootstrap-admin ships in the same image (issue #95 / queue #59). Migration 000066 removes the
# source-committed demo admin, so on a fresh deploy this tool is the ONLY way to obtain a first
# login — and an operator tool that is not in the image is not a way. It was written, documented
# in the go-live checklist, and never built: the checklist's command named a compose service
# ("app") that does not exist, against an image that did not carry the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/bootstrap-admin ./cmd/bootstrap-admin

FROM alpine:3.20
# /app/storage is the LocalBackend root for uploaded blobs (FILE_STORAGE_DIR, pinned in
# compose.prod.yml). It MUST exist and be owned by `nexus` BEFORE the USER switch: a named
# volume mounted onto a path the image does not create inherits root:root, and the app —
# running as uid 10001 — then fails every upload with 500 FILE_OP_FAILED. Creating + chowning
# it here makes Docker seed the volume's ownership from this directory instead.
# /app/tenant-backups is the SAME story for the per-customer backup zips (TENANT_BACKUP_DIR,
# specs/068): a second volume, a second directory the app must own before the USER switch.
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 nexus \
	&& mkdir -p /app/storage /app/tenant-backups && chown -R nexus:nexus /app
USER nexus
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /out/bootstrap-admin /app/bootstrap-admin
EXPOSE 8080
ENTRYPOINT ["/app/server"]
