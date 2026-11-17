#!/usr/bin/env bash
# Black-box tests for scripts/doctor.sh.
#
# Tests each preflight check for ok, warn, and fail conditions, including
# non-1000 UID environments, disk space thresholds, Soloist key expiration,
# and secret redaction. Stubs docker, tailscale, df, stat, and id on PATH.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
DOCTOR_SRC="$REPO_ROOT/scripts/doctor.sh"
MAKE_SRC="$REPO_ROOT/Makefile"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/doctor-test.XXXXXX")"
STUB_BIN="$TMP/bin"
STUB_EMPTY_BIN="$TMP/empty_bin"
mkdir -p "$STUB_BIN" "$STUB_EMPTY_BIN"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
STATUS=0
OUT=""
SCENARIO=""

pass() { PASS=$((PASS + 1)); }

fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL: %s\n' "$*"
	if [[ -f "$OUT" ]]; then
		printf -- '--- output ---\n'
		cat "$OUT"
		printf -- '--------------\n'
	fi
}

assert_status() {
	local want=$1 desc=$2
	if [[ "$STATUS" == "$want" ]]; then
		pass
	else
		fail "$desc: exit $STATUS, want $want"
	fi
}

assert_nonzero() {
	local desc=$1
	if [[ "$STATUS" -ne 0 ]]; then
		pass
	else
		fail "$desc: exit $STATUS, expected non-zero"
	fi
}

assert_out_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$OUT"; then
		pass
	else
		fail "$desc: output lacks '$needle'"
	fi
}

assert_out_not_contains() {
	local desc=$1 needle=$2
	if ! grep -qF -- "$needle" "$OUT"; then
		pass
	else
		fail "$desc: output unexpectedly contains '$needle'"
	fi
}

# Create stub executables in STUB_BIN.
write_stubs() {
	# docker stub
	cat >"$STUB_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_DIR:?STUB_DIR required}"
if [[ -f "$dir/docker-missing" ]]; then
	exit 127
fi

cmd="${1:-}"
shift || true
case "$cmd" in
compose)
	if [[ -f "$dir/compose-missing" ]]; then
		exit 1
	fi
	sub="${1:-}"
	shift || true
	case "$sub" in
	version)
		if [[ -f "$dir/compose-v1" ]]; then
			echo "Docker Compose version v1.29.2"
		else
			echo "Docker Compose version v2.24.5"
		fi
		;;
	*) echo "stub docker compose $sub" ;;
	esac
	;;
info)
	if [[ -f "$dir/docker-unreachable" ]]; then
		echo "Cannot connect to the Docker daemon" >&2
		exit 1
	fi
	if [[ "${1:-}" == "--format" && "${2:-}" == "{{.DockerRootDir}}" ]]; then
		if [[ -f "$dir/docker-root" ]]; then
			cat "$dir/docker-root"
		else
			echo "/var/lib/docker"
		fi
		exit 0
	fi
	echo "Docker info ok"
	;;
image)
	sub="${1:-}"
	shift || true
	case "$sub" in
	ls)
		if [[ -f "$dir/images" ]]; then
			cat "$dir/images"
		else
			cat <<'IMAGES'
clanka/assistant|main|500MB|img1
clanka/assistant|0123456|500MB|img2
clanka/tts|feat|2GB|img3
IMAGES
		fi
		;;
	*) echo "stub docker image $sub" ;;
	esac
	;;
*) echo "stub docker $cmd" ;;
esac
EOF
	chmod +x "$STUB_BIN/docker"

	# tailscale stub
	cat >"$STUB_BIN/tailscale" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_DIR:?STUB_DIR required}"
if [[ -f "$dir/tailscale-missing" ]]; then
	exit 127
fi

cmd="${1:-}"
shift || true
case "$cmd" in
version)
	echo "1.0.0"
	;;
serve)
	sub="${1:-}"
	shift || true
	case "$sub" in
	status)
		if [[ -f "$dir/tailscale-status" ]]; then
			cat "$dir/tailscale-status"
		else
			echo "https://clanka.example.ts.net (tailnet only)"
			echo "|-- / proxy http://127.0.0.1:8082"
		fi
		;;
	*) echo "stub tailscale serve $sub" ;;
	esac
	;;
*) echo "stub tailscale $cmd" ;;
esac
EOF
	chmod +x "$STUB_BIN/tailscale"

	# df stub
	cat >"$STUB_BIN/df" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_DIR:-}"
if [[ -n "$dir" && -f "$dir/df-kb" ]]; then
	avail="$(cat "$dir/df-kb")"
else
	# Default 20 GB (in 1024-byte blocks)
	avail=20971520
fi
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/stub 1000000000 1000000 %s 1%% /\n' "$avail"
EOF
	chmod +x "$STUB_BIN/df"

	# stat stub
	cat >"$STUB_BIN/stat" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_DIR:-}"
if [[ -n "$dir" && -f "$dir/stat-override" && "${1:-}" == "-c" && "${2:-}" == "%u:%g" ]]; then
	cat "$dir/stat-override"
	exit 0
fi
/usr/bin/stat "$@"
EOF
	chmod +x "$STUB_BIN/stat"

	# id stub
	cat >"$STUB_BIN/id" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_DIR:-}"
if [[ -n "$dir" && -f "$dir/id-u" && "${1:-}" == "-u" ]]; then
	cat "$dir/id-u"
	exit 0
fi
if [[ -n "$dir" && -f "$dir/id-un" && "${1:-}" == "-un" ]]; then
	cat "$dir/id-un"
	exit 0
fi
if [[ -n "$dir" && -f "$dir/id-g" && "${1:-}" == "-g" ]]; then
	cat "$dir/id-g"
	exit 0
fi
/usr/bin/id "$@"
EOF
	chmod +x "$STUB_BIN/id"
}

# Initialize fresh scenario directory.
new_scenario() {
	SCENARIO="$(mktemp -d "$TMP/scenario.XXXXXX")"
	mkdir -p "$SCENARIO"
}

# Create a clean git repository containing doctor.sh, Makefile, and standard layout.
new_repo() {
	local repo
	repo="$(mktemp -d "$TMP/repo.XXXXXX")"
	mkdir -p "$repo/scripts"
	cp "$DOCTOR_SRC" "$repo/scripts/doctor.sh"
	cp "$MAKE_SRC" "$repo/Makefile"
	cp "$REPO_ROOT/scripts/prune-images.sh" "$repo/scripts/prune-images.sh"
	chmod +x "$repo/scripts/doctor.sh" "$repo/scripts/prune-images.sh"

	git -C "$repo" init -q -b main
	git -C "$repo" config user.email "doctor-test@example.com"
	git -C "$repo" config user.name "doctor test"
	echo ".env" >"$repo/.gitignore"
	echo "content" >"$repo/tracked.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm "initial commit"
	echo "$repo"
}

# Set up a fully valid environment fixture where all 10 checks pass.
setup_passing_fixture() {
	local repo=$1

	# .env file with owner
	cat >"$repo/.env" <<'EOF'
ASSISTANT_UID=1000
ASSISTANT_GID=1000
ASSISTANT_DASHBOARD_OWNER=operator@example.ts.net
EOF

	# Pulse socket and cookie
	local user_runtime="$SCENARIO/run_user"
	mkdir -p "$user_runtime/pulse"
	touch "$user_runtime/pulse/native"

	local home_dir="$SCENARIO/home"
	mkdir -p "$home_dir/.config/pulse"
	echo "dummycookie" >"$home_dir/.config/pulse/cookie"
	chmod 600 "$home_dir/.config/pulse/cookie"

	# exports directory
	mkdir -p "$repo/exports"
	chmod 755 "$repo/exports"

	# D-Bus socket
	local dbus_socket="$SCENARIO/system_bus_socket"
	touch "$dbus_socket"

	# Soloist private dir (profile off by default)
	mkdir -p "$SCENARIO/soloist"
}

# Execute scripts/doctor.sh within the specified repository and environment.
run_doctor() {
	local repo=$1
	shift
	OUT="$TMP/output.$$"
	(
		cd "$repo" || exit 99
		STUB_DIR="$SCENARIO" \
		PATH="$STUB_BIN:$PATH" \
		env "$@" bash scripts/doctor.sh
	) >"$OUT" 2>&1
	STATUS=$?
}

write_stubs

# Check fails when docker is not installed.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
touch "$SCENARIO/docker-missing"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 1: missing docker exits non-zero"
assert_out_contains "check 1: reports docker missing" "1. docker + compose v2: fail: docker is not installed"
assert_out_contains "check 1: reports fix command" "fix: install Docker"

# Check fails when docker compose plugin is missing.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
touch "$SCENARIO/compose-missing"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 1: missing compose exits non-zero"
assert_out_contains "check 1: reports compose plugin missing" "1. docker + compose v2: fail: docker compose plugin is not installed"

# Check fails when docker compose is v1.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
touch "$SCENARIO/compose-v1"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 1: compose v1 exits non-zero"
assert_out_contains "check 1: reports compose v1 rejected" "1. docker + compose v2: fail: docker compose is v1"

# Check fails when docker daemon is unreachable.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
touch "$SCENARIO/docker-unreachable"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 1: daemon unreachable exits non-zero"
assert_out_contains "check 1: reports daemon unreachable" "1. docker + compose v2: fail: docker daemon is unreachable"

# Check fails when .env is missing.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
rm -f "$REPO/.env"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 2: missing .env exits non-zero"
assert_out_contains "check 2: reports .env missing" "2. .env configuration: fail: .env file not found"

# Check warns when ASSISTANT_DASHBOARD_OWNER is empty.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
cat >"$REPO/.env" <<'EOF'
ASSISTANT_UID=1000
ASSISTANT_GID=1000
ASSISTANT_DASHBOARD_OWNER=
EOF
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 2: empty owner is only a warning"
assert_out_contains "check 2: reports empty owner warning" "2. .env configuration: warn: ASSISTANT_DASHBOARD_OWNER is empty"

# Check passes when owner is configured, and never prints secret values.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
SECRET_OWNER="super_secret_owner_12345@domain.example"
cat >"$REPO/.env" <<EOF
ASSISTANT_UID=1000
ASSISTANT_GID=1000
ASSISTANT_DASHBOARD_OWNER=$SECRET_OWNER
EOF
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 2: valid owner passes"
assert_out_contains "check 2: ok line" "2. .env configuration: ok"
assert_out_not_contains "check 2: redacts owner value" "$SECRET_OWNER"

# Check fails when Pulse socket is missing.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
rm -f "$RUN_USER/pulse/native"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 3: missing pulse socket exits non-zero"
assert_out_contains "check 3: reports missing socket" "3. pulse socket and cookie: fail: Pulse native socket not found"
assert_out_contains "check 3: explains assistant deaf" "the assistant is deaf"

# Check fails on UID/GID stat mismatch and prints exact mismatch.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
echo "1001:1001" >"$SCENARIO/stat-override"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 3: stat mismatch exits non-zero"
assert_out_contains "check 3: prints exact stat mismatch" "stat mismatch: $RUN_USER/pulse/native is owned by 1001:1001, expected 1000:1000"

# Check passes with non-1000 UID when configuration matches.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
cat >"$REPO/.env" <<'EOF'
ASSISTANT_UID=1005
ASSISTANT_GID=1005
ASSISTANT_DASHBOARD_OWNER=user@example.ts.net
EOF
echo "1005" >"$SCENARIO/id-u"
echo "1005" >"$SCENARIO/id-g"
echo "1005:1005" >"$SCENARIO/stat-override"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 3: non-1000 UID ok when matching"
assert_out_contains "check 3: non-1000 UID ok" "3. pulse socket and cookie: ok"

# Check fails when Pulse cookie is unreadable.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
rm -f "$SCENARIO/home/.config/pulse/cookie"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 3: unreadable cookie exits non-zero"
assert_out_contains "check 3: reports unreadable cookie" "3. pulse socket and cookie: fail: Pulse cookie not readable"

# Check warns when runtime directory is missing and advises linger.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
rm -rf "$RUN_USER"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 3 fails due to missing socket, check 4 warns"
assert_out_contains "check 4: warns on missing runtime dir" "4. user runtime directory: warn: $RUN_USER does not exist"
assert_out_contains "check 4: advises loginctl enable-linger" "sudo loginctl enable-linger"

# Check fails when exports directory is missing.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
rm -rf "$REPO/exports"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 5: missing exports exits non-zero"
assert_out_contains "check 5: reports exports missing" "5. exports directory: fail: $REPO/exports does not exist (bundle export is broken)"

# Check fails when exports is unwritable.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
chmod 555 "$REPO/exports"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 5: unwritable exports exits non-zero"
assert_out_contains "check 5: reports unwritable exports" "is not writable by deploy user"

# Check fails when disk space is below 5 GB.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
# Four GB available in KB
echo "4194304" >"$SCENARIO/df-kb"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 6: < 5 GB exits non-zero"
assert_out_contains "check 6: reports disk space failure" "6. docker root free space: fail: 4 GB free on /var/lib/docker"
assert_out_contains "check 6: quantifies image sets" "3 clanka/* image set(s)"
assert_out_contains "check 6: points to prune-images" "reclaim disk space with 'make prune-images'"

# Check warns when disk space is between 5 GB and 15 GB.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
# Ten GB available in KB
echo "10485760" >"$SCENARIO/df-kb"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 6: < 15 GB warns but exits 0 when no failures"
assert_out_contains "check 6: reports warning" "6. docker root free space: warn: 10 GB free on /var/lib/docker"
assert_out_contains "check 6: points to prune-images" "reclaim disk space with 'make prune-images'"

# Check recommends manual docker image rmi when prune-images target is missing.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
rm -f "$REPO/scripts/prune-images.sh"
echo "4194304" >"$SCENARIO/df-kb"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 6: failure with missing prune script"
assert_out_contains "check 6: advises manual rmi" "docker image rmi"

# Check passes with 'not configured (profile off)' when key is absent.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	SOLOIST_DIR="$SCENARIO/soloist" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 7: missing key is profile off"
assert_out_contains "check 7: profile off" "7. soloist receiver: ok (not configured (profile off))"

# Check warns when key is present but build is expired.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
mkdir -p "$SCENARIO/soloist/status"
SECRET_KEY="secret_key_material_abcxyz"
echo "$SECRET_KEY" >"$SCENARIO/soloist/api-key"
echo "expires_days=-2" >"$SCENARIO/soloist/status/soloist.status"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	SOLOIST_DIR="$SCENARIO/soloist" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 7: expired build warns, does not fail"
assert_out_contains "check 7: warns expired" "7. soloist receiver: warn: key present but build is expired (-2 days remaining)"
assert_out_contains "check 7: recommends update" "make soloist-update"
assert_out_not_contains "check 7: redacts secret key" "$SECRET_KEY"

# Check warns when key is present and expires_days < 7.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
mkdir -p "$SCENARIO/soloist/status"
echo "key_data" >"$SCENARIO/soloist/api-key"
echo "expires_days=3" >"$SCENARIO/soloist/status/soloist.status"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	SOLOIST_DIR="$SCENARIO/soloist" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 7: expires in 3 days warns"
assert_out_contains "check 7: warns expiry soon" "7. soloist receiver: warn: key present but build expires in 3 day(s) (< 7 days)"

# Check passes when key is present and expires_days >= 7.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
mkdir -p "$SCENARIO/soloist/status"
echo "key_data" >"$SCENARIO/soloist/api-key"
echo "expires_days=85" >"$SCENARIO/soloist/status/soloist.status"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	SOLOIST_DIR="$SCENARIO/soloist" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 7: unexpired key passes"
assert_out_contains "check 7: ok with days" "7. soloist receiver: ok (key present, build expires in 85 days)"

# Check warns when D-Bus system bus socket is missing.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/missing_dbus_socket"
assert_status 0 "check 8: missing dbus warns, does not fail"
assert_out_contains "check 8: warns on missing dbus socket" "8. d-bus system bus socket: warn: $SCENARIO/missing_dbus_socket not present (BLE lights disabled)"

# Check fails when git status is dirty.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
echo "dirty edit" >>"$REPO/tracked.txt"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_nonzero "check 9: dirty tree exits non-zero"
assert_out_contains "check 9: reports uncommitted changes" "9. git status and branch: fail: working tree has uncommitted changes"

# Check warns when on a non-main branch.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
git -C "$REPO" checkout -q -b feature-test
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 9: feature branch warns, does not fail"
assert_out_contains "check 9: warns on feature branch" "9. git status and branch: warn: on branch 'feature-test', expected 'main'"

# Check passes when tailscale is not installed.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
touch "$SCENARIO/tailscale-missing"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 10: missing tailscale is ok"
assert_out_contains "check 10: reports tailscale not installed" "10. tailscale serve mapping: ok (tailscale not installed)"

# Check warns when port 8082 is not mapped in tailscale serve.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
echo "No serve mappings active" >"$SCENARIO/tailscale-status"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "check 10: missing mapping warns"
assert_out_contains "check 10: warns on missing mapping" "10. tailscale serve mapping: warn: no tailscale serve mapping for port 8082 found"
assert_out_contains "check 10: suggests serve fix" "fix: tailscale serve --bg 8082"

# Clean dev box passes all checks and exits 0.
new_scenario
REPO="$(new_repo)"
setup_passing_fixture "$REPO"
RUN_USER="$SCENARIO/run_user"
run_doctor "$REPO" \
	XDG_RUNTIME_DIR="$RUN_USER" \
	HOME="$SCENARIO/home" \
	DOCTOR_DBUS_SOCKET="$SCENARIO/system_bus_socket"
assert_status 0 "clean dev box exits 0"
assert_out_contains "summary: ok" "doctor: OK (all checks passed)"
assert_out_contains "check 1 ok" "1. docker + compose v2: ok"
assert_out_contains "check 2 ok" "2. .env configuration: ok"
assert_out_contains "check 3 ok" "3. pulse socket and cookie: ok"
assert_out_contains "check 4 ok" "4. user runtime directory: ok"
assert_out_contains "check 5 ok" "5. exports directory: ok"
assert_out_contains "check 6 ok" "6. docker root free space: ok"
assert_out_contains "check 7 ok" "7. soloist receiver: ok"
assert_out_contains "check 8 ok" "8. d-bus system bus socket: ok"
assert_out_contains "check 9 ok" "9. git status and branch: ok"
assert_out_contains "check 10 ok" "10. tailscale serve mapping: ok"

if ((FAIL > 0)); then
	printf '\n%d test(s) failed, %d passed\n' "$FAIL" "$PASS" >&2
	exit 1
fi
printf '\nAll %d tests passed\n' "$PASS"
exit 0
