#!/usr/bin/env bash

set -euo pipefail

CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:RELEASE.2025-04-22T22-12-26Z}"
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-kruntimes}"
MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-kruntimes-secret}"
MINIO_BUCKET="${MINIO_BUCKET:-kruntimes-artifacts-test}"
CONTAINER_NAME="kruntimes-minio-test-$$"

cleanup() {
  "${CONTAINER_TOOL}" rm -f "${CONTAINER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${CONTAINER_TOOL}" run --rm -d \
  --name "${CONTAINER_NAME}" \
  -p 127.0.0.1::9000 \
  -e "MINIO_ROOT_USER=${MINIO_ACCESS_KEY}" \
  -e "MINIO_ROOT_PASSWORD=${MINIO_SECRET_KEY}" \
  "${MINIO_IMAGE}" server /data >/dev/null

port="$("${CONTAINER_TOOL}" port "${CONTAINER_NAME}" 9000/tcp | awk -F: 'NR == 1 {print $NF}')"
endpoint="http://127.0.0.1:${port}"

for _ in $(seq 1 60); do
  if curl --fail --silent "${endpoint}/minio/health/ready" >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail --silent "${endpoint}/minio/health/ready" >/dev/null

AWS_ACCESS_KEY_ID="${MINIO_ACCESS_KEY}" \
AWS_SECRET_ACCESS_KEY="${MINIO_SECRET_KEY}" \
AWS_REGION="us-east-1" \
AWS_EC2_METADATA_DISABLED="true" \
KRUNTIMES_S3_ENDPOINT="${endpoint}" \
KRUNTIMES_S3_BUCKET="${MINIO_BUCKET}" \
go test -tags=integration ./internal/artifact/s3 -run '^TestMinIO' -v -count=1
