# Uses `docker compose` when the Docker CLI has the Compose plugin, `docker-compose` otherwise.
COMPOSE ?= $(shell docker compose version >/dev/null 2>&1 && echo "docker compose" || echo docker-compose)
DEV := $(COMPOSE) -f compose.yaml -f compose.dev.yaml

.PHONY: up dev down logs check-claude test

up: ## Start the production-like stack in the background
	$(COMPOSE) up --build --detach

dev: ## Start the stack with hot reload (stays in the foreground)
	$(DEV) up --build --watch

down: ## Stop the stack (dev or not), keeping the database volume
	$(COMPOSE) down

logs: ## Follow the logs of all services
	$(COMPOSE) logs --follow

check-claude: ## Make one tiny claude -p call from the worker image, to check the token
	$(COMPOSE) run --rm --build worker check

test: ## Run the backend tests and build the frontend
	cd backend && go test ./...
	cd frontend && npm ci --no-fund --no-audit && npm run build && npm run lint
