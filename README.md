# nui

Self-hosted, passive usage analytics for your own web products. One host serves the collector under `/api` and the
dashboard under `/dashboard`.

- A browser script (`/nui.js`) and an optional server feed from your backend deliver events.
- The dashboard (behind OIDC) shows per-domain figures, trends and who is asking: people, known bots, uptime
  probes and unknown automation, each with the reasons it was classified that way.
- With an [llmwire](https://github.com/trick77/toolkit) model key it adds a short briefing and readings of
  automated visitors; every figure is computed without it.

## Development

```sh
cd ui && npm install
npm run dev        # http://127.0.0.1:5173, /api proxied to the Go server on :8080
npm test           # unit tests (vitest)
make check         # the full gate (ship), from the repo root
```

The Go server embeds the built UI: `npm run build` in `ui/`, then in `backend/`:

```sh
mkdir -p data && cp ../docs/sites.example.json data/sites.json
BACKEND_DATA_DIR=./data BACKEND_AUTH_DISABLED=true go run ./cmd/server -demo 30
```

and open <http://localhost:8080/dashboard/>. `-demo 30` fills an empty database with 30 days of synthetic traffic.

## Running it

`compose.yaml` runs the released image behind an existing Traefik:

```sh
cp .env.example .env    # set PUBLIC_FQDN, the OIDC client and BACKEND_SESSION_SECRET
mkdir -p data && sudo chown 65532:65532 data
docker compose pull && docker compose up -d
```

### Sites

nui only stores events for the sites listed in `data/sites.json` (see
[docs/sites.example.json](docs/sites.example.json)): domain, product, path rules that group raw paths into
templates, and for a server feed the SHA-256 of its key. Create a key and its hash with

```sh
docker compose run --rm nui -new-feed-key
```

The key goes to the product's feed emitter (`Authorization: Bearer <key>` on `POST /api/feed`), the hash into
`sites.json`. Restart nui after changing the file.

On a page, include the script:

```html
<script src="https://nui.example.com/nui.js" defer></script>
```

## License

MIT
