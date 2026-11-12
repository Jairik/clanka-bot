SHELL := /bin/bash

# Images are tagged with the commit so a rollback can pick the matching ones.
GIT_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
export GIT_SHA

.PHONY: build up down logs deploy rollback build-arm64 test check routing-eval latency-report

build: ## Build every service image for this host.
	docker compose build

up: ## Start the stack in the background.
	docker compose up -d

down: ## Stop the stack.
	docker compose down

logs: ## Follow the stack's logs.
	docker compose logs -f

deploy: ## Pull, build, start and wait for health (see docs/deploy.md).
	bash scripts/deploy.sh $(SHA)

rollback: ## Reuse an earlier commit's images: make rollback SHA=<commit>
	@test -n "$(SHA)" || { echo "usage: make rollback SHA=<commit>" >&2; exit 2; }
	bash scripts/deploy.sh "$(SHA)"

build-arm64: ## Build all images for the Pi under QEMU (not for latency work).
	DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose build

test: ## Run the assistant, deployment and mic-check test suites.
	cd assistant && go test ./...
	bash scripts/tests/deploy_test.sh
	bash scripts/tests/check_compose_test.sh
	python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v

check: ## Check compose and Dockerfile deployment conventions.
	bash scripts/check-compose.sh

routing-eval: ## Real-MiniLM routing check; enforces the 0.95 per-Tool floor.
	@test -n "$(ROUTING_EVAL_EMBED_URL)" || { \
		echo "usage: make routing-eval ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085" >&2; \
		echo "start the real embed service first; see docs/routing-eval.md" >&2; \
		exit 2; \
	}
	cd assistant && ROUTING_EVAL_EMBED_URL=$(ROUTING_EVAL_EMBED_URL) \
		go test ./internal/pipeline -run TestRoutingEvalAgainstMiniLM -count=1 -v

latency-report: ## Summarize per-stage Response latency from the running stack.
	bash scripts/latency-report.sh
