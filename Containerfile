# syntax=docker/dockerfile:1
# The dashboard (ui/) built into the Go binary (backend/), which serves it. Port 8080, non-root.
FROM node:26-alpine AS frontend
WORKDIR /app/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui/ ./
# outDir ../backend/web/dist -> /app/backend/web/dist
RUN npm run build

FROM golang:1.27-alpine AS backend
ARG VERSION=dev
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=frontend /app/backend/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/trick77/ship/kit/version.Version=${VERSION}" -o /out/server ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=backend /out/server /app/server
USER 65532:65532
# Mounted by compose.yaml: /data (BACKEND_DATA_DIR, written by Trakk) and /conf (BACKEND_CONF_DIR, read-only).
EXPOSE 8080
# Exec form: distroless has no shell.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/app/server", "-healthcheck"]
ENTRYPOINT ["/app/server"]
