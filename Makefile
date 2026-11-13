SHELL := /bin/bash

# Images are tagged with the commit so a rollback can pick the matching ones.
GIT_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
export GIT_SHA

.PHONY: web web-dev web-image build up down logs deploy rollback build-arm64 test tts-test check routing-eval latency-report new-tool wakeword-test soloist soloist-update deploy-watch-install

build: ## Build every service image for this host.
	docker compose build

up: ## Start the stack in the background.
	mkdir -p exports
	bash scripts/soloist-setup.sh
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

deploy-watch-install: ## Install the Pi timer that deploys main when it moves (sudo).
	bash scripts/install-deploy-watch.sh

soloist: ## Install the official Soloist receiver binary and private dirs.
	bash scripts/soloist-setup.sh

soloist-update: ## Replace the receiver binary with the current official build.
	SOLOIST_UPDATE=1 bash scripts/soloist-setup.sh

build-arm64: ## Build all images for the Pi under QEMU (not for latency work).
	DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose build

web: ## Build the Dashboard React app into the Go embed directory.
	cd assistant && go run ./cmd/gentypes
	cd assistant/web && pnpm install --frozen-lockfile && pnpm build

web-image: ## Ship a UI-only change: rebuild dashboard-web, no Go rebuild or assistant restart.
	docker compose build dashboard-web
	docker compose up -d --no-deps dashboard-web

web-dev: ## Dashboard dev server; proxies to a local assistant (see assistant/web/README.md).
	cd assistant/web && pnpm install && pnpm dev

test: ## Run the assistant, deployment, wakeword-CLI and mic-check test suites.
	cd assistant && go test ./...
	bash scripts/tests/deploy_test.sh
	bash scripts/tests/deploy_watch_test.sh
	bash scripts/tests/check_compose_test.sh
	python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
	python3 -m unittest discover -s wakeword/tests -p 'test_*.py' -v

tts-test: ## Run the Wyoming/pcm Python tests (needs wyoming and numpy installed).
	python3 -m pytest tts/tests -q

check: ## Check compose and Dockerfile deployment conventions.
	bash scripts/check-compose.sh

# make exports the command-line `name` variable; the generator reads it from
# the environment so the value never becomes shell text.
new-tool: ## Scaffold a Tool module: make new-tool name=<module>.<tool>
	@test -n "$$name" || { \
		echo "usage: make new-tool name=<module>.<tool>" >&2; \
		exit 2; \
	}
	cd assistant && go run ./cmd/newtool

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

# The detector acceptance test builds the real wakeword image and drives the
# real Wyoming service; it needs Docker and the trained models/clanker.tflite.
wakeword-test: ## Build the wakeword image and assert clanker fires, near misses do not
	bash wakeword/tests/run_detection_test.sh
