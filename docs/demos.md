# End-to-End Demos

These demos assume you have access to a Kubernetes cluster. The cluster can be
a local kind or minikube cluster, or a shared development cluster. See
[Quick Start](quickstart.md) and [Installation](installation.md) for cluster
requirements and Helm installation details.

Install the kruntimes control plane from the published Helm OCI chart:

```bash
KRUNTIMES_VERSION=0.0.3

kubectl create namespace kruntimes-system
helm upgrade --install kruntimes oci://ghcr.io/kruntimes/charts/kruntimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace kruntimes-system
kubectl wait deployment -n kruntimes-system -l app=kruntimes-controller --for=condition=Available --timeout=120s
kubectl wait deployment -n kruntimes-system -l app=kruntimes-scheduler --for=condition=Available --timeout=120s
```

Install the built-in Bash and Python Runtime definitions in a namespace
dedicated to experimentation:

```bash
kubectl create namespace kruntimes-demo
helm upgrade --install kruntimes-runtimes oci://ghcr.io/kruntimes/charts/kruntimes-runtimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace kruntimes-demo
kubectl get runtime,pods -n kruntimes-demo
```

## Demo 1: Low-Latency Bash and Python Runs

This demo shows the core warm-pool path: Kubernetes keeps Runtime Pods ready,
then kruntimes schedules Runs into those warm Pods.

Wait for Runtime Pods:

```bash
kubectl wait pod -n kruntimes-demo -l runtime=bash --for=condition=Ready --timeout=120s
kubectl wait pod -n kruntimes-demo -l runtime=python --for=condition=Ready --timeout=120s
```

Create a Bash Run:

```bash
kubectl apply -n kruntimes-demo -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: demo-bash
spec:
  runtime: bash
  source:
    inline: |
      echo "language=bash" >> "$KRUNTIME_OUTPUTS"
      echo "hello from bash"
  entrypoint: script
EOF
```

Create a Python Run:

```bash
kubectl apply -n kruntimes-demo -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: demo-python
spec:
  runtime: python
  source:
    inline: |
      import os
      print("hello from python")
      with open(os.environ["KRUNTIME_OUTPUTS"], "a", encoding="utf-8") as f:
          f.write("language=python\n")
  entrypoint: script
EOF
```

Watch the Runs:

```bash
kubectl get runs -n kruntimes-demo -w
```

Inspect compact outputs:

```bash
kubectl get run demo-bash -n kruntimes-demo -o jsonpath='{.status.outputs}{"\n"}'
kubectl get run demo-python -n kruntimes-demo -o jsonpath='{.status.outputs}{"\n"}'
```

Inspect execution logs. If you have the `krt` CLI installed, use:

```bash
krt logs demo-bash -n kruntimes-demo
krt logs demo-python -n kruntimes-demo
```

Without `krt`, read the assigned Runtime Pod's structured runtimed logs and
filter by Run UID:

```bash
BASH_POD="$(kubectl get run demo-bash -n kruntimes-demo -o jsonpath='{.status.assignedPod}')"
BASH_UID="$(kubectl get run demo-bash -n kruntimes-demo -o jsonpath='{.metadata.uid}')"
kubectl logs "$BASH_POD" -n kruntimes-demo -c runtimed | grep "\"run_uid\":\"${BASH_UID}\""

PYTHON_POD="$(kubectl get run demo-python -n kruntimes-demo -o jsonpath='{.status.assignedPod}')"
PYTHON_UID="$(kubectl get run demo-python -n kruntimes-demo -o jsonpath='{.metadata.uid}')"
kubectl logs "$PYTHON_POD" -n kruntimes-demo -c runtimed | grep "\"run_uid\":\"${PYTHON_UID}\""
```

The logs should include `hello from bash` and `hello from python`.

## Demo 2: Burst Short-Task Execution

This demo submits more Runs than a single Runtime Pod can execute at once. The
scheduler should keep excess Runs Pending or Scheduled until warm Runtime
capacity is available, then continue dispatching as earlier Runs finish.

Scale the Bash Runtime to two replicas with two concurrent Runs per Pod:

```bash
kubectl patch runtime bash -n kruntimes-demo --type merge -p '{
  "spec": {
    "replicas": 2,
    "capacity": {
      "resources": {
        "runs": "2"
      }
    }
  }
}'
kubectl wait pod -n kruntimes-demo -l runtime=bash --for=condition=Ready --timeout=120s
```

Submit a burst of short Runs:

```bash
for i in $(seq 1 12); do
  kubectl apply -n kruntimes-demo -f - <<EOF
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: burst-$i
spec:
  runtime: bash
  source:
    inline: |
      echo "run=$i" >> "\$KRUNTIME_OUTPUTS"
      sleep 2
      echo "done $i"
  entrypoint: script
EOF
done
```

Observe scheduling and completion:

```bash
kubectl get runs -n kruntimes-demo -w
kubectl get runs -n kruntimes-demo \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,POD:.status.assignedPod
```

The exact ordering depends on cluster timing, but all Runs should eventually
reach `Succeeded` when Runtime Pods remain healthy.

Inspect logs for one burst Run:

```bash
krt logs burst-1 -n kruntimes-demo
```

Or use Kubernetes logs directly:

```bash
BURST_POD="$(kubectl get run burst-1 -n kruntimes-demo -o jsonpath='{.status.assignedPod}')"
BURST_UID="$(kubectl get run burst-1 -n kruntimes-demo -o jsonpath='{.metadata.uid}')"
kubectl logs "$BURST_POD" -n kruntimes-demo -c runtimed | grep "\"run_uid\":\"${BURST_UID}\""
```

## Demo 3: Custom Bash Runtime Image

A custom Runtime does not have to implement a new Runtime Server. A common
starting point is to reuse the built-in Bash Runtime and add packages or
internal binaries that your Runs need. See
[Custom Runtime Development](custom-runtime.md) for the full customization
model, including Pod template fields and the advanced Runtime Server path.

Create a small Dockerfile that extends the published Bash Runtime image:

```bash
mkdir -p my-bash-runtime
cat > my-bash-runtime/Dockerfile <<'EOF'
ARG KRUNTIMES_VERSION=0.0.3
FROM ghcr.io/kruntimes/bash-runtime:${KRUNTIMES_VERSION}

USER 0
RUN apt-get update \
  && apt-get install -y --no-install-recommends jq \
  && rm -rf /var/lib/apt/lists/*
USER 65532
EOF
```

Build and push the custom Runtime image to a registry your cluster can pull:

```bash
CUSTOM_BASH_IMAGE=<registry>/my-bash-runtime:0.1.0
docker build \
  --build-arg KRUNTIMES_VERSION="${KRUNTIMES_VERSION}" \
  -t "${CUSTOM_BASH_IMAGE}" \
  ./my-bash-runtime
docker push "${CUSTOM_BASH_IMAGE}"
```

Create the Runtime:

```bash
kubectl apply -n kruntimes-demo -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: custom-bash
spec:
  port: 9091
  replicas: 1
  capacity:
    resources:
      runs: "1"
  template:
    spec:
      containers:
        - name: runtime
          image: <registry>/my-bash-runtime:0.1.0
          args:
            - --port=9091
          ports:
            - containerPort: 9091
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "1"
              memory: 1Gi
EOF
```

The `Runtime.spec.template` field is a Pod template. You can customize the
runtime container image, packages, resources, environment, security context,
ServiceAccount, scheduling constraints, init containers, volumes, and sidecars
as long as they do not conflict with kruntimes-managed fields.

Wait for the Runtime Pod:

```bash
kubectl wait pod -n kruntimes-demo -l runtime=custom-bash --for=condition=Ready --timeout=120s
```

Create a Run for the custom Runtime:

```bash
kubectl apply -n kruntimes-demo -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: custom-runtime-demo
spec:
  runtime: custom-bash
  source:
    inline: |
      echo '{"runtime":"custom-bash","tool":"jq"}' | jq -r '.runtime + " has " + .tool'
  entrypoint: script
EOF
```

Inspect status:

```bash
kubectl get run custom-runtime-demo -n kruntimes-demo -w
kubectl describe run custom-runtime-demo -n kruntimes-demo
krt logs custom-runtime-demo -n kruntimes-demo
```

Implementing a new Runtime Server is a more advanced customization path. Use
that when Bash, Python, or another existing Runtime cannot model the execution
semantics you need. The protocol contract is covered in
[Custom Runtime Development](custom-runtime.md).

## Clean Up

```bash
kubectl delete namespace kruntimes-demo
helm uninstall kruntimes -n kruntimes-system
kubectl delete namespace kruntimes-system
```

Deleting the demo namespace removes demo Runs and Runtime objects. Uninstalling
the control-plane chart removes the shared controller and scheduler workloads;
Helm keeps CRDs by default.
