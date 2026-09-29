# syntax=docker/dockerfile:1.7

FROM node:24.13.1 AS frontend
WORKDIR /app
COPY console/frontend/package.json console/frontend/package-lock.json console/frontend/
RUN cd console/frontend && npm ci
COPY console/frontend console/frontend
RUN cd console/frontend && npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.0@sha256:0ecdc2a9f6156af6451080bfe3d8382a662fcc4e209608c6f919e643453514c1 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/console ./console/cmd

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=frontend /app/console/backend/assets /console/assets
COPY --from=builder /out/console /usr/local/bin/console
USER 65532
ENTRYPOINT ["/usr/local/bin/console"]
