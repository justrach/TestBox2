#!/usr/bin/env bash
# Build CubeSandbox runtime "snapshots" (templates) for common languages.
#
# Run on an installed, healthy CubeSandbox control node. Requires `docker`
# and `cubemastercli` (auto-detected under the standard install prefix).
#
# Usage:
#   ./build-snapshots.sh                 # build all runtimes
#   ./build-snapshots.sh py313t bun      # build a subset (template id or folder)
#
# Env:
#   CUBE_SNAPSHOT_BASE_IMAGE   base image (default ghcr.io/tencentcloud/cubesandbox-base:2026.16)
#   CUBE_SNAPSHOT_WRITABLE_LAYER  writable layer size (default 4Gi)
#   CUBEMASTERCLI              path to cubemastercli
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASE_IMAGE="${CUBE_SNAPSHOT_BASE_IMAGE:-ghcr.io/tencentcloud/cubesandbox-base:2026.16}"
WLS="${CUBE_SNAPSHOT_WRITABLE_LAYER:-4Gi}"

CUBEMASTERCLI="${CUBEMASTERCLI:-cubemastercli}"
if ! command -v "${CUBEMASTERCLI}" >/dev/null 2>&1; then
  CUBEMASTERCLI=/usr/local/services/cubetoolbox/CubeMaster/bin/cubemastercli
fi
command -v docker >/dev/null 2>&1 || { echo "ERROR: docker is required" >&2; exit 1; }
command -v "${CUBEMASTERCLI}" >/dev/null 2>&1 || { echo "ERROR: cubemastercli not found" >&2; exit 1; }

# folder:template-id
RUNTIMES=(
  "zig-0.16.0:zig016"
  "node-latest:node"
  "bun-latest:bun"
  "python-3.10:py310"
  "python-3.11:py311"
  "python-3.12:py312"
  "python-3.13:py313"
  "python-3.13t:py313t"
  "python-3.14:py314"
  "python-3.14t:py314t"
)

SELECT="$*"
selected() {
  [ -z "${SELECT}" ] && return 0
  case " ${SELECT} " in *" $1 "*|*" $2 "*) return 0 ;; *) return 1 ;; esac
}

ok=0; fail=0; FAILED=""
build_one() {
  local dir="$1" tid="$2" img="cube-snap/$2:latest" s=""
  echo "==> [$tid] docker build ($dir) from ${BASE_IMAGE}"
  if ! docker build --build-arg BASE_IMAGE="${BASE_IMAGE}" -t "${img}" "${HERE}/${dir}"; then
    echo "==> [$tid] BUILD FAILED"; fail=$((fail+1)); FAILED="${FAILED} ${tid}(build)"; return
  fi
  echo "==> [$tid] cubemastercli tpl create-from-image"
  # Delete any pre-existing template with this id first: create-from-image is a
  # no-op when the id already exists, which would silently keep a stale image.
  "${CUBEMASTERCLI}" tpl delete --template-id "${tid}" >/dev/null 2>&1 || true
  "${CUBEMASTERCLI}" tpl create-from-image \
    --image "${img}" --template-id "${tid}" \
    --writable-layer-size "${WLS}" \
    --expose-port 49983 --probe 49983 --probe-path /health >/dev/null 2>&1
  for _ in $(seq 1 80); do
    s="$("${CUBEMASTERCLI}" tpl list 2>/dev/null | awk -v t="${tid}" '$1==t{print $2}')"
    [ "${s}" = "READY" ] && break
    [ "${s}" = "FAILED" ] && break
    sleep 3
  done
  if [ "${s}" = "READY" ]; then
    echo "==> [$tid] READY"; ok=$((ok+1))
  else
    echo "==> [$tid] template status: ${s:-UNKNOWN}"; fail=$((fail+1)); FAILED="${FAILED} ${tid}(${s:-template})"
  fi
}

for entry in "${RUNTIMES[@]}"; do
  dir="${entry%%:*}"; tid="${entry##*:}"
  selected "${dir}" "${tid}" || continue
  build_one "${dir}" "${tid}"
done

echo
echo "=== templates ==="
"${CUBEMASTERCLI}" tpl list
echo
echo "=== summary: ${ok} ready, ${fail} failed${FAILED:+ -}${FAILED} ==="
[ "${fail}" -eq 0 ]
