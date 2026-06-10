# Songguo — dev / build orchestration
.PHONY: dev backend frontend build install test clean

# Run the Go backend (:8080) and the Vite dev server (:5173) together.
# Vite proxies /api, /v1, /x, /healthz to the backend. Ctrl+C stops BOTH.
dev:
	@command -v go >/dev/null || { echo "go not found in PATH"; exit 1; }
	@test -d frontend/node_modules || (cd frontend && npm install)
	@echo "backend  -> http://localhost:8080"
	@echo "frontend -> http://localhost:5173   (open this)"
	@trap 'kill 0' INT TERM EXIT; \
		( cd backend && SONGGUO_DB=$(CURDIR)/songguo.db SONGGUO_LISTEN=:8080 go run ./cmd/songguo ) & \
		( cd frontend && npm run dev ) & \
		wait

backend:
	cd backend && SONGGUO_DB=$(CURDIR)/songguo.db SONGGUO_LISTEN=:8080 go run ./cmd/songguo

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
