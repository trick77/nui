# AGENTS.md

Self-hosted, passive usage analytics for the user's own web products. One host: the collector under `/api`, the
dashboard SPA under `/dashboard` (OIDC, an admin token, or both). `backend/`: Go on ship's kit, embeds the UI. `ui/`: React 19, Vite, Tailwind v4.

Adding nui to another product ("integrate nui"): `docs/integrate.md`. It is what other repos' agents read: a change
to `/nui.js`, `/api/in`, `/api/feed` or `sites.json` updates it in the same commit.

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
- No aggregate tables (user): every figure is computed from raw events at read time. A statement over all of a site's
  events spans at most a day (`readSpan`: month buckets in day pieces, log and footer counts in windows): a read held
  for seconds keeps the WAL from restarting while the collector writes (ipverse-core's lesson; a month's actor GROUP
  BY took 0.8 s native on 24 M events, a day 0.02 s). One address's reads (visitor detail) span the range on the
  actor index. `TestLargeTable` fails any statement whose plan scans `event`. At 24 M events (25 months, ~31k/day) a
  12-month page took ~60 s: many short reads, not one long one.
- Feed: one event's content never refuses the batch, its fields are the product's public clients' (one crafted
  method would blind the feed). Clip to the README's limits, drop and count unusable events; `/api/in` stays strict.
- Sites are declared in `<data>/sites.json` (`docs/sites.example.json`), never self-registering: a browser can name
  any host. Feed keys are stored as SHA-256 only; `server -new-feed-key` prints a key and its hash.
- Own prefix table (`internal/refdata`), JSON sites file: `gaissmai/bart` (173 stars) and `yaml/go-yaml` (571)
  failed the 1k-star rule. AS prefixes nest across owners: one sorted level per prefix length, never flat ranges.
- The model never makes a figure or an action: it picks fact ids and verdicts from fixed lists and writes short
  fields; any digit or number word stating a quantity drops the text (first real run: every briefing line repeated
  the figure shown beside it, "four pages" slipped through). "one" only as a quantity ("only one"): as a determiner it
  dropped plain lines ("One operator…"). Every fixed choice is spelled out in the prompt: with the
  options only in the schema's enum the model mapped 4 of 4 readings onto a verdict its own summary contradicted.
  Readings are cached per behaviour signature, so they hold no address, network name or date; evidence ids resolve
  against the actor shown. A briefing shows only for the fact-id set it was written for (ids carry direction).
  Request text (paths, UAs, referrers) is hostile: data in the user message only. Everything works without
  `LLMWIRE_MIMO_API_KEY`.
- Rate limit: one token on arrival, before the body is read, then one per event or per 2 KiB stored, whichever is
  more (page URL, UA and ids repeat on every event: 490 caller-named headers once stored 1.4 MB for one token). The
  /64 burst (60) must stay ≥ the 50-event batch and ≥ the batch byte bound / 2 KiB, or such a batch is a 413.

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
