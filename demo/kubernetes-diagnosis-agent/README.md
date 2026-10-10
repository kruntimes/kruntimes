# Kubernetes Diagnosis Agent

This preview demonstrates an agent that uses OpenAI tool calls and one
kruntimes Session Run to inspect a Kubernetes namespace over multiple steps.
Each diagnostic result is stored under the session workspace's `evidence/`
directory and the final model response is stored as `report.md` before the
agent closes the Session Run.

The Session Runtime is deliberately limited to read-only namespace diagnostics.
It is a trusted-workload preview, not a safe environment for untrusted
LLM-generated code. The OpenAI API key remains in the client process and is
never sent to the Runtime Pod.

## Prerequisites

- A Kubernetes cluster with kruntimes installed and the shared Runtime gateway
  enabled.
- An image registry that your cluster can pull from.
- `kubectl`, Python 3.10+, and an `OPENAI_API_KEY` in the local environment.
- A caller identity allowed to create, get, update, and delete `kruntimes.io/runs` in
  the target namespace. Gateway requests require `get` access to that Run.
  For local access, it also needs `get` on the shared gateway Service and
  `get`, `list` on its Pods in the gateway namespace to establish the scoped
  port-forward.

## Deploy the Runtime

Choose an image name and build the image that adds `kubectl` to the built-in
Python Runtime:

```bash
export KRUNTIMES_VERSION=0.0.3
export DIAGNOSIS_RUNTIME_IMAGE=ghcr.io/<owner>/diagnosis-python-runtime:0.1.0
docker build \
  --build-arg KRUNTIMES_VERSION="${KRUNTIMES_VERSION}" \
  --tag "${DIAGNOSIS_RUNTIME_IMAGE}" \
  .
docker push "${DIAGNOSIS_RUNTIME_IMAGE}"
```

Create a namespace and its read-only Runtime ServiceAccount and Role:

```bash
kubectl create namespace agent-demo
kubectl apply --namespace agent-demo -f rbac.yaml
sed "s|<registry>/diagnosis-python-runtime:0.1.0|${DIAGNOSIS_RUNTIME_IMAGE}|" runtime.yaml \
  | kubectl apply --namespace agent-demo -f -
kubectl wait pod --namespace agent-demo -l runtime=diagnosis-python \
  --for=condition=Ready --timeout=120s
```

The `runs: "1"` capacity makes the Session Run exclusive to one Runtime Pod.
The Dockerfile pins `kubectl` to v1.32.0 and verifies its official SHA-256
checksum during the image build.

## Run the Agent

Install the SDK from this repository and the OpenAI client:

```bash
python -m pip install -e ../../sdk/python[kubernetes] openai
export OPENAI_API_KEY=<key>
python agent.py --namespace agent-demo
```

The local SDK starts a scoped `kubectl port-forward` to the shared Runtime
gateway. It preserves the endpoint path published in `Run.status.endpoint` and
does not expose runtimed or Runtime Server gRPC ports.

The agent accepts only `pods`, `events`, `services`, `deployments`, and
`replicasets` tool calls. It maps them to argv-only `kubectl get` commands
within the requested namespace; it does not execute model-provided shell text.
After completion, inspect the Run and its structured operation logs:

```bash
kubectl get runs --namespace agent-demo
RUN_NAME=<created-run-name>
krt logs "${RUN_NAME}" --namespace agent-demo
```

For an in-cluster agent, add `--in-cluster`. The agent Pod's ServiceAccount
needs the same Run and gateway permissions described above.
