#!/usr/bin/env bash

# The Makefile installs protoc and Helm by downloading per-platform release
# assets. A wrong mapping only fails on the platform it points away from, so
# check every supported host mapping from any runner, including Linux CI.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

failures=0

read_field() {
	local output="$1" key="$2"
	awk -F= -v key="${key}" '$1 == key { print $2 }' <<<"${output}"
}

check() {
	local host_os="$1" host_arch="$2" want_protoc="$3" want_helm="$4"
	local output protoc_arch helm_platform
	output="$(make --no-print-directory -s tool-platform "HOST_OS=${host_os}" "HOST_ARCH=${host_arch}")"
	protoc_arch="$(read_field "${output}" protoc_arch)"
	helm_platform="$(read_field "${output}" helm_platform)"
	if [[ "${protoc_arch}" != "${want_protoc}" || "${helm_platform}" != "${want_helm}" ]]; then
		echo "FAIL ${host_os}/${host_arch}: protoc_arch=${protoc_arch} (want ${want_protoc}), helm_platform=${helm_platform} (want ${want_helm})" >&2
		failures=$((failures + 1))
		return
	fi
	echo "ok ${host_os}/${host_arch}: protoc_arch=${protoc_arch}, helm_platform=${helm_platform}"
}

# protoc ships osx-aarch_64, osx-x86_64, linux-x86_64, and linux-aarch_64;
# Helm ships darwin-arm64, darwin-amd64, linux-amd64, and linux-arm64.
check Darwin arm64 osx-aarch_64 darwin-arm64
check Darwin x86_64 osx-x86_64 darwin-amd64
check Linux x86_64 linux-x86_64 linux-amd64
check Linux aarch64 linux-aarch_64 linux-arm64
check Linux arm64 linux-aarch_64 linux-arm64

# The runner's own host must resolve to a non-empty asset pair too.
host_output="$(make --no-print-directory -s tool-platform)"
echo "ok host: ${host_output//$'\n'/, }"
for key in protoc_arch helm_platform; do
	if [[ -z "$(read_field "${host_output}" "${key}")" ]]; then
		echo "FAIL host: ${key} is empty" >&2
		failures=$((failures + 1))
	fi
done

# The mapping is a default: callers may still point at another platform's asset.
override_output="$(PROTOC_ARCH=linux-aarch_64 HELM_PLATFORM=linux-arm64 make --no-print-directory -s tool-platform)"
if [[ "$(read_field "${override_output}" protoc_arch)" != "linux-aarch_64" \
	|| "$(read_field "${override_output}" helm_platform)" != "linux-arm64" ]]; then
	echo "FAIL override: PROTOC_ARCH/HELM_PLATFORM from the environment were not honored" >&2
	failures=$((failures + 1))
fi

if [[ "${failures}" -ne 0 ]]; then
	echo "${failures} platform mapping check(s) failed" >&2
	exit 1
fi
