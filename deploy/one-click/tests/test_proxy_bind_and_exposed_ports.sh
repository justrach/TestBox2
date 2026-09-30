#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright (C) 2026 Tencent. All rights reserved.
#
# Two deployment options that replace hand-edits after an install:
#   CUBEMASTER_EXPOSED_PORTS  extra ports in CubeMaster's exposed_port_list (one_click_patch_exposed_ports)
#   CUBE_PROXY_BIND_ADDR      bind the proxy's HTTP/HTTPS/gRPC listeners to one address
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ONE_CLICK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
ROOT_DIR="$(cd "${ONE_CLICK_DIR}/../.." && pwd)"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

: > "${TMP_DIR}/empty-runtime.env"
export ONE_CLICK_RUNTIME_ENV_FILE="${TMP_DIR}/empty-runtime.env"
export ONE_CLICK_RUNTIME_DIR="${TMP_DIR}/run"
export ONE_CLICK_LOG_DIR="${TMP_DIR}/log"

# shellcheck source=../lib/common.sh
source "${ONE_CLICK_DIR}/lib/common.sh"

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}
assert_contains() { grep -Fq -- "$2" "$1" || fail "expected $1 to contain: $2"; }
assert_not_contains() { ! grep -Fq -- "$2" "$1" || fail "expected $1 NOT to contain: $2"; }

sample_conf() {
  cat > "$1" <<'EOF'
cube_master:
  enable_exposed_port: true
  exposed_port_list:
    - "80"
  disable_redis_proxy_port: true
EOF
}

test_exposed_ports_are_added_once_and_the_default_stays() {
  local conf="${TMP_DIR}/master.yaml"
  sample_conf "${conf}"
  one_click_patch_exposed_ports "${conf}" "49983, 8787"
  assert_contains "${conf}" '    - "49983"'
  assert_contains "${conf}" '    - "8787"'
  assert_contains "${conf}" '    - "80"'
  assert_contains "${conf}" 'disable_redis_proxy_port: true'
  one_click_patch_exposed_ports "${conf}" "49983,8787,80"
  [[ "$(grep -c '"49983"' "${conf}")" == 1 ]] || fail "49983 was listed twice after a second run"
  [[ "$(grep -c '"80"' "${conf}")" == 1 ]] || fail "80 was listed twice"
}

test_empty_input_changes_nothing() {
  local conf="${TMP_DIR}/empty.yaml"
  sample_conf "${conf}"
  cp "${conf}" "${conf}.orig"
  one_click_patch_exposed_ports "${conf}" ""
  one_click_patch_exposed_ports "${conf}" "  "
  cmp -s "${conf}" "${conf}.orig" || fail "empty CUBEMASTER_EXPOSED_PORTS modified the config"
}

test_bad_ports_are_refused_and_leave_the_file_alone() {
  local conf="${TMP_DIR}/bad.yaml" bad
  sample_conf "${conf}"
  cp "${conf}" "${conf}.orig"
  for bad in "abc" "0" "65536" "80;rm" '80"'; do
    if (one_click_patch_exposed_ports "${conf}" "${bad}") >/dev/null 2>&1; then
      fail "port '${bad}' was accepted"
    fi
  done
  cmp -s "${conf}" "${conf}.orig" || fail "a refused port still changed the config"
}

test_proxy_template_binds_the_three_listeners_to_the_chosen_address() {
  local src="${ROOT_DIR}/CubeProxy/nginx.conf" rendered="${TMP_DIR}/nginx.rendered" tmpl="${TMP_DIR}/nginx.tmpl" bind host
  [[ -f "${src}" ]] || { fail "missing ${src}"; return; }
  # The same rules the release bundle applies (build-release-bundle.sh).
  sed \
    -e 's|^\(\s*listen \)8081\( reuseport;\)|\1__CUBE_PROXY_LISTEN_HOST____CUBE_PROXY_HTTP_PORT__\2|' \
    -e 's|^\(\s*listen \)8080\( ssl reuseport;\)|\1__CUBE_PROXY_LISTEN_HOST____CUBE_PROXY_HTTPS_PORT__\2|' \
    -e 's|^\(\s*listen \)9090\( http2 reuseport;\)|\1__CUBE_PROXY_LISTEN_HOST____CUBE_PROXY_GRPC_PORT__\2|' \
    "${src}" > "${tmpl}"
  for bind in "" "127.0.0.1"; do
    host=""
    [[ -z "${bind}" ]] || host="${bind}:"
    sed -e "s/__CUBE_PROXY_LISTEN_HOST__/${host}/g" -e 's/__CUBE_PROXY_HTTPS_PORT__/443/g' \
        -e 's/__CUBE_PROXY_HTTP_PORT__/80/g' -e 's/__CUBE_PROXY_GRPC_PORT__/9090/g' "${tmpl}" > "${rendered}"
    assert_contains "${rendered}" "listen ${host}80 reuseport;"
    assert_contains "${rendered}" "listen ${host}443 ssl reuseport;"
    assert_contains "${rendered}" "listen ${host}9090 http2 reuseport;"
    assert_not_contains "${rendered}" "__CUBE_PROXY_LISTEN_HOST__"
  done
}

test_the_proxy_script_validates_the_address_and_scopes_its_port_check() {
  local up="${ONE_CLICK_DIR}/scripts/one-click/up-cube-proxy.sh"
  grep -Fq 'CUBE_PROXY_BIND_ADDR must be an IPv4 address' "${up}" || fail "no address validation in up-cube-proxy.sh"
  grep -Fq '__CUBE_PROXY_LISTEN_HOST__' "${up}" || fail "up-cube-proxy.sh does not render the listen host"
  grep -Fq 'and src ${CUBE_PROXY_BIND_ADDR}' "${up}" || fail "the port pre-check ignores the bind address"
  grep -Fq 'CUBEMASTER_EXPOSED_PORTS' "${ONE_CLICK_DIR}/install.sh" || fail "install.sh does not apply CUBEMASTER_EXPOSED_PORTS"
  grep -Fq 'CUBE_PROXY_BIND_ADDR' "${ONE_CLICK_DIR}/env.example" || fail "env.example does not document CUBE_PROXY_BIND_ADDR"
}

test_exposed_ports_are_added_once_and_the_default_stays
test_empty_input_changes_nothing
test_bad_ports_are_refused_and_leave_the_file_alone
test_proxy_template_binds_the_three_listeners_to_the_chosen_address
test_the_proxy_script_validates_the_address_and_scopes_its_port_check

if [[ "${failures}" -gt 0 ]]; then
  echo "${failures} proxy-bind / exposed-ports test(s) failed" >&2
  exit 1
fi
echo "proxy bind and exposed ports tests OK"
