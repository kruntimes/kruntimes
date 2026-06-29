---
title: "安全、授权与威胁模型"
---

kruntimes 使用 Kubernetes namespaces 和 RBAC 作为当前的行政管理边界。内置 Bash 和
Python runtimes 在共享 Runtime Pods 内执行受信任的代码；它们不是用于互不信任的租户的
沙箱。

## 威胁模型摘要

主要安全边界是 Kubernetes namespace。能够在 namespace 中创建 Run 的主体可以在该
namespace 中任何匹配的 Runtime Pod 内执行代码。能够创建或更新 Runtime 的主体可以影响
生成的 Runtime Deployment，包括可执行镜像、命令、资源、artifact 存储和 workload 身份。

kruntimes 目前保护其拥有的 Kubernetes API 对象，保持 Run 状态转换的显式性，限制对
runtimed endpoints 的直接入口访问，并记录了受信任 workload 的预期。它不在内置
runtimes 内提供 per-Run 沙箱隔离。

### 资产

| 资产 | 安全目标 |
| --- | --- |
| Run 和 Workflow specs | 防止未授权的代码执行、源码泄露和篡改。 |
| Run 状态、输出、artifact 引用和日志 | 防止未授权的泄露和误导性状态更新。 |
| Runtime specs 和生成的 Deployments | 限制谁可以选择可执行镜像、命令、卷、凭据和 service accounts。 |
| Runtime Pod workspace | 防止意外的跨 Run 数据复用；不声称提供恶意代码隔离。 |
| Artifact stores 和凭据 | 保持存储对象限定在所属 Run 范围内，防止凭据暴露给未授权用户。 |
| 控制平面 service accounts | 将 scheduler、controller 和 runtimed 的状态变更权限保留给 kruntimes 组件。 |

### 角色

| 角色 | 假设 |
| --- | --- |
| 集群管理员 | 受信安装 CRDs、Helm charts、controller RBAC 和 namespace 策略。 |
| Runtime 管理员 | 受信的 namespace 运维人员；可配置可执行的 Runtime pools 及其附带的凭据。 |
| Run 提交者 | 受信在 namespace 信任边界内执行代码，但不被信任进行控制平面状态写入。 |
| Run 观察者 | 可以检查 Run 状态、输出、artifact 元数据，在被授予 port-forward 权限时可选择性访问日志/artifacts。 |
| 内置 Runtime 实现 | 在共享 Runtime Pod 内运行的受信任代码；不是恶意代码沙箱。 |
| 自定义 Runtime 实现 | 负责其所声称的任何更强的隔离边界。 |

### 信任边界

- **Namespace 边界**：scheduler 的放置是 namespace 本地的。不应将互不信任的 Run 提交者
  与共享的内置 Runtime pool 放在同一个 namespace。
- **Runtime Pod 边界**：分配到同一内置 Runtime Pod 的所有 Runs 共享 Pod 的网络命名空间、
  runtime 进程命名空间、挂载的卷、workspace 卷和 Kubernetes service account。
- **控制平面边界**：只有 kruntimes controllers 应该更新 `runs/status`、
  `runtimes/status` 和生成的 Runtime Pod 就绪条件。
- **Artifact 边界**：artifacts 从 Run 状态中引用，但存储在 etcd 之外。存储配置和凭据是
  Runtime 管理员信任边界的一部分。

### 威胁、缓解措施和当前差距

| 威胁 | 当前缓解措施 | 当前差距或所需运维控制 |
| --- | --- | --- |
| 未授权用户通过创建 Runs 执行代码 | Kubernetes RBAC 控制 `runs` 创建。 | RBAC 无法按用户限制 `spec.runtime`、命令、env 或源码；使用 namespace 分离或 admission policy。 |
| Runtime 管理员部署恶意 runtime 或 sidecar 镜像 | 将 `runtimes` 写入权限视为 workload-admin 访问。 | kruntimes 不强制执行镜像策略；使用 admission controls 和签名镜像策略。 |
| Run 代码读取同一 Runtime Pod 中另一个 Run 的文件 | runtimed 使用 per-Run workspace 目录和清理。 | 内置 runtimes 不将恶意代码与共享 Pod 文件系统或 runtime 进程进行沙箱隔离。 |
| Run 代码使用 Runtime Pod 网络或 service account | Namespace 本地调度和记录的职责分离。 | 内置 runtimes 不提供 per-Run 网络或身份隔离；使用自定义 runtimes 或单独的 namespaces/service accounts。 |
| 用户从 Run specs 或状态中读取 Secrets | 文档警告不要在 `Run.spec.env` 中放置凭据；RBAC 控制 `runs` 读取。 | Kubernetes 为授权读者存储可读的 Run spec/status；改用受信 runtimes 挂载的 Secrets。 |
| 用户直接访问 runtimed 状态或 artifact endpoints | 默认 NetworkPolicy 限制 Pod 入口访问；CLI 访问使用 Kubernetes port-forward 权限。 | 运维人员必须仅向允许读取日志/artifacts 的用户授予 `pods/portforward`。 |
| 过时或受入侵的组件变更 Run 状态 | 状态写入保留给控制平面 service accounts。 | 集群 RBAC 不得向用户或无关 controllers 授予 status update verbs。 |
| Artifact 凭据泄露或对象混淆 | Artifact 引用是紧凑的元数据；store drivers 验证引用和路径。 | Runtime 管理员控制 artifact store 凭据；集中清理和恢复语义单独跟踪。 |
| 跨 namespace Run 执行 | Scheduler 仅考虑 Run namespace 中的 Runtime Pods。 | 集群级别的 controller RBAC 仍需仔细安装和审计。 |

### 内置 Runtimes 的非目标

内置 Bash 和 Python runtimes 不旨在提供：

- 同一 Runtime Pod 中互不信任的 Runs 之间的隔离；
- per-Run 的 Kubernetes service accounts、网络策略或 cgroups；
- 防止代码有意检查 Runtime Pod 内共享的进程、网络或文件系统状态；
- 直接放在 Run 对象上的租户级 secret 隔离。

当需要这些属性时，使用带有 per-Run 专用容器、沙箱或 microVM 的自定义 Runtime。

## 权限语义

根据权限提供的实际能力（而非仅 Kubernetes 资源名称）来授予 kruntimes 权限。

| 权限 | 安全含义 |
| --- | --- |
| `create`、`update` 或 `patch` `runtimes` | 在 namespace 中部署或更改可执行的 runtime 和 runtimed 镜像。Runtime 可以选择命令、环境、资源、artifact PVCs 和 artifact 凭据 Secrets。将其视为 namespace workload 管理员访问。 |
| `delete` `runtimes` | 移除 Runtime pool 并中断或搁置已分配给它的 Runs。 |
| `create` `runs` | 在匹配的 Runtime Pod 中执行代码。代码共享该 Pod 的进程、网络、workspace、挂载的卷和 workload 身份（取决于 Runtime 实现）。 |
| `update` 或 `patch` `runs` | 更改待处理 workload 或请求取消。kruntimes 目前不暴露更细粒度的取消子资源。 |
| `delete` `runs` | 移除执行状态，并在存在 finalizer 时启动 artifact 清理。 |
| `get`、`list` 或 `watch` `runs` | 读取 Run 对象上存储的源码引用、参数、环境变量值、执行状态、输出和 artifact 元数据。 |
| `update` 或 `patch` `runs/status` | 控制调度和执行状态。保留给 kruntimes 控制平面 service accounts。 |
| `create` `pods/portforward` 加上 `get` `pods` 和 `runs` | 通过 Kubernetes API 访问 Runtime Pod 的 runtimed 状态和 artifact endpoint，如 `krt logs` 和 artifact 下载所用。 |

Kubernetes 不会将 Run 与引用的 Runtime 作为单独操作授权。如果主体可以在 namespace
中创建 Run，它就可以请求该 namespace 中调度器可用的任何 Runtime 名称。

## 推荐的职责分离

为应用用户使用 namespace 范围内的 `Role` 和 `RoleBinding` 对象：

- **Runtime 管理员** 可以管理 `runtimes`。只有受信的平台运维人员应获得此能力，因为
  `spec.template` 中的镜像、命令、卷、service accounts 和调度字段，以及
  `spec.daemonImage` 和 artifact 凭据会影响生成的 Runtime Pods。
- **Run 提交者** 可以创建和读取 `runs`。仅当他们也需取消时才授予 `update` 或 `patch`；
  这些 verbs 目前允许更广泛的 Run 变更。
- **Run 观察者** 可以接收 `runs` 的只读访问。仅当允许他们读取 runtime 日志或 artifacts
  时才添加 `pods/portforward`。
- **控制平面 service accounts** 拥有状态变更权限。不要授予用户对 `runs/status` 或
  `runtimes/status` 的写入访问。

不要向 Run 提交者授予通配符 verbs 或资源。Kubernetes RBAC 无法将用户限制到特定的
`spec.runtime`、源码 URL、命令或环境变量值。当 namespace 包含具有不同信任级别的 Runtime
pools 时，使用 validating admission policy 或 admission webhook 强制执行这些策略。

## Namespace 信任边界

调度器仅将 Run 分配给同一 namespace 中的 Runtime Pods。因此 namespace 应将共享同一
信任级别的主体和 Runtime pools 分组。不要将互不信任的 Run 提交者与共享的内置 Runtime
pool 放在同一个 namespace 中。

Platform Helm chart 安装集群范围的控制平面以及集群级别的 controller 和 scheduler 角色。
这些角色是为 kruntimes 组件准备的，不是最终用户访问的示例。
应用用户访问应单独通过 namespace 范围的 RBAC 授予。

NetworkPolicy 限制了直接 Pod 入口访问，但它不隔离在同一 Runtime Pod 内执行的 Runs。
Run 代码可能共享：

- Runtime Pod 的网络命名空间和出站连接；
- 共享的 workspace 卷，由 runtimed 强制执行 per-Run 目录；
- runtime 进程命名空间和 runtime server；
- Runtime Pod 暴露的卷和 Kubernetes workload 身份。

对于不受信任的代码，使用自定义 Runtime 来创建更强的 per-Run 边界，如专用容器、沙箱化
runtime 或 microVM，并结合 namespace 隔离、受限的 service accounts、出口策略和
admission controls。

## Secrets 和输入

- 不要将凭据直接放在 `Run.spec.env` 中；Run 对象的读者可以检查其 spec。
- 将 Git 仓库和内联源码视为可执行输入。
- Runtime artifact `credentialsSecretName` 暴露给 runtimed 容器。因为 Runtime 管理
  员也可以选择 `spec.daemonImage`，只有受信的管理员才能配置 Runtime 对象。
- 在允许用户向自定义 Runtime 提交 Runs 之前，审查其 service account、卷、Secrets、
  网络访问和节点放置。
