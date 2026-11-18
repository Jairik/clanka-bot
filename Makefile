SHELL := /bin/bash

# Images are tagged with the commit so a rollback can pick the matching ones.
GIT_SHA ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
export GIT_SHA

.PHONY: web web-dev web-image build up down logs doctor deploy rollback prune-images build-arm64 test tts-test stt-test check routing-eval latency-report new-tool wakeword-test soloist soloist-update spotify-up deploy-watch-install backup smoke

build: ## Build every service image for this host.
	docker compose build

up: ## Start the stack in the background (Spotify receiver is opt-in).
	mkdir -p exports
	@if [ ! -w exports ]; then \
		echo "error: ./exports is not writable (likely owned by root from a previous docker compose up)." >&2; \
		echo "fix it with: sudo chown -R $$(id -u):$$(id -g) exports" >&2; \
		exit 1; \
	fi
	bash scripts/soloist-setup.sh --optional
	docker compose up -d

spotify-up: ## Start the stack plus the Spotify receiver (requires a Soloist key).
	mkdir -p exports
	@if [ ! -w exports ]; then \
		echo "error: ./exports is not writable (likely owned by root from a previous docker compose up)." >&2; \
		echo "fix it with: sudo chown -R $$(id -u):$$(id -g) exports" >&2; \
		exit 1; \
	fi
	bash scripts/soloist-setup.sh
	COMPOSE_PROFILES=spotify docker compose up -d

down: ## Stop the stack.
	docker compose down

logs: ## Follow the stack's logs.
	docker compose logs -f

doctor: ## Host, Pulse, disk, and key preflight before deploy.
	bash scripts/doctor.sh

deploy: ## Pull, build, start and wait for health (see docs/deploy.md).
	bash scripts/deploy.sh $(SHA)

rollback: ## Reuse an earlier commit's images: make rollback SHA=<commit>
	@test -n "$(SHA)" || { echo "usage: make rollback SHA=<commit>" >&2; exit 2; }
	bash scripts/deploy.sh "$(SHA)"

prune-images: ## Reclaim old SHA-tagged images; keeps the deployed set and DEPLOY_KEEP_IMAGES.
	bash scripts/prune-images.sh

deploy-watch-install: ## Install the Pi timer that deploys main when it moves (sudo).
	bash scripts/install-deploy-watch.sh

soloist: ## Install the official Soloist receiver binary and private dirs.
	bash scripts/soloist-setup.sh

soloist-update: ## Replace the receiver binary with the current official build.
	SOLOIST_UPDATE=1 bash scripts/soloist-setup.sh

build-arm64: ## Build all images for the Pi under QEMU (not for latency work).
	DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose build

backup: ## Download an authenticated SQLite backup of the assistant database.
	@mkdir -p backups
	@test -n "$$OWNER" || { echo "usage: make backup OWNER=you@tailnet.example.com" >&2; exit 2; }
	@# Publish by link so a second backup cannot overwrite the finished file.
	@set -e; umask 077; out=backups/assistant-$$(date +%Y%m%d-%H%M%S).db; \
	test ! -e "$$out" || { echo "backup already exists: $$out" >&2; exit 1; }; \
	tmp=$$(mktemp backups/.assistant-backup.XXXXXX); \
	trap 'rm -f "$$tmp"' EXIT; \
	curl -fsS -X POST -H "Tailscale-User-Login: $$OWNER" -H 'Sec-Fetch-Site: same-origin' http://127.0.0.1:8082/api/ui/backup -o "$$tmp"; \
	test -s "$$tmp"; ln -T "$$tmp" "$$out"; \
	echo "Wrote $$out ($$(wc -c < "$$out") bytes)"; \
	ls -lh "$$out"

smoke: ## Post-deploy smoke turn: prove the pipeline answers a typed request.
	bash scripts/smoke.sh

web: ## Build the Dashboard React app into the Go embed directory.
	cd assistant && go run ./cmd/gentypes
	cd assistant/web && pnpm install --frozen-lockfile && pnpm build

web-image: ## Ship a UI-only change: rebuild dashboard-web, no Go rebuild or assistant restart.
	docker compose build dashboard-web
	docker compose up -d --no-deps dashboard-web

web-dev: ## Dashboard dev server; proxies to a local assistant (see assistant/web/README.md).
	cd assistant/web && pnpm install && pnpm dev

test: ## Run the assistant, deployment, auto-deploy, wakeword-CLI, STT, prototype-run and mic-check suites, plus the real compose check.
	cd assistant && go test ./...
	bash scripts/tests/doctor_test.sh
	bash scripts/tests/deploy_test.sh
	bash scripts/tests/smoke_test.sh
	bash scripts/tests/prune_images_test.sh
	bash scripts/tests/deploy_watch_test.sh
	bash scripts/tests/check_compose_test.sh
	bash scripts/tests/check_dockerignore_test.sh
	python3 -m unittest discover -s scripts/tests -p 'test_*.py' -v
	python3 -m unittest discover -s scripts/prototype-run/tests -p 'test_*.py' -v
	python3 -m unittest discover -s wakeword/tests -p 'test_*.py' -v
	python3 -m pytest tts/tests stt/tests -q
	bash scripts/check-compose.sh

tts-test: ## Run the Wyoming/pcm and STT Python tests (needs wyoming and numpy installed).
	python3 -m pytest tts/tests stt/tests -q

stt-test: ## Run the STT Wyoming server tests (needs wyoming and pytest installed).
	python3 -m pytest stt/tests -q

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
