# nui

Self-hosted, passive usage analytics for your own web products. One host serves the collector under `/api` and the
dashboard under `/dashboard`.

## Development

```sh
cd ui && npm install
npm run dev        # http://127.0.0.1:5173, /api proxied to the Go server on :8080
npm test           # unit tests (vitest)
make check         # the full gate (ship), from the repo root
```

The Go server embeds the built UI: `npm run build` in `ui/`, then `go run ./cmd/server` in `backend/` and open
<http://localhost:8080/dashboard/>.

## Running it

`compose.yaml` runs the released image behind an existing Traefik:

```sh
cp .env.example .env    # set PUBLIC_FQDN
mkdir -p data && sudo chown 65532:65532 data
docker compose pull && docker compose up -d
```

## License

MIT
