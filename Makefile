# ship:begin (owned by ship init)
# The gate: everything CI checks, configured in ship.yaml. Master's ship, as the pre-push hook and the @master
# workflows run it: its sha asked of GitHub (a proxy lags on @master). A local ship: SHIP_CMD=<go-installed binary>,
# one name for here and the hook, so make check and the push gate never run different ships.
# Exactly refs/heads/master (ls-remote matches by suffix), a 40-hex sha or nothing runs, as in the workflows.
SHIP_CMD ?= sha=$$(git ls-remote https://github.com/trick77/ship refs/heads/master | \
		awk '$$2 == "refs/heads/master" { print $$1 }'); \
	echo "$$sha" | grep -Eqx '[0-9a-f]{40}' || \
		{ echo "cannot resolve ship's master on GitHub: '$$sha' (git ls-remote, error above)"; exit 1; }; \
	go run github.com/trick77/ship@$$sha

.PHONY: check hooks lint

check:
	$(SHIP_CMD) check

hooks:
	$(SHIP_CMD) hooks

# Quick lint while editing, at the version check runs.
lint:
	$(SHIP_CMD) lint
# ship:end

# The UI against a simulated world: the real server (server -sim) on a temporary data directory, sign-in off, a
# seeded history backfilled and advancing as the dashboard asks, and Vite's dev server proxying to it. Ctrl-C stops
# both. Knobs: NUI_SIM_* (README). The server is built first: go run would not pass the stop on to it.
SIM_SERVER = $${TMPDIR:-/tmp}/nui-sim-server

.PHONY: sim sim-server

sim:
	cd backend && go build -o $(SIM_SERVER) ./cmd/server
	$(SIM_SERVER) -sim & pid=$$!; trap 'kill $$pid 2>/dev/null' EXIT INT TERM; \
	cd ui && NUI_API_TARGET=http://127.0.0.1:$${NUI_SIM_PORT:-8080} npm run dev

# Just the simulated server, for curl or a second UI process.
sim-server:
	cd backend && go build -o $(SIM_SERVER) ./cmd/server
	$(SIM_SERVER) -sim
