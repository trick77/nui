# AGENTS.md

Self-hosted, passive usage analytics for the user's own web products. One host: the collector under `/api`, the
dashboard SPA under `/dashboard` (OIDC, an admin token, or both). `backend/`: Go on ship's kit, embeds the UI. `ui/`: React 19, Vite, Tailwind v4.

Adding Stakeout to another product ("integrate Stakeout"): `INTEGRATION.md`. It is what other repos' agents read: a
change to `/stakeout.js`, `/api/in`, `/api/feed` or `sites.json` updates it in the same commit.

## Decisions
- The app mounts `/dashboard` itself, no Traefik StripPrefix: collector and dashboard share the host. Vite base `./`,
  the server rewrites the shell's `./` refs to `/dashboard/` (kit's CSP has `base-uri 'none'`, so no `<base href>`),
  as ../reworm does. `/api` never falls back to the shell: a typo stays a 404, not HTML to a JSON caller.
- Design tokens and fonts are the family's (riptide's `index.css` `@theme` + `@font-face`): never retuned here, a
  change forks the family.
- Hostname only in `.env` (`PUBLIC_FQDN`), never in config, code or tests: examples use `example.com`. App variables
  are `BACKEND_*`.
- Stored names never carry the product's name (user): database file (`store.FileName`), cookies, session key label,
  `localStorage` keys, simulator markers. A rename once started the server on an empty database beside its data and
  would have signed everyone out and counted every browser as new.
- TLS: no certresolver, the server's Traefik holds a wildcard certificate.
- Schema: `docs/schema.md`, generated (`SCHEMADOC_UPDATE=1 go test ./... -run TestSchemaDoc` in `backend/`).
  0001 shipped in 0.1.0: never edit an applied migration, add the next number, DDL only; data steps are background
  jobs.
- Aggregates (owner): per site and UTC day, built at 02:00 two days on; 24h, 7d and their prior period read raw,
  the rest a frozen, clean day's aggregates, else raw. Every raw change (writes, restamps, retention's cuts) bumps
  a built day's version in the same transaction, or figures drift unseen. Permanent once raw is gone: the sketch format
  (pinned by tests) and the dimension definitions; change either only with a builder version bump. The size cap
  never cuts the last 3 days: unbuilt, once lost for good. aud=all visits count a session with rows in both parts
  twice; a classified day has none.
- A statement over a site's events or aggregates spans at most a day (`readSpan`): a read held for seconds keeps
  the WAL from restarting while the collector writes (ipverse-core's lesson; a month's actor GROUP BY took 0.8 s
  on 24 M events, a day 0.02 s). `internal/plancheck` fails a plan walking a table or a whole site; `agg_addr_key`
  is pinned (else the planner walks the day's every address). At 24 M events a 12-month page took ~60 s raw.
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
- Automation probes: the CDP trick (stack getter via `console.debug`) fired in no run on Chrome 159, Puppeteer's
  `Runtime.enable` included. A WebGL context took up to 411 ms.
- Simulator (`internal/sim`): drives the real collector, classifier and dashboard, never a copy of their logic
  (hand-written fixtures drifted and hid defects). Documentation addresses only, its own ipverse-format ref data.
- Rate limit: one token on arrival, before the body is read, then one per event or per 2 KiB stored, whichever is
  more (page URL, UA and ids repeat on every event: 490 caller-named headers once stored 1.4 MB for one token). The
  /64 burst (60) must stay ≥ the 50-event batch and ≥ the batch byte bound / 2 KiB, or such a batch is a 413.

## Dev / prod
- Dev: `make sim` (UI on a simulated world, `STAKEOUT_SIM_*`, README). `npm run dev` in `ui/` proxies `/api` to
  `127.0.0.1:8080` (`STAKEOUT_API_TARGET`); full server: `npm run build` in `ui/`, then `go run ./cmd/server` in
  `backend/`. UI without a server: `npm run dev:fixtures` in `ui/`. `-demo 30`: README.
- Gate: `make check` (= ship check; `make hooks` once per clone).
- Commit subjects + PR titles: Go style `area: what` (`backend: …`, `ui: …`, `deploy: …`, `repo: …`, `sim: …`), gated by ship; release notes list them by area.
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
- Prod: `compose.yaml` pulls `ghcr.io/trick77/stakeout` behind Traefik; `./data` owned by `65532:65532`.
- Release: Actions "Release" by hand (bump input); the first one `bump=minor`. Never dispatch Release yourself.
