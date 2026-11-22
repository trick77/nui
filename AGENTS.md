# AGENTS.md

Self-hosted, passive usage analytics for the user's own web products. One host: the collector under `/api`, the
dashboard SPA under `/dashboard` (OIDC). `backend/`: Go on ship's kit, embeds the UI. `ui/`: React 19, Vite, Tailwind v4.

## Decisions
- The app mounts `/dashboard` itself, no Traefik StripPrefix: collector and dashboard share the host. Vite base `./`,
  the server rewrites the shell's `./` refs to `/dashboard/` (kit's CSP has `base-uri 'none'`, so no `<base href>`),
  as ../reworm does. `/api` never falls back to the shell: a typo stays a 404, not HTML to a JSON caller.
- Design tokens and fonts are the family's (riptide's `index.css` `@theme` + `@font-face`): never retuned here, a
  change forks the family.
- Hostname only in `.env` (`PUBLIC_FQDN`), never in config, code or tests: examples use `example.com`. App variables
  are `BACKEND_*`.
- TLS: no certresolver, the server's Traefik holds a wildcard certificate.
- Schema: `docs/schema.md`, generated (`SCHEMADOC_UPDATE=1 go test ./... -run TestSchemaDoc` in `backend/`).
  Migration 0001 is unreleased until v1 ships: edit it, never add 0002 before the first release.
- No aggregate tables (user): every figure is computed from raw events at read time. A long range reads one short
  statement per bucket, never one long read: a read held for seconds stalls the WAL checkpoint while the collector
  writes (ipverse-core's lesson). `TestLargeTable` fails any statement whose plan scans `event`.
- Sites are declared in `<data>/sites.json` (`docs/sites.example.json`), never self-registering: a browser can name
  any host. Feed keys are stored as SHA-256 only; `server -new-feed-key` prints a key and its hash.
- Own prefix table (`internal/refdata`), JSON sites file: `gaissmai/bart` (173 stars) and `yaml/go-yaml` (571)
  failed the 1k-star rule. AS prefixes nest across owners: one sorted level per prefix length, never flat ranges.
- The model never makes a figure or an action: it picks fact ids and verdicts from fixed lists and writes short
  fields; a number in its text that the input lacks drops the text. Request text (paths, UAs, referrers) is
  hostile: data in the user message only. Everything works without `LLMWIRE_MIMO_API_KEY`.
- MiMo flash answers wrong with thinking off (llmwire profile notes): calls use `ReasoningBalanced`, not minimal.
- API errors are `{"detail": …}` (contract), not kit's `{"error": …}`: use `internal/apix`.
- Rate limit: `kit/ratelimit.Tiers`, per address or /64 plus per /48 (IPv4 /24), charged per event; the /64
  burst (60) must stay ≥ the 50-event batch, or every full batch is a 413.

## Dev / prod
- Dev: `npm run dev` in `ui/` (proxies `/api` to `127.0.0.1:8080`); full server: `npm run build` in `ui/`, then
  `go run ./cmd/server` in `backend/`, open `/dashboard/`. UI without a server: `npm run dev:fixtures` in `ui/`.
  Local data: `BACKEND_DATA_DIR=./data BACKEND_AUTH_DISABLED=true BACKEND_REFDATA_DISABLED=true go run ./cmd/server
  -demo 30` (needs `./data/sites.json`; auth-off is refused with `PUBLIC_FQDN` set).
- Gate: `make check` (= ship check; `make hooks` once per clone).
- Commit subjects + PR titles: Go style `area: what` (`backend: …`, `ui: …`, `deploy: …`, `repo: …`), gated by ship; release notes list them by area.
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
- Prod: `compose.yaml` pulls `ghcr.io/trick77/nui` behind Traefik; `./data` owned by `65532:65532`.
- Release: Actions "Release" by hand (bump input); the first one `bump=minor`. Never dispatch Release yourself.
