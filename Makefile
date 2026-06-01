# BlueShip — common development tasks. Run `make` (or `make help`) to list them.
.DEFAULT_GOAL := help
.PHONY: help setup db down run build test vet fmt tidy

help: ## List available tasks
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

setup: ## One-time setup: start Postgres, fetch deps, create .env
	@test -f .env || cp .env.example .env
	go mod download
	docker compose up -d db
	@echo ""
	@echo "Setup complete. Edit .env with your ANTHROPIC_API_KEY and TELEGRAM_BOT_TOKEN,"
	@echo "then start your agent with:  make run"

db: ## Start the local Postgres container
	docker compose up -d db

down: ## Stop the local Postgres container
	docker compose down

run: ## Run the minimal example agent (loads .env)
	@test -f .env || { echo "no .env — run 'make setup' first (or cp .env.example .env)"; exit 1; }
	@set -a; . ./.env; set +a; go run ./examples/minimal

build: ## Build everything
	go build ./...

test: ## Run tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go files
	gofmt -w .

tidy: ## Tidy go.mod
	go mod tidy
