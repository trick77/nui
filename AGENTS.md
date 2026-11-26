# AGENTS.md

Self-hosted, passive web traffic observability, never "web analytics" in copy (user: no funnels). One host: collector
under `/api`, dashboard under `/dashboard`. `backend/`: Go on ship's kit, embeds `ui/`.

`INTEGRATION.md` is what other repos' agents read to add Trakk: a change to `/trakk.js`, `/api/in`,
`/api/feed` or `sites.json` updates it in the same commit, and trick77/trakk-wordpress, whose CI runs `trakk:latest`.

## Decisions
- The app mounts `/dashboard` itself, no Traefik StripPrefix. The server rewrites the shell's `./` refs: kit's CSP
  has `base-uri 'none'`, so no `<base href>`. `/api` never falls back to the shell: a typo stays a 404.
- Design tokens and fonts are the family's (riptide's `index.css`): a change here forks the family.
- Hostname only in `.env` (`PUBLIC_FQDN`); code, tests and examples use `example.com`. TLS: Traefik's wildcard
  certificate, no certresolver.
- Stored names never carry the product's name (user): database file, cookies, session key label, `localStorage`
  keys, simulator markers, env vars, tables. A rename once started the server on an empty
  database beside its data and would have signed everyone out and counted every browser as new.
- Migrations: never edit an applied one; the next number, DDL only, data steps are background jobs (0004 copies two
  tables: a CHECK changes no other way, owner's OK). `docs/schema.md` is generated: `SCHEMADOC_UPDATE=1 go test ./... -run TestSchemaDoc` in `backend/`.
- Aggregates (owner): every raw change (writes, restamps, retention's cuts) bumps a built day's version in the same
  transaction, or figures drift unseen. Permanent once raw is gone: the sketch and the
  duration-histogram formats and the dimension definitions; change either only with a builder version bump. The last 3 days are unbuilt: raw lost there is lost
  for good. Known: aud=all visits count a session with rows in both parts twice; a classified day has none.
- Raw purge: a day loses raw only once its aggregate rows are complete (days up to the months' cutoff excepted),
  else its figures read empty for good. 7 days' grace from a purging release's first start: figures checked against
  whole raw. Raw days ≥ 15: 7d and its prior period read raw from local midnight, any zone.
- A statement over a site's events or aggregates spans at most a day: a read held for seconds keeps the WAL from
  restarting while the collector writes (ipverse-core's lesson; a month's actor GROUP BY took 0.8 s on 24 M events,
  a day 0.02 s). A 12-month page took ~60 s raw at that size.
- Script hosts ride `engagement`'s props: a new kind is an action to the aggregates and a 400 to an older `/api/in`.
- Feed: one event's content never refuses the batch, its fields are the product's public clients' (one crafted
  method would blind the feed). Clip, drop and count; `/api/in` stays strict.
- `account` outranks the bot list (owner): the list held our own host as Amazonbot (a scraper read its own address
  off the page), so an MCP client with a good key read as a known bot.
- Bot list: no code per service (owner). Its name is stored (`event.svc`, `services` rows), frozen per built day: no
  day with bots built without a list (names freeze empty). Purpose, group and an agent's parent service join today's
  list at read: no permanent vocabulary. Signature-Agent: shown, no signal, never the model's.
- Sites never self-register: a browser can name any host.
- Own prefix table, JSON sites file: `gaissmai/bart` (173 stars) and `yaml/go-yaml` (571) failed the 1k-star rule.
  AS prefixes nest across owners: one sorted level per prefix length, never flat ranges.
- The model never makes a figure, an action or an alert: it picks fact ids and verdicts from fixed lists and writes short
  fields. A digit or number word stating a quantity drops the text (first real run: every briefing line repeated
  the figure beside it); "one" only as a quantity, as a determiner it dropped plain lines. Every fixed choice is
  spelled out in the prompt: with the options only in the schema's enum the model mapped 4 of 4 readings onto a
  verdict its own summary contradicted. Readings are cached per behaviour signature, so they hold no address,
  network name or date. Request text (paths, UAs, referrers) is hostile: data in the user message only.
- Automation probes: the CDP trick (stack getter via `console.debug`) fired in no run on Chrome 159, Puppeteer's
  `Runtime.enable` included. A WebGL context took up to 411 ms.
- The `pushState` wrapper remembers what it rethrows: WebKit masks errors through `trakk.js` as "Script error.", CORS
  on the tag does not help (measured, 27.2).
- Simulator: drives the real collector, classifier and dashboard, never a copy of their logic (hand-written
  fixtures drifted and hid defects). Documentation addresses only.
- Rate limit: one token on arrival, then one per event or per 2 KiB stored, whichever is more (490 caller-named
  headers once stored 1.4 MB for one token). The /64 burst must stay ≥ the event batch and ≥ the batch byte
  bound / 2 KiB, or such a batch is a 413.
- Alerts notify transitions; a baseline (a rule's first pass, new signature, 6 h idle) sends nothing, else a
  loosened rule floods. Thresholds never reach Noteworthy (`factsHash`); a notify.json fault never stops collecting.

## Dev / prod
- Dev: `make sim` (README).
- Gate: `make check` (= ship check; `make hooks` once per clone).
- Commit subjects + PR titles: Go style `area: what` (`backend: …`, `ui: …`, `deploy: …`, `repo: …`, `sim: …`), gated by ship; release notes list them by area.
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
