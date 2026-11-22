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
# both. Knobs: NUI_SIM_* (README). The server is built first: go run would not pass the stop on to it. The UI starts
# only once this server answers (its pid in X-Nui-Sim-Pid, not whatever else listens on the port), on the port the
# server itself reads from NUI_SIM_PORT.
SIM_SERVER = $${TMPDIR:-/tmp}/nui-sim-server

.PHONY: sim sim-server

sim:
	cd backend && go build -o $(SIM_SERVER) ./cmd/server
	@port=$$($(SIM_SERVER) sim port) || exit 1; \
	$(SIM_SERVER) -sim & pid=$$!; trap 'kill $$pid 2>/dev/null' EXIT INT TERM; \
	echo "make sim: waiting for the simulated server (pid $$pid) on 127.0.0.1:$$port"; \
	waited=0; \
	until curl -s -o /dev/null -D - "http://127.0.0.1:$$port/api/dash/sites" 2>/dev/null | tr -d '\r' | \
		grep -qix "x-nui-sim-pid: $$pid"; do \
		if ! kill -0 $$pid 2>/dev/null; then \
			echo "make sim: the simulated server stopped before it answered on port $$port (in use? its log is above; NUI_SIM_PORT picks another)" >&2; \
			exit 1; \
		fi; \
		waited=$$((waited + 1)); \
		if [ $$waited -gt 1200 ]; then \
			echo "make sim: no answer from the simulated server on port $$port in 10 minutes (curl missing? its log is above)" >&2; \
			exit 1; \
		fi; \
		sleep 0.5; \
	done; \
	cd ui && NUI_API_TARGET=http://127.0.0.1:$$port npm run dev

# Just the simulated server, for curl or a second UI process.
sim-server:
	cd backend && go build -o $(SIM_SERVER) ./cmd/server
	$(SIM_SERVER) -sim
