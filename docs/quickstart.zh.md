---
title: "快速开始"
---

本指南演示如何在已有 Kubernetes 集群上安装发布版 kruntimes，并执行一个 Bash Run。

## 前置条件

- 支持 CRD 的 Kubernetes 集群
- kubectl
- Helm 3

kruntimes 运行在 Kubernetes 上。这个集群可以是生产集群，也可以是本地开发集群，
例如 [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) 或
[minikube](https://minikube.sigs.k8s.io/docs/start/)。请先按照集群提供方的
setup guide 完成集群准备，然后确认 kubectl 可访问：

```bash
kubectl cluster-info
```

## 安装 kruntimes

设置 Helm charts 和 images 使用的发布版本：

```bash
KRUNTIMES_VERSION=0.0.3
```

每个集群安装一次 control plane：

```bash
helm upgrade --install kruntimes oci://ghcr.io/kruntimes/charts/kruntimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace kruntimes-system \
  --create-namespace
```

将内置 Runtime definitions 安装到 Runs 将要执行的 namespace：

```bash
helm upgrade --install kruntimes-runtimes oci://ghcr.io/kruntimes/charts/kruntimes-runtimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace default \
  --create-namespace
```

charts 默认使用发布在 `ghcr.io/kruntimes/*` 下的镜像仓库，并在 image value 没有 tag
或 digest 时自动追加 chart `appVersion`。

检查 control plane 和 Runtime Pods：

```bash
kubectl get deploy -n kruntimes-system
kubectl get runtime,pods -n default
```

等待 control plane 和 Bash Runtime Pods ready：

```bash
kubectl wait deployment -n kruntimes-system -l app=kruntimes-controller --for=condition=Available --timeout=120s
kubectl wait deployment -n kruntimes-system -l app=kruntimes-scheduler --for=condition=Available --timeout=120s
kubectl wait pod -n default -l runtime=bash --for=condition=Ready --timeout=120s
```

## 执行命令

```bash
kubectl apply -n default -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  source:
    inline: |
      echo "hello from kruntimes"
EOF
```

观察状态：

```bash
kubectl get run hello -n default -w
```

查看最终对象：

```bash
kubectl get run hello -n default -o yaml
```

查看日志。如果已经安装 `krt` CLI，可以使用：

```bash
krt logs hello -n default
```

`krt` 的安装命令见 [Installation](installation.md#krt-cli)。如果没有 `krt`，也可以读取
assigned Runtime Pod 中的结构化 runtimed logs：

```bash
HELLO_POD="$(kubectl get run hello -n default -o jsonpath='{.status.assignedPod}')"
HELLO_UID="$(kubectl get run hello -n default -o jsonpath='{.metadata.uid}')"
kubectl logs "$HELLO_POD" -n default -c runtimed | grep "\"run_uid\":\"${HELLO_UID}\""
```

## 清理

```bash
helm uninstall kruntimes-runtimes --namespace default --ignore-not-found
helm uninstall kruntimes --namespace kruntimes-system --ignore-not-found
```

Helm uninstall 不会删除 kruntimes CRDs。集群卸载细节见
[Installation](installation.md) 和 [Operations Guide](operations.md)。

## 下一步

- [Installation](installation.md)
- [Usage Guide](usage.md)
- [End-to-End Demos](demos.md)
- [Troubleshooting](troubleshooting.md)
