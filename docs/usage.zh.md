---
title: "使用指南"
---

本指南覆盖 Runtime 和 Run 对象的常见用户流程。

## 创建 Runtime

Runtime 定义一组预热的 Runtime Pods。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: bash
spec:
  replicas: 2
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
```

重要字段：

- `spec.replicas`：Runtime Pods 数量。
- `spec.capacity.resources.runs`：每个 Runtime Pod 可并发执行的 Runs 数。
- `spec.template`：用于创建 Runtime Pods 的 Pod template。
- `spec.template.spec.serviceAccountName`：可选的用户自定义 workload ServiceAccount；
  controller 会在同一 namespace 内授予 runtimed 所需权限。

## 创建 Run

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  args:
    - echo
    - hello
```

scheduler 会 watch Pending Runs，并将它们分配到同一 namespace 内健康的 Runtime Pods。

## 使用环境变量

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: env-example
spec:
  runtime: bash
  env:
    MESSAGE: hello
  args:
    - sh
    - -c
    - echo "$MESSAGE"
```

不要把 secrets 直接放在 `Run.spec.env`。请使用 namespace 隔离、Runtime-controlled
mounts，或适合你的集群的 admission policy。

## 使用 Source 和 Entrypoint

Run source 会被准备到每个 Run 独立的 workspace。Entrypoints 必须是相对路径，
且不能包含 `..`。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: script-example
spec:
  runtime: bash
  source:
    inline:
      files:
        main.sh: |
          echo "hello from source"
  entrypoint: main.sh
```

## Outputs

workload 以 `KEY=VALUE` 行的形式将结构化 outputs 写入 `$KRUNTIME_OUTPUTS`。
runtimed 会把有界 outputs 存储到 `Run.status.outputs`。

```bash
echo "result=ok" >> "$KRUNTIME_OUTPUTS"
```

## Artifacts

`$KRUNTIME_ARTIFACTS_DIR` 下的文件会通过配置的 ArtifactStore 持久化。
Run status 存储紧凑的 `artifactRefs` metadata，而不是完整 artifact data。

```bash
mkdir -p "$KRUNTIME_ARTIFACTS_DIR"
echo "artifact body" > "$KRUNTIME_ARTIFACTS_DIR/result.txt"
```

## 取消

设置 `spec.cancelRequested` 请求取消：

```bash
kubectl patch run hello --type merge -p '{"spec":{"cancelRequested":true}}'
```

取消生效后，终态 phase 会变为 `Cancelled`。

## Timeout 和 Retry

Runs 可以定义 timeout 和 retry policy。Timeout 会以 `Timeout` 终态结束，而不是泛化为
`Failed`。

Retry 语义是 at-least-once。Runtime Servers 必须让重复 `Execute` delivery 具备确定性
且安全。

## Logs

完整 stdout 和 stderr 通过带 Run UID 的结构化 runtimed logs 暴露。它们不会被完整复制到
`status.message`。

## CLI

`krt` CLI 支持 kubeconfig/context、namespace 选择、等待、输出格式、logs、取消和结果查看。
发布的 release binaries 见 [Release Process](release.md)。
