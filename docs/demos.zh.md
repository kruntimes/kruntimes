# 端到端 Demo

这些 demo 假设你可以访问一个 Kubernetes 集群。这个集群可以是本地 kind 或 minikube
集群，也可以是共享开发集群。集群要求和 Helm 安装细节见
[快速开始](quickstart.md) 和 [安装](installation.md)。

从已发布的 Helm OCI chart 安装 kruntimes control plane：

```bash
KRUNTIMES_VERSION=0.0.3

kubectl create namespace kruntimes-system
helm upgrade --install kruntimes oci://ghcr.io/kruntimes/charts/kruntimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace kruntimes-system
kubectl wait deployment -n kruntimes-system -l app=kruntimes-controller --for=condition=Available --timeout=120s
kubectl wait deployment -n kruntimes-system -l app=kruntimes-scheduler --for=condition=Available --timeout=120s
```

在单独的实验 namespace 中安装内置 Bash 和 Python Runtime 定义：

```bash
kubectl create namespace kruntimes-demo
helm upgrade --install kruntimes-runtimes oci://ghcr.io/kruntimes/charts/kruntimes-runtimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace kruntimes-demo
kubectl get runtime,pods -n kruntimes-demo
```

## Demo 1：低延迟 Bash 和 Python Run

这个 demo 展示 warm-pool 核心路径：Kubernetes 保持 Runtime Pod ready，kruntimes 把
Run 调度到这些 warm Pod 中。

等待 Runtime Pod：

```bash
kubectl wait pod -n kruntimes-demo -l runtime=bash --for=condition=Ready --timeout=120s
kubectl wait pod -n kruntimes-demo -l runtime=python --for=condition=Ready --timeout=120s
```

创建 Bash Run：

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

创建 Python Run：

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

观察 Run：

```bash
kubectl get runs -n kruntimes-demo -w
```

查看 compact outputs：

```bash
kubectl get run demo-bash -n kruntimes-demo -o jsonpath='{.status.outputs}{"\n"}'
kubectl get run demo-python -n kruntimes-demo -o jsonpath='{.status.outputs}{"\n"}'
```

查看执行日志。如果已经安装 `krt` CLI，可以使用：

```bash
krt logs demo-bash -n kruntimes-demo
krt logs demo-python -n kruntimes-demo
```

如果没有 `krt`，也可以读取 assigned Runtime Pod 中的结构化 runtimed logs，并按 Run UID
过滤：

```bash
BASH_POD="$(kubectl get run demo-bash -n kruntimes-demo -o jsonpath='{.status.assignedPod}')"
BASH_UID="$(kubectl get run demo-bash -n kruntimes-demo -o jsonpath='{.metadata.uid}')"
kubectl logs "$BASH_POD" -n kruntimes-demo -c runtimed | grep "\"run_uid\":\"${BASH_UID}\""

PYTHON_POD="$(kubectl get run demo-python -n kruntimes-demo -o jsonpath='{.status.assignedPod}')"
PYTHON_UID="$(kubectl get run demo-python -n kruntimes-demo -o jsonpath='{.metadata.uid}')"
kubectl logs "$PYTHON_POD" -n kruntimes-demo -c runtimed | grep "\"run_uid\":\"${PYTHON_UID}\""
```

日志中应该能看到 `hello from bash` 和 `hello from python`。

## Demo 2：Burst Short-Task Execution

这个 demo 一次提交超过单个 Runtime Pod 可并发执行数量的 Runs。scheduler 应该让超出容量
的 Runs 保持 Pending 或 Scheduled，等前面的 Runs 完成释放 warm Runtime 容量后继续调度。

把 Bash Runtime 调整为两个副本，每个 Pod 两个并发 Run：

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

提交一批短任务：

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

观察调度和完成情况：

```bash
kubectl get runs -n kruntimes-demo -w
kubectl get runs -n kruntimes-demo \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,POD:.status.assignedPod
```

具体顺序取决于集群时序，但只要 Runtime Pod 保持健康，所有 Runs 最终都应该进入
`Succeeded`。

查看其中一个 burst Run 的日志：

```bash
krt logs burst-1 -n kruntimes-demo
```

也可以直接使用 Kubernetes logs：

```bash
BURST_POD="$(kubectl get run burst-1 -n kruntimes-demo -o jsonpath='{.status.assignedPod}')"
BURST_UID="$(kubectl get run burst-1 -n kruntimes-demo -o jsonpath='{.metadata.uid}')"
kubectl logs "$BURST_POD" -n kruntimes-demo -c runtimed | grep "\"run_uid\":\"${BURST_UID}\""
```

## Demo 3：自定义 Bash Runtime 镜像

custom Runtime 不一定要实现一个新的 Runtime Server。更常见的起点是复用内置 Bash
Runtime，然后添加你的 Runs 需要的 packages 或内部 binaries。完整定制模型见
[自定义 Runtime 开发](custom-runtime.md)，其中包括 Pod template 字段和更高级的
Runtime Server 路径。

创建一个扩展已发布 Bash Runtime image 的 Dockerfile：

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

构建并推送 custom Runtime image 到集群可拉取的 registry：

```bash
CUSTOM_BASH_IMAGE=<registry>/my-bash-runtime:0.1.0
docker build \
  --build-arg KRUNTIMES_VERSION="${KRUNTIMES_VERSION}" \
  -t "${CUSTOM_BASH_IMAGE}" \
  ./my-bash-runtime
docker push "${CUSTOM_BASH_IMAGE}"
```

创建 Runtime：

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

`Runtime.spec.template` 是一个 Pod template。你可以定制 runtime container image、
packages、resources、environment、security context、ServiceAccount、调度约束、
init containers、volumes 和 sidecars，只要它们不与 kruntimes 管理的字段冲突。

等待 Runtime Pod：

```bash
kubectl wait pod -n kruntimes-demo -l runtime=custom-bash --for=condition=Ready --timeout=120s
```

创建 custom Runtime Run：

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

查看状态：

```bash
kubectl get run custom-runtime-demo -n kruntimes-demo -w
kubectl describe run custom-runtime-demo -n kruntimes-demo
krt logs custom-runtime-demo -n kruntimes-demo
```

实现新的 Runtime Server 是更高级的定制路径。当 Bash、Python 或其他已有 Runtime 无法表达
你需要的执行语义时，再选择这种方式。协议约定见
[自定义 Runtime 开发](custom-runtime.md)。

## Demo 4：Workflow 复用和数据共享

这个 demo 记录 v0.x 的目标 workflow 形态以及当前 API skeleton。当前 release 已经包含
`WorkflowRun`、可复用 `Workflow` 和 `Action` definition CRD，但 workflow 执行、`uses`
解析、Action 展开、artifact fan-in 和 workspace wiring 仍然是计划中的工作。

创建可复用 definition 和一个 WorkflowRun execution instance：

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

查看已经创建的 skeleton resources：

```bash
kubectl get actions,workflows,workflowruns -n kruntimes-demo
kubectl get workflow build-and-test -n kruntimes-demo -o yaml
kubectl get workflowrun release-demo -n kruntimes-demo -o yaml
```

在当前阶段，应该把 WorkflowRun 看作 API object，而不是一个已经可执行的 workflow。controller
还不会创建 child Runs，也不会把 reusable definitions 解析成执行图。

预期的 workflow data-sharing 模型应该是：

- job 之间通过 ArtifactStore-backed artifacts 传递持久数据。
- 同一个 job 内的 Runs 共享由 workflow controller 创建的 job-local `PersistentWorkspace`。
- 常见场景下，Workflow API 不向用户暴露 workspace plumbing。
- scheduler 和 runtimed 保持 workflow-agnostic，只实现通用的 Run affinity 和 workspace
  primitives。

未来 v0.x work 的目标 data-sharing 草图：

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

在这个模型里，`checkout`、`test` 和 `package` 可以共享同一个 job-local workspace，不需要
反复上传和下载中间文件。`deploy` job 只接收来自 `build` 的显式 `dist.tgz` artifact，因此
跨 job 边界的数据仍然是持久、可审计的，并且不依赖 Runtime Pod placement。

Reusable Workflow 和 Action expansion 的目标形态应该是：

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

`WorkflowRun.spec.uses`、`Workflow.spec.jobs.<job>.uses` 和 `Action` CRD 已经属于 skeleton
API。缺口是 namespace-local lookup、input binding、output propagation、step-level Action
expansion 和 execution status。

## Demo 5：LLM Agent Tool Execution

这个 demo 是 v0.x 的目标场景，当前 release 尚不支持。在
[Function Mode 和 Agent Sandboxes](design/function-mode.md) 中列出的产品 gap 完成之前，
它应该保持 draft 状态。

Agent 负责 LLM reasoning、model routing、prompts、memory 和 planning。kruntimes 负责
低延迟 sandbox execution path。SDK 应向 agent 开发者暴露 sandbox 语义，而底层 Kubernetes
生命周期对象仍然是 function-mode Run。

目标 agent-side SDK flow：

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

SDK 向调用方呈现一个 sandbox handle，包含 `commands`、`files`、`logs`、`artifacts`、
`info`、`disconnect`、`reattach` 和 `terminate` 等操作。调用方不需要知道哪个 Runtime
Pod 拥有这个 sandbox，也不需要关心 gateway connection 如何建立。

底层目标 Run 形态是：

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

Function-mode Runs 仍然遵守 Runtime capacity。当 capacity 允许时，多个 function-mode
Runs 可以共享同一个 Runtime Pod。对于 AgentSandbox-style 使用，推荐的部署形态是每个
Runtime Pod 只承载一个 Run，因此当 `runs` 大于 `1` 时，SDK 应该 warning，或帮助创建
dedicated Runtime。

支持这个 demo 前必须补齐的 gap：

- `Run.spec.mode.function.handler` 的 function-mode registration 和 readiness handling。
- 为每个 Runtime 创建 Runtime gateway Service。
- runtimed ownership cache 和 invoke routing。
- Runtime Server register、invoke、unregister 和 status APIs。
- Python 和 Go sandbox-facing SDKs。
- 不需要为每个操作走 Kubernetes reconciliation 的 workspace/file/log/artifact APIs。
- E2E 覆盖 ready、local/proxied invoke、repeated invocation、disconnect、reattach、
  terminate、idle timeout、cleanup 和 Runtime Pod restart recovery。

## 清理

```bash
kubectl delete namespace kruntimes-demo
helm uninstall kruntimes -n kruntimes-system
kubectl delete namespace kruntimes-system
```

删除 demo namespace 会删除 demo Runs 和 Runtime objects。卸载 control-plane chart 会删除
共享 controller 和 scheduler workloads；Helm 默认会保留 CRDs。
