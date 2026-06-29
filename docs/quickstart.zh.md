---
title: "快速开始"
---

本指南演示如何在已有 Kubernetes 集群上安装 kruntimes，并执行一个 Bash Run。

## 前置条件

- 支持 CRD 的 Kubernetes 集群。
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

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

将内置 Runtime definitions 安装到 Runs 将要执行的 namespace：

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace \
  --set bash.image=<bash-runtime-image> \
  --set python.image=<python-runtime-image>
```

请使用集群能够拉取的 image references。对于本地 kind 或 minikube 集群，可以把本地
build 的 images load 进集群，也可以让 chart 指向集群可访问的 registry。

检查 control plane 和 Runtime Pods：

```bash
kubectl get deploy -n kruntimes-system
kubectl get runtime,pods -n default
```

等待 Bash Runtime Pods ready：

```bash
kubectl get pods -n default -l runtime=bash -w
```

## 执行命令

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

观察状态：

```bash
kubectl get run hello -w
```

查看最终对象：

```bash
kubectl get run hello -o yaml
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
- [Troubleshooting](troubleshooting.md)
