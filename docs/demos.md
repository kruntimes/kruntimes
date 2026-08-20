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

## Demo 4: Workflow Reuse and Data Sharing

This demo records the target v0.x workflow shape and the current API skeleton.
The current release includes `WorkflowRun`, reusable `Workflow`, and `Action`
definition CRDs, but workflow execution, `uses` resolution, Action expansion,
artifact fan-in, and workspace wiring are still planned work.

Create reusable definitions and a WorkflowRun execution instance:

```bash
kubectl apply -n kruntimes-demo -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Action
metadata:
  name: setup-python-tools
spec:
  inputs:
    version:
      type: string
      default: "3.12"
  steps:
    - name: setup
      run: |
        echo "python-version=${{ inputs.version }}" >> "$KRUNTIME_OUTPUTS"
        echo "installing toolchain"
---
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: build-and-test
spec:
  inputs:
    image:
      type: string
      required: true
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: |
            echo "building ${{ inputs.image }}"
            echo package=ok >> "$KRUNTIME_OUTPUTS"
    test:
      runs-on: bash
      needs:
        - build
      steps:
        - name: verify
          run: |
            echo "tests passed"
---
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  uses: build-and-test
  with:
    image: agent:v0.1.0
EOF
```

Inspect the created skeleton resources:

```bash
kubectl get actions,workflows,workflowruns -n kruntimes-demo
kubectl get workflow build-and-test -n kruntimes-demo -o yaml
kubectl get workflowrun release-demo -n kruntimes-demo -o yaml
```

At this stage, treat the WorkflowRun as an API object rather than an executable
workflow. The controller does not yet create child Runs or resolve reusable
definitions into an execution graph.

Expected workflow data sharing should look like this:

- Jobs pass durable data to other jobs through ArtifactStore-backed artifacts.
- Runs within one job share a job-local `PersistentWorkspace` created by the
  workflow controller.
- Workflow API does not expose workspace plumbing for the common case.
- Scheduler and runtimed stay workflow-agnostic. They only implement generic
  Run affinity and workspace primitives.

Target data-sharing sketch for future v0.x work:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: task-with-persistent-workspace
spec:
  runtime: bash
  workspace:
    name: ci-build-workspace
    kind: PersistentWorkspace
    apiGroup: kruntimes.io/v1alpha1
  source:
    inline: |
      mkdir -p src
      echo 'print("hello")' > src/app.py
---
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: ci-data-sharing-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: checkout
          run: |
            mkdir -p src
            echo 'print("hello")' > src/app.py
        - name: test
          run: |
            test -f src/app.py
            echo "tests=passed" >> "$KRUNTIME_OUTPUTS"
        - name: package
          run: |
            mkdir -p "$KRUNTIME_ARTIFACTS_DIR"
            tar -czf "$KRUNTIME_ARTIFACTS_DIR/dist.tgz" src
    deploy:
      runs-on: bash
      needs:
        - build
      steps:
        - name: verify-artifact
          artifacts:
            - from: jobs.build.artifacts.dist.tgz
              path: ./dist.tgz
          run: |
            tar -tzf dist.tgz
            echo "artifact verified"
```

In this model, `checkout`, `test`, and `package` share the same job-local
workspace without uploading and downloading intermediate files. The `deploy`
job receives only the explicit `dist.tgz` artifact from `build`, so data
crossing a job boundary remains durable, auditable, and independent of Runtime
Pod placement.

Reusable Workflow and Action expansion should look like this:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Action
metadata:
  name: setup-python-tools
spec:
  inputs:
    version:
      type: string
      default: "3.12"
  steps:
    - name: setup
      run: |
        echo "python-version=${{ inputs.version }}" >> "$KRUNTIME_OUTPUTS"
        echo "installing toolchain"
---
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: build-and-test
spec:
  inputs:
    image:
      type: string
      required: true
  jobs:
    build:
      runs-on: bash
      steps:
        - name: setup
          uses: setup-python-tools
          with:
            version: "3.13"
        - name: package
          run: |
            echo "building ${{ inputs.image }}"
---
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  jobs:
    release:
      uses: build-and-test
      with:
        image: agent:v0.1.0
```

`WorkflowRun.spec.uses`, `Workflow.spec.jobs.<job>.uses`, and `Action` CRDs are
already part of the skeleton API. The missing pieces are namespace-local lookup,
input binding, output propagation, step-level Action expansion, and execution
status.

## Demo 5: LLM Agent Tool Execution

This demo is a target v0.x scenario and is not supported by the current release
yet. It should remain draft until the product gaps in
[Function Mode and Agent Sandboxes](design/function-mode.md) are implemented.

The agent owns LLM reasoning, model routing, prompts, memory, and planning.
kruntimes owns the low-latency sandbox execution path. The SDK should expose
sandbox semantics to the agent developer, while the underlying Kubernetes
lifecycle object remains a function-mode Run.

Target agent-side SDK flow:

```python
from kruntimes import SandboxClient
from openai import OpenAI

client = OpenAI()
sandboxes = SandboxClient(namespace="kruntimes-demo")

with sandboxes.create_sandbox(
    name="kube-diagnose-agent",
    runtime="python-agent-sandbox",
    source_file="agent_tools.py",
    idle_timeout_seconds=600,
) as sandbox:
    plan = client.responses.create(
        model="gpt-4.1",
        input="Diagnose pending Kubernetes pods and produce next actions.",
    )

    sandbox.files.write("cluster-snapshot.json", b'{"namespace":"default","pods":[]}')

    result = sandbox.commands.run({
        "tool": "diagnose-kubernetes",
        "plan": plan.output_text,
        "inputPath": "cluster-snapshot.json",
    })

    report = sandbox.files.read("report.md")
    print(result.outputs["summary"])
```

The SDK presents a sandbox handle with `commands`, `files`, `logs`,
`artifacts`, `info`, `disconnect`, `reattach`, and `terminate` operations.
The caller does not need to know which Runtime Pod owns the sandbox or how the
gateway connection is established.

The underlying target Run shape is:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: python-agent-sandbox
spec:
  capacity:
    resources:
      runs: "1"
---
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: kube-diagnose-agent
spec:
  runtime: python-agent-sandbox
  source:
    inline: |
      def invoke(request):
          return {
              "outputs": {
                  "summary": "diagnosis complete"
              },
              "artifactRefs": [
                  {
                      "name": "report.md",
                      "uri": "s3://kruntimes-artifacts/runs/kube-diagnose-agent/report.md"
                  }
              ],
          }
  mode:
    function:
      handler: agent_tools.invoke
      idleTimeoutSeconds: 600
```

Function-mode Runs still obey Runtime capacity. Multiple function-mode Runs can
share one Runtime Pod when capacity allows it. For AgentSandbox-style use, the
recommended deployment shape is one Run per Runtime Pod, so the SDK should warn
or help create a dedicated Runtime when `runs` is greater than `1`.

Required gaps before this demo can become executable:

- Function-mode registration and readiness handling for
  `Run.spec.mode.function.handler`.
- Runtime gateway Service per Runtime.
- runtimed ownership cache and invoke routing.
- Runtime Server register, invoke, unregister, and status APIs.
- Python and Go sandbox-facing SDKs.
- Workspace/file/log/artifact APIs that do not require Kubernetes
  reconciliation for each operation.
- E2E coverage for ready, local/proxied invoke, repeated invocation,
  disconnect, reattach, terminate, idle timeout, cleanup, and Runtime Pod
  restart recovery.

## Clean Up

```bash
kubectl delete namespace kruntimes-demo
helm uninstall kruntimes -n kruntimes-system
kubectl delete namespace kruntimes-system
```

Deleting the demo namespace removes demo Runs and Runtime objects. Uninstalling
the control-plane chart removes the shared controller and scheduler workloads;
Helm keeps CRDs by default.
