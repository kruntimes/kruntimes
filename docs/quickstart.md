# Quick Start

This guide gets kruntimes running on a local kind cluster and executes one Bash
Run.

## Prerequisites

- Go version from `go.mod`
- Docker or another compatible container tool
- kind
- kubectl
- Helm 3

## Start a Local Environment

Build local images, create or reuse a kind cluster, load images, install CRDs,
and deploy the platform chart:

```bash
make e2e-setup
```

The default namespace is `default`. Override it with:

```bash
NAMESPACE=kruntimes-system make e2e-setup
```

## Create a Runtime

The E2E setup installs the platform but not every user Runtime. Create a Bash
Runtime in the workload namespace:

```bash
kubectl apply -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: bash
spec:
  replicas: 1
  capacity:
    resources:
      runs: 4
  template:
    metadata:
      labels:
        runtime: bash
    spec:
      containers:
        - name: runtime
          image: kruntimes-bash-runtime:latest
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 19091
EOF
```

Wait for the Runtime Pod:

```bash
kubectl get pods -l runtime=bash -w
```

## Run a Command

```bash
kubectl apply -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  args:
    - echo
    - hello from kruntimes
EOF
```

Watch status:

```bash
kubectl get run hello -w
```

Inspect the final object:

```bash
kubectl get run hello -o yaml
```

## Run the Full E2E Suite

```bash
make e2e
```

## Clean Up

```bash
make e2e-cleanup
```

## Next Steps

- [Installation](installation.md)
- [Usage Guide](usage.md)
- [Troubleshooting](troubleshooting.md)
