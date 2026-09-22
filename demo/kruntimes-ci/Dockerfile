ARG BASE_IMAGE=ghcr.io/kruntimes/bash-runtime:nightly
FROM ${BASE_IMAGE}

USER 0
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl gcc git libc6-dev make python3 python3-pip tar wget \
    && python3 -m pip install --break-system-packages --no-cache-dir uv \
    && rm -rf /var/lib/apt/lists/*
USER 65532
