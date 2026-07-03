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
	@echo "backend  -> http://localhost:12345"
	@echo "frontend -> http://localhost:12346   (open this)"
	@stop() { \
		echo; echo "stopping dev servers..."; \
		kill 0 2>/dev/null || true; \
		for sig in TERM TERM KILL; do \
			pids=$$({ lsof -ti tcp:12345; lsof -ti tcp:12346; } 2>/dev/null | sort -u); \
			[ -z "$$pids" ] && break; \
			echo "$$pids" | xargs kill -$$sig 2>/dev/null || true; \
			sleep 0.4; \
		done; \
	}; \
	trap stop INT TERM EXIT; \
	( cd backend && exec env SONGGUO_DB=$(CURDIR)/songguo.db SONGGUO_LISTEN=:12345 go run ./cmd/songguo ) & \
	( cd frontend && exec npm run dev ) & \
	wait

backend:
	cd backend && SONGGUO_DB=$(CURDIR)/songguo.db SONGGUO_LISTEN=:12345 go run ./cmd/songguo

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
