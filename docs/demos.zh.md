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
  entrypoint: script
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
  entrypoint: script
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
  entrypoint: script
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
  entrypoint: script
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

## 清理

```bash
kubectl delete namespace kruntimes-demo
helm uninstall kruntimes -n kruntimes-system
kubectl delete namespace kruntimes-system
```

删除 demo namespace 会删除 demo Runs 和 Runtime objects。卸载 control-plane chart 会删除
共享 controller 和 scheduler workloads；Helm 默认会保留 CRDs。
