#!/usr/bin/env bash

set -euo pipefail

release_tag="${1:-${RELEASE_TAG:-}}"
if [[ -z "${release_tag}" ]]; then
	echo "usage: $0 vX.Y.Z" >&2
	exit 2
fi

version="${release_tag#v}"
semver_re='[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?'
if [[ ! "${release_tag}" =~ ^v${semver_re}$ ]]; then
	echo "release tag ${release_tag} must be v-prefixed SemVer" >&2
	exit 1
fi

for chart in charts/kruntimes charts/kruntimes-runtimes; do
	chart_version="$(awk '$1 == "version:" { print $2 }' "${chart}/Chart.yaml")"
	app_version="$(awk '$1 == "appVersion:" { gsub(/"/, "", $2); print $2 }' "${chart}/Chart.yaml")"
	if [[ ! "${chart_version}" =~ ^${semver_re}$ ]]; then
		echo "${chart} version ${chart_version} is not valid SemVer" >&2
		exit 1
	fi
	if [[ "${app_version}" != "${version}" ]]; then
		echo "${chart} appVersion ${app_version} does not match release ${version}" >&2
		exit 1
	fi
done
