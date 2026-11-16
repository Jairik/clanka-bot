#!/usr/bin/env bash
# Build-context hygiene tests for assistant/.dockerignore.
#
# The assistant image does `COPY . .`, so anything left in the context reaches
# the builder and its cache. A local run outside Compose creates data/, whose
# unencrypted secrets table holds the write-only values (docs/guides/README.md
# "Secrets"), and
# `go build` leaves the dev binary behind; a build that exports its cache would
# carry both off the machine. The assertions below pin the ignore file, then
# build a scratch context through Docker when it is available so the real
# matching rules decide, not a hand-rolled matcher.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
IGNORE="$REPO_ROOT/assistant/.dockerignore"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/check-dockerignore-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
SKIP=0

pass() { PASS=$((PASS + 1)); }

skip() {
	SKIP=$((SKIP + 1))
	printf 'SKIP: %s\n' "$*"
}

fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL: %s\n' "$*"
}

assert_ignored() {
	local pattern=$1
	if grep -qxF -- "$pattern" "$IGNORE"; then
		pass
	else
		fail "assistant/.dockerignore has no exact '$pattern' line"
	fi
}

assert_kept() {
	local path=$1
	if grep -qxF -- "$path" "$IGNORE"; then
		fail "assistant/.dockerignore ignores '$path', which the image needs"
	else
		pass
	fi
}

test_ignores_the_local_database_and_dev_binary() {
	local pattern
	# `data` is the directory `assistant` creates outside Compose (ADR-0003).
	for pattern in data assistant '**/*.db' '**/*.db-wal' '**/*.db-shm' '**/*.log'; do
		assert_ignored "$pattern"
	done
}

test_ignores_host_build_artefacts() {
	local pattern
	for pattern in .git web/node_modules web/test-results web/playwright-report internal/dashboard/ui/dist; do
		assert_ignored "$pattern"
	done
}

test_keeps_what_the_image_needs() {
	local path
	for path in config.yaml go.mod go.sum cmd internal tools web web/package.json; do
		assert_kept "$path"
	done
	# A catch-all would strip the source tree and silently break the build.
	if grep -qE '^[[:space:]]*(\*|\*\*|\*/\*)[[:space:]]*$' "$IGNORE"; then
		fail "assistant/.dockerignore has a catch-all pattern"
	else
		pass
	fi
}

docker_usable() {
	command -v docker >/dev/null 2>&1 &&
		docker info >/dev/null 2>&1 &&
		docker buildx version >/dev/null 2>&1
}

test_scratch_context_drops_local_artefacts() {
	if ! docker_usable; then
		skip "docker/buildx unavailable: the scratch-context check did not run"
		return
	fi

	local ctx="$TMP/context" out="$TMP/out" entry
	mkdir -p "$ctx/data/voices" "$ctx/web/test-results" "$ctx/web/playwright-report" \
		"$ctx/web/node_modules/pkg" "$ctx/.git/objects" "$ctx/cmd/assistant" \
		"$ctx/internal/dashboard/ui/dist" "$ctx/custom/state" "$ctx/web/src" "$out"
	cp "$IGNORE" "$ctx/.dockerignore"
	printf 'FROM scratch\nCOPY . /ctx\n' >"$ctx/Dockerfile"

	# Fake host artefacts next to the files the image really needs.
	printf 'CREATE TABLE secrets;\n' >"$ctx/data/assistant.db"
	printf 'dummy wal\n' >"$ctx/data/assistant.db-wal"
	printf 'dummy log\n' >"$ctx/data/assistant.log"
	# A custom ASSISTANT_DATA_DIR can be nested anywhere in the build context.
	for entry in assistant.db assistant.db-wal assistant.db-shm assistant.log; do
		printf 'private fixture\n' >"$ctx/custom/state/$entry"
		printf 'private fixture\n' >"$ctx/$entry"
	done
	printf 'binary\n' >"$ctx/assistant"
	printf 'ref: main\n' >"$ctx/.git/HEAD"
	printf 'x\n' >"$ctx/web/test-results/report.json"
	printf 'x\n' >"$ctx/web/playwright-report/index.html"
	printf 'x\n' >"$ctx/web/node_modules/pkg/index.js"
	printf 'x\n' >"$ctx/internal/dashboard/ui/dist/index.js"
	printf '{}\n' >"$ctx/config.yaml"
	printf 'module example\n' >"$ctx/go.mod"
	printf '' >"$ctx/go.sum"
	printf 'package main\n' >"$ctx/cmd/assistant/main.go"
	printf 'package internal\n' >"$ctx/internal/x.go"
	printf '{}\n' >"$ctx/web/package.json"
	printf '// Required frontend source fixture.\nexport {};\n' >"$ctx/web/src/main.tsx"
	printf '/* Required frontend style fixture. */\nbody {}\n' >"$ctx/web/src/style.css"
	printf '// Required Vite configuration fixture.\nexport default {};\n' >"$ctx/web/vite.config.ts"
	printf '{}\n' >"$ctx/web/tsconfig.json"
	printf 'lockfileVersion: 9\n' >"$ctx/web/pnpm-lock.yaml"

	if ! docker buildx build --progress=plain --output "type=local,dest=$out" "$ctx" \
		>"$TMP/docker.log" 2>&1; then
		fail "scratch context build failed"
		cat "$TMP/docker.log"
		return
	fi

	for entry in data/assistant.db data/assistant.db-wal data/assistant.log assistant \
		assistant.db assistant.db-wal assistant.db-shm assistant.log \
		custom/state/assistant.db custom/state/assistant.db-wal custom/state/assistant.db-shm custom/state/assistant.log \
		.git web/test-results web/playwright-report web/node_modules \
		internal/dashboard/ui/dist; do
		if [[ -e "$out/ctx/$entry" ]]; then
			fail "build context still carries $entry"
		else
			pass
		fi
	done

	for entry in config.yaml go.mod go.sum cmd/assistant/main.go internal/x.go web/package.json \
		web/src/main.tsx web/src/style.css web/vite.config.ts web/tsconfig.json web/pnpm-lock.yaml; do
		if [[ -e "$out/ctx/$entry" ]]; then
			pass
		else
			fail "build context dropped $entry, which the image needs"
		fi
	done
}

run_test() {
	printf '== %s\n' "$1"
	"$1"
}

run_test test_ignores_the_local_database_and_dev_binary
run_test test_ignores_host_build_artefacts
run_test test_keeps_what_the_image_needs
run_test test_scratch_context_drops_local_artefacts

printf '\n%d passed, %d failed, %d skipped\n' "$PASS" "$FAIL" "$SKIP"
[[ "$FAIL" -eq 0 ]]
