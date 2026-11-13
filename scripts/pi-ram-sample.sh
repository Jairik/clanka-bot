#!/usr/bin/env bash
# Per-container memory on a Pi whose kernel boots with cgroup_disable=memory.
#
# Why this exists (#69): the Pi 5 kernel command line carries
# `cgroup_disable=memory`, so cgroup v2 exposes no memory controller and both
# `docker stats` and `memory.current` report 0B. Re-enabling it needs a reboot
# and a boot-config edit, which the measurement run was not allowed to do.
#
# The workaround measures processes instead of cgroups:
#
#   1. Ask Docker for the running containers and their full ids.
#   2. Map every host PID to its container by reading /proc/<pid>/cgroup, so a
#      PID that Docker started is attributed even when it is not `docker top`'s
#      container-namespace numbering.
#   3. Sum the kernel's own per-process rollup of PSS
#      (/proc/<pid>/smaps_rollup). PSS charges every shared page to all of its
#      mappers in proportion to how much of it each maps, so summing PSS across
#      a container does not double-count shared pages the way RSS does. That is
#      the number to compare against `free`; summing RSS would overshoot.
#   4. Record /proc/meminfo system totals in the same sample so the container
#      sum can be reconciled against MemAvailable.
#
# Usage (root; the script re-executes itself through sudo when it is not):
#   scripts/pi-ram-sample.sh                                  # one sample
#   scripts/pi-ram-sample.sh --interval 10 --duration 600     # 10 min idle
#   scripts/pi-ram-sample.sh --interval 2 --duration 900 \
#       --project clanka-bot --out /tmp/load.jsonl            # under load
#
# Output: one JSON object per line on stdout, appended to --out when given.
# --project restricts the per-container table to one Compose project; host and
# other-container PIDs still appear in pss_all_kb, so nothing is hidden.

set -euo pipefail

usage() {
  sed -n '18,31p' "$0" | sed 's/^# \{0,1\}//'
}

if [ "$(id -u)" -ne 0 ]; then
  self=$(readlink -f -- "$0")
  exec sudo -n -- "$self" "$@"
fi

interval=10
duration=0
out=""
project=""

while [ $# -gt 0 ]; do
  case "$1" in
    --interval) interval=${2:?--interval needs a value}; shift 2 ;;
    --duration) duration=${2:?--duration needs a value}; shift 2 ;;
    --out) out=${2:?--out needs a value}; shift 2 ;;
    --project) project=${2:?--project needs a value}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "pi-ram-sample: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

case "$interval" in ''|*[!0-9]*|0) echo "pi-ram-sample: --interval must be a positive integer" >&2; exit 2 ;; esac
case "$duration" in ''|*[!0-9]*) echo "pi-ram-sample: --duration must be a non-negative integer" >&2; exit 2 ;; esac

# One sample. Emits a single JSON line.
sample() {
  local ts epoch
  # UTC, independent of the host's timezone: %()T formats in local time, so
  # TZ=UTC0 makes the trailing Z truthful. The epoch field is the authority.
  TZ=UTC0 printf -v ts '%(%Y-%m-%dT%H:%M:%SZ)T' -1
  epoch=$(date +%s)

  local mem_total mem_free mem_available swap_total swap_free cached buffers sreclaim load1
  read -r mem_total mem_free mem_available swap_total swap_free cached buffers sreclaim < <(
    awk '
      $1 == "MemTotal:"     { t = $2 }
      $1 == "MemFree:"      { f = $2 }
      $1 == "MemAvailable:" { a = $2 }
      $1 == "SwapTotal:"    { st = $2 }
      $1 == "SwapFree:"     { sf = $2 }
      $1 == "Cached:"       { c = $2 }
      $1 == "Buffers:"      { b = $2 }
      $1 == "SReclaimable:" { s = $2 }
      END { print t, f, a, st, sf, c, b, s }
    ' /proc/meminfo
  )
  # `free`'s used column, computed the way free(1) does: everything that is
  # neither free nor reclaimable cache/slab.
  local used=$(( ${mem_total:-0} - ${mem_free:-0} - ${buffers:-0} - ${cached:-0} - ${sreclaim:-0} ))
  read -r load1 _ < /proc/loadavg

  # Container id -> name for the containers this sample reports on. The
  # project label is read with {{.Label}} and compared here instead of using
  # `docker ps --filter label=...`: images built by Compose carry
  # com.docker.compose.project as an image label, so a plain `docker run` from
  # such an image (the load client) inherits it and a label filter cannot tell
  # it apart from a real service. clanka.ram.exclude marks a container that
  # must never be counted, whatever labels it inherited.
  local -A name_of=()
  local -A wanted=()
  local line name id container_project exclude
  while IFS=$'\t' read -r name id container_project exclude; do
    [ -n "$id" ] || continue
    [ -z "$exclude" ] || continue
    if [ -n "$project" ] && [ "$container_project" != "$project" ]; then
      continue
    fi
    name_of["$id"]=$name
    wanted["$id"]=1
  done < <(docker ps --no-trunc --format \
    '{{.Names}}\t{{.ID}}\t{{.Label "com.docker.compose.project"}}\t{{.Label "clanka.ram.exclude"}}')

  # pid -> cgroup, and the smaps_rollup files that can be read right now.
  local -A cgroup_of=()
  local files=()
  local pid file cgroup
  for pid in /proc/[0-9]*; do
    pid=${pid#/proc/}
    file=/proc/$pid/smaps_rollup
    [ -r "$file" ] || continue
    cgroup=""
    read -r cgroup < /proc/$pid/cgroup || cgroup=""
    cgroup_of["$file"]=$cgroup
    files+=("$file")
  done

  # One awk for the whole sample. getline is used instead of passing the files
  # as arguments because mawk aborts the whole run when a PID disappears
  # between the listing and the read, which is normal at a 2 s cadence. The
  # manifest pairs every readable smaps_rollup with the container id parsed
  # from its cgroup, or "-" for host and other-container processes.
  local manifest totals
  manifest=$(mktemp)
  : >"$manifest"
  for file in "${files[@]}"; do
    cgroup=${cgroup_of[$file]:-}
    id="-"
    if [ -n "$cgroup" ]; then
      id=${cgroup##*docker-}
      if [ "$id" = "$cgroup" ]; then
        id="-"
      else
        id=${id%.scope}
      fi
    fi
    printf '%s %s\n' "$file" "$id" >>"$manifest"
  done

  # Output: "<id> <pss_kb> <rss_kb> <private_kb> <pids>" per id, plus "all".
  totals=$(awk -v manifest="$manifest" '
    BEGIN {
      while ((getline row < manifest) > 0) {
        n = split(row, parts, " ")
        path = parts[1]; id = parts[2]
        p = 0; r = 0; v = 0
        while ((getline line < path) > 0) {
          if (line ~ /^Pss:/)      { split(line, f, /[[:space:]]+/); p += f[2] }
          else if (line ~ /^Rss:/) { split(line, f, /[[:space:]]+/); r += f[2] }
          else if (line ~ /^Private_/) { split(line, f, /[[:space:]]+/); v += f[2] }
        }
        close(path)
        pss[id] += p; rss[id] += r; priv[id] += v; pids[id] += 1
        all += p
      }
      for (id in pss) printf "%s %d %d %d %d\n", id, pss[id], rss[id], priv[id], pids[id]
      printf "all %d 0 0 0\n", all
    }
  ' /dev/null)
  rm -f "$manifest"

  local -A c_pss=() c_rss=() c_priv=() c_pids=()
  local pss_all=0
  local cid pss rss priv npids
  while read -r cid pss rss priv npids; do
    [ -n "$cid" ] || continue
    if [ "$cid" = "all" ]; then
      pss_all=$pss
      continue
    fi
    [ -n "${wanted[$cid]:-}" ] || continue
    c_pss["$cid"]=$pss
    c_rss["$cid"]=$rss
    c_priv["$cid"]=$priv
    c_pids["$cid"]=$npids
  done <<<"$totals"

  # Emit. All memory values are KiB, exactly as the kernel reports them.
  local body="" first=1
  for id in $(printf '%s\n' "${!wanted[@]}" | sort); do
    [ -n "${c_pids[$id]:-}" ] || continue   # container between samples
    [ $first -eq 1 ] || body+=","
    first=0
    body+=$(printf '{"name":"%s","pss_kb":%d,"rss_kb":%d,"private_kb":%d,"pids":%d}' \
      "${name_of[$id]}" \
      "${c_pss[$id]:-0}" "${c_rss[$id]:-0}" "${c_priv[$id]:-0}" "${c_pids[$id]:-0}")
  done

  printf '{"ts":"%s","epoch":%s,"mem_total_kb":%s,"mem_free_kb":%s,"mem_available_kb":%s,"used_kb":%d,"swap_total_kb":%s,"swap_free_kb":%s,"cached_kb":%s,"buffers_kb":%s,"sreclaimable_kb":%s,"pss_all_kb":%d,"load1":"%s","containers":[%s]}\n' \
    "$ts" "$epoch" \
    "${mem_total:-0}" "${mem_free:-0}" "${mem_available:-0}" "$used" \
    "${swap_total:-0}" "${swap_free:-0}" \
    "${cached:-0}" "${buffers:-0}" "${sreclaim:-0}" \
    "$pss_all" "$load1" "$body"
}

emit() {
  if [ -n "$out" ]; then
    tee -a "$out"
  else
    cat
  fi
}

if [ "$duration" -eq 0 ]; then
  sample | emit
  exit 0
fi

end=$((SECONDS + duration))
while :; do
  sample | emit
  [ $SECONDS -ge $end ] && break
  remaining=$((end - SECONDS))
  [ "$remaining" -gt 0 ] || break
  [ "$remaining" -lt "$interval" ] && sleep "$remaining" || sleep "$interval"
done
