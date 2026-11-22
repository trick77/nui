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

## Dev / prod
- Dev: `npm run dev` in `ui/` (proxies `/api` to `127.0.0.1:8080`); full server: `npm run build` in `ui/`, then
  `go run ./cmd/server` in `backend/`, open `/dashboard/`.
- Gate: `make check` (= ship check; `make hooks` once per clone).
- Commit subjects + PR titles: Go style `area: what` (`backend: …`, `ui: …`, `deploy: …`, `repo: …`), gated by ship; release notes list them by area.
- Fresh clone: `ship hooks` before the first push (check, conform and init install it too); without the pre-push hook a push skips the gate.
- Prod: `compose.yaml` pulls `ghcr.io/trick77/nui` behind Traefik; `./data` owned by `65532:65532`.
- Release: Actions "Release" by hand (bump input); the first one `bump=minor`. Never dispatch Release yourself.
