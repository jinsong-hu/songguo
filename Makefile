# Songguo — dev / build orchestration
.PHONY: dev backend frontend build install test clean

# Use bash so the cleanup function/loop below behaves consistently.
SHELL := /bin/bash

# Load .env (if present) and export its variables to all recipes.
-include .env
export

# Run the Go backend (:12345) and the Vite dev server (:12346) together.
# Vite proxies /api, /v1, /x, /healthz to the backend. Ctrl+C stops BOTH.
dev:
	@command -v go >/dev/null || { echo "go not found in PATH"; exit 1; }
	@test -d frontend/node_modules || (cd frontend && npm install)
	@echo "backend  -> http://127.0.0.1:12345"
	@echo "frontend -> http://127.0.0.1:12346   (open this)"
	@backend_pid=; frontend_pid=; \
	running() { \
		kill -0 "$$1" 2>/dev/null || return 1; \
		[ "$$(ps -o state= -p "$$1" 2>/dev/null | tr -d ' ')" != "Z" ]; \
	}; \
	stop() { \
		status=$${1:-$$?}; \
		trap - INT TERM EXIT; \
		echo; echo "stopping dev servers..."; \
		for sig in TERM TERM KILL; do \
			pids=$$({ \
				[ -n "$$backend_pid" ] && echo "$$backend_pid"; \
				[ -n "$$frontend_pid" ] && echo "$$frontend_pid"; \
				lsof -ti tcp:12345; \
				lsof -ti tcp:12346; \
			} 2>/dev/null | sort -u); \
			[ -z "$$pids" ] && break; \
			echo "$$pids" | xargs kill -$$sig 2>/dev/null || true; \
			sleep 0.4; \
		done; \
		[ -n "$$backend_pid" ] && wait "$$backend_pid" 2>/dev/null || true; \
		[ -n "$$frontend_pid" ] && wait "$$frontend_pid" 2>/dev/null || true; \
		exit "$$status"; \
	}; \
	trap 'stop 130' INT; \
	trap 'stop 143' TERM; \
	trap 'stop $$?' EXIT; \
	( cd backend && exec env SONGGUO_DB=$(CURDIR)/songguo.db SONGGUO_LISTEN=127.0.0.1:12345 go run ./cmd/songguo ) & \
	backend_pid=$$!; \
	( cd frontend && exec npm run dev ) & \
	frontend_pid=$$!; \
	while running "$$backend_pid" && running "$$frontend_pid"; do sleep 1; done; \
	if ! running "$$backend_pid"; then wait "$$backend_pid"; status=$$?; else wait "$$frontend_pid"; status=$$?; fi; \
	stop "$$status"

backend:
	cd backend && SONGGUO_DB=$(CURDIR)/songguo.db SONGGUO_LISTEN=127.0.0.1:12345 go run ./cmd/songguo

frontend:
	cd frontend && npm run dev

# Build the dashboard into the embed dir, then compile the single binary at repo root.
build:
	cd frontend && npm install && npm run build
	cd backend && go build -o $(CURDIR)/songguo ./cmd/songguo
	@echo "built ./songguo"

install:
	cd frontend && npm install

test:
	cd backend && go test ./...

clean:
	rm -f songguo songguo.db songguo.db-*
