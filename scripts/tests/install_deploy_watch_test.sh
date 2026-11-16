#!/usr/bin/env bash
# Black-box tests for scripts/install-deploy-watch.sh.
#
# The installer is exercised without root against a scratch unit directory
# (SYSTEMD_UNIT_DIR) and a stub systemctl. `id` is a scripted double so a
# non-1000 UID can be fed in, proving the unit's XDG_RUNTIME_DIR is resolved
# from the real user instead of silently assuming UID 1000.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
INSTALL_SRC="$REPO_ROOT/scripts/install-deploy-watch.sh"
WATCH_SRC="$REPO_ROOT/scripts/deploy-watch.sh"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/install-deploy-watch-test.XXXXXX")"
STUB_BIN="$TMP/bin"
mkdir -p "$STUB_BIN"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
STATUS=0
OUT=""

pass() { PASS=$((PASS + 1)); }
fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL: %s\n' "$*"
	if [[ -f "$OUT" ]]; then
		printf -- '--- output ---\n'
		tail -n 30 "$OUT"
		printf -- '--------------\n'
	fi
}

# A systemctl double that records its calls and succeeds.
write_systemctl_stub() {
	cat >"$STUB_BIN/systemctl" <<'STUB'
#!/usr/bin/env bash
dir="${STUB_SYSTEMCTL_DIR:?STUB_SYSTEMCTL_DIR is required}"
printf 'systemctl %s\n' "$*" >>"$dir/systemctl-calls"
exit 0
STUB
	chmod +x "$STUB_BIN/systemctl"
}

# An `id` double: returns UID 1042 (and name fakeuser) for the deploy user,
# unless FAIL_USER is set to that user, in which case `id -u <user>` exits 1 so
# the installer must error out cleanly instead of assuming UID 1000.
write_id_stub() {
	cat >"$STUB_BIN/id" <<'STUB'
#!/usr/bin/env bash
arg1="${1:-}"
if [[ "$arg1" == "-u" ]]; then
	if [[ "$2" == "fakeuser" || "$2" == "baduser" ]]; then
		if [[ -n "${FAIL_USER:-}" && "$2" == "$FAIL_USER" ]]; then
			echo "id: no such user: $2" >&2
			exit 1
		fi
		echo 1042
		exit 0
	fi
	exec /usr/bin/id "$@"
elif [[ "$arg1" == "-un" ]]; then
	if [[ "$2" == "fakeuser" || "$2" == "baduser" ]]; then
		echo "$2"
		exit 0
	fi
	exec /usr/bin/id "$@"
fi
exec /usr/bin/id "$@"
STUB
	chmod +x "$STUB_BIN/id"
}

write_systemctl_stub
write_id_stub

# Build a scratch checkout that looks enough like the repo for the installer:
# a git repo whose parent holds scripts/install-deploy-watch.sh and an
# executable scripts/deploy-watch.sh.
new_repo() {
	local repo remote
	repo="$(mktemp -d "$TMP/repo.XXXXXX")"
	mkdir -p "$repo/scripts"
	cp "$INSTALL_SRC" "$repo/scripts/install-deploy-watch.sh"
	cp "$WATCH_SRC" "$repo/scripts/deploy-watch.sh"
	chmod +x "$repo/scripts/install-deploy-watch.sh" "$repo/scripts/deploy-watch.sh"
	git -C "$repo" init -q -b main
	git -C "$repo" config user.email "install-test@example.com"
	git -C "$repo" config user.name "install test"
	echo one >"$repo/tracked.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm "initial"
	echo "$repo"
}

run_install() {
	local repo=$1 unit_dir=$2
	shift 2
	OUT="$TMP/output.$$"
	UNIT_DIR="$unit_dir"
	(
		cd "$repo" || exit 99
		STUB_SYSTEMCTL_DIR="$unit_dir" PATH="$STUB_BIN:$PATH" \
			SYSTEMD_UNIT_DIR="$unit_dir" \
			SYSTEMCTL="$STUB_BIN/systemctl" \
			"$@" \
			bash scripts/install-deploy-watch.sh --user fakeuser
	) >"$OUT" 2>&1
	STATUS=$?
}

assert_status() {
	local want=$1 desc=$2
	if [[ "$STATUS" == "$want" ]]; then pass; else fail "$desc: exit $STATUS, want $want"; fi
}

assert_out_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$OUT"; then pass; else fail "$desc: output lacks '$needle'"; fi
}

assert_unit_contains() {
	local desc=$1 unit=$2 needle=$3
	if grep -qF -- "$needle" "$unit"; then pass; else fail "$desc: unit lacks '$needle'"; fi
}

assert_unit_not_contains() {
	local desc=$1 unit=$2 needle=$3
	if [[ ! -f "$unit" ]]; then pass; return; fi
	if grep -qF -- "$needle" "$unit"; then fail "$desc: unit unexpectedly contains '$needle'"; else pass; fi
}

test_resolves_xdg_runtime_dir_for_non_1000_user() {
	local repo unit_dir service_unit
	repo=$(new_repo)
	unit_dir="$(mktemp -d "$TMP/units.XXXXXX")"
	: >"$unit_dir/systemctl-calls"
	run_install "$repo" "$unit_dir"
	assert_status 0 "installer succeeds for a non-1000 user"
	service_unit="$unit_dir/clanka-deploy.service"
	assert_unit_contains "unit sets XDG_RUNTIME_DIR from the real UID" \
		"$service_unit" "Environment=XDG_RUNTIME_DIR=/run/user/1042"
	assert_unit_not_contains "unit does not assume UID 1000" \
		"$service_unit" "XDG_RUNTIME_DIR=/run/user/1000"
}

test_assumes_nothing_when_uid_unresolvable() {
	local repo unit_dir service_unit
	repo=$(new_repo)
	unit_dir="$(mktemp -d "$TMP/units.XXXXXX")"
	: >"$unit_dir/systemctl-calls"
	OUT="$TMP/output.$$"
	(
		cd "$repo" || exit 99
		STUB_SYSTEMCTL_DIR="$unit_dir" PATH="$STUB_BIN:$PATH" \
			SYSTEMD_UNIT_DIR="$unit_dir" \
			SYSTEMCTL="$STUB_BIN/systemctl" \
			FAIL_USER=baduser \
			bash scripts/install-deploy-watch.sh --user baduser
	) >"$OUT" 2>&1
	STATUS=$?
	assert_status 1 "installer fails when the UID cannot be resolved"
	assert_out_contains "installer explains the failure" "no such user"
	assert_unit_not_contains "installer never assumes UID 1000" \
		"$unit_dir/clanka-deploy.service" "XDG_RUNTIME_DIR=/run/user/1000"
}

run_test() {
	local name=$1
	printf '== %s\n' "$name"
	"$name"
}

run_test test_resolves_xdg_runtime_dir_for_non_1000_user
run_test test_assumes_nothing_when_uid_unresolvable

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
