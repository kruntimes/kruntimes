# Dashboard

本文描述 v0.x 的目标设计，当前尚未实现。

kruntimes 应该提供一个小型只读 dashboard，帮助开发者和运维人员理解当前有哪些任务在运行、
哪些任务卡住了，以及如何找到 logs 和 artifacts，而不需要在多个 `kubectl` 和 `krt`
命令之间来回切换。

dashboard 不应该成为 workflow engine，也不应该成为新的主控制面。它应该展示已经存在于
CRD、Pod、conditions、logs 和 artifact references 中的 Kubernetes-native 状态。

## 目标

- 按 namespace 浏览 Runs。
- 查看 Run phase、conditions、runtime、assigned Runtime Pod、attempts、时间戳、
  有界 outputs 和 artifact references。
- 通过与 `krt logs` 相同的安全边界 stream 或 retrieve Run logs。
- 保持 Kubernetes RBAC 和 namespace 边界。
- 为 Pending、Scheduled、Running、Succeeded、Failed、Cancelled 和 TimedOut Runs
  提供面向运维的视图。
- 为后续 WorkflowRun、Workflow、Action 和 PersistentWorkspace 视图预留空间。

## 非目标

- 第一版不提供 create、cancel、delete、retry 或 edit 操作。
- 第一版不提供 workflow editor 或 visual DAG builder。
- 不允许浏览器直接访问 Runtime Pods、Runtime Servers 或 runtimed endpoints。
- 不引入绕过 Kubernetes authentication 和 authorization 的自定义身份系统。
- 不替代 Prometheus、log collection 或长期 audit storage。
- v0.x 不承诺稳定的公开 dashboard HTTP API。

## 用户

开发者通过 dashboard 回答：

- 我的 Run 是否启动了；
- 哪个 Runtime 处理了它；
- 为什么它 Pending 或 Failed；
- logs 和 bounded outputs 是什么；
- artifacts 存储在哪里。

运维人员通过 dashboard 回答：

- 哪些 namespace 里有卡住或失败的 Runs；
- capacity、readiness、RBAC 或 image/runtime 问题是否体现在 Run conditions 中；
- 哪些 Runtime Pods 正在接收任务；
- 用户是否需要额外 RBAC 才能读取 logs 或 artifacts。

## 架构

dashboard 应该包含两个组件：

| 组件 | 作用 |
| --- | --- |
| Dashboard backend | 访问 Kubernetes API，执行所选 auth/RBAC 模型，读取 kruntimes CRDs，并在允许时代理 logs/artifacts 访问。 |
| Dashboard frontend | 只读 Web UI，展示 namespace、Run list、Run detail、logs 和 artifact metadata。 |

第一版应读取以下数据源：

- 通过 Kubernetes API 读取 `Run` objects；
- 读取 `Run.status.assignedPod` 引用的 Runtime Pod metadata；
- 在可用时读取与 Runs 和 Runtime Pods 相关的 Kubernetes Events；
- 通过 backend-controlled 路径访问 runtimed log/status endpoints；
- 读取 `Run.status.outputs` 和 `Run.status.artifactRefs`。

后续版本可以增加：

- `WorkflowRun`、`Workflow` 和 `Action` list/detail pages；
- PersistentWorkspace detail pages；
- runtime pool capacity 和 health views；
- 基于 Prometheus 或其他 metrics backend 的 metrics panels。

## 日志访问

Dashboard backend 不能把 Runtime Pods 直接暴露给浏览器。

v0.x 预期路径是：

1. 用户打开某个 Run 的 logs。
2. Backend 读取该 Run 以及它 assigned 的 Runtime Pod。
3. Backend 使用配置好的 Kubernetes auth/RBAC 模型校验请求。
4. Backend 通过与 `krt logs` 概念上相同的边界访问 runtimed，并 stream 或返回请求的
   log tail。

具体 transport 可以演进。它可以使用 Kubernetes port-forwarding、internal service 或
专用 log proxy，但边界应该保持不变：用户需要有读取 Run 以及访问 runtime logs 的权限。

结构化 runtimed logs 应继续以 Run UID 作为 key，这样即使 Runtime Pods 同时处理多个 Runs，
dashboard 也能展示正确的 logs。

## 安全模型

dashboard 默认必须是只读的。

建议的 v0.x 生产模型是 Kubernetes bearer-token login：

- 用户将 Kubernetes bearer token 输入 dashboard。浏览器仅在内存中保存 token，并只通过
  dashboard 的 HTTPS origin 使用 `Authorization: Bearer` header 发送；不得写入
  localStorage、sessionStorage、cookie 或磁盘。backend 不创建 dashboard 专用 identity 或
  session，也不得持久化或记录该 token，包括 HTTP access logs；
- backend 使用该 bearer token、in-cluster API server 地址和 cluster CA 创建 request-scoped
  Kubernetes client。它绝不使用 dashboard ServiceAccount 代表用户读取资源；
- namespace 可见性和读取权限由 Kubernetes API authorization 决定，而不是 dashboard
  自己维护 policy；
- 初始 UI 可以 best-effort 列出 namespaces。若 token 没有 list Namespace objects 的权限，
  UI 必须允许用户输入 namespace name，并展示 API 的正常 authorization 结果；
- logs 访问需要同一个 token 能够读取 Run 及其 assigned Pod、创建 `krt logs` 所使用的 Pod
  `portforward` subresource，并在 runtimed log fallback 需要时读取 `log` subresource；
- artifacts 访问需要 Run read permission；当 dashboard 通过 runtimed 的 artifact endpoint
  访问时，还需要读取 assigned Pod 并创建其 `portforward` subresource 的权限。直接访问
  artifact-store 时也需要选定 backend 所定义的权限；
- 默认隐藏 secrets、service account tokens、environment variables 和 raw pod specs，
  除非未来明确增加 privileged operator view。

这与 Kubernetes Dashboard token login 的初始用户体验一致。集群 identity integration 可以在
dashboard 外部 mint 或 exchange bearer token，但 v0.x 不定义 external-auth header protocol、
impersonation model 或 custom identity provider。

本地开发中，`krt` 可以 port-forward dashboard，并通过 local-only proxy 提供当前 kubeconfig
credential。这个 convenience path 不是生产 authentication mode，并且不能让浏览器在本地会话结束
后保留 kubeconfig credential 或 token。

### 创建 Dashboard 登录 Token

operator 应在每个允许 dashboard 用户查看的 namespace 中，为最小权限的 *viewer*
ServiceAccount 创建短期 token。该 ServiceAccount 是登录 token 所代表的用户身份，与 dashboard
Deployment 自身使用的 ServiceAccount 不同。以下示例授予单个 namespace 的只读 Run、Runtime、
Workflow、日志和 artifact 访问，不授予 Secrets 或 workload mutation verb；通过 runtimed
读取日志和下载 artifact 所需的 `pods/portforward` `create` subresource permission 是唯一例外：

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kruntimes-dashboard-viewer
  namespace: team-a
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kruntimes-dashboard-viewer
  namespace: team-a
rules:
  - apiGroups: ["kruntimes.io"]
    resources: ["runs", "runtimes", "workflowruns", "workflows", "actions", "persistentworkspaces"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kruntimes-dashboard-viewer
  namespace: team-a
subjects:
  - kind: ServiceAccount
    name: kruntimes-dashboard-viewer
    namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kruntimes-dashboard-viewer
```

应用该 manifest 后，生成有时限的 token，并将其粘贴到 dashboard 登录页面。dashboard 只在当前
浏览器内存 session 中保留该 token：

```bash
kubectl apply -f dashboard-viewer.yaml
kubectl -n team-a create token kruntimes-dashboard-viewer --duration=1h
```

`kubectl create token` 要求 Kubernetes 1.24 或更高版本。日常 dashboard 访问不要使用
cluster-admin credential。cluster identity system 也可以提供等价 user token；dashboard 会将两者
都视为标准 Kubernetes bearer token。若要浏览多个 namespaces，可以创建等价的 namespace-scoped
bindings，或者在审查其范围后显式授予额外的 cluster-level read access。

## 内部 API 形状

dashboard frontend 可以使用随二进制版本演进的内部 HTTP API。v0.x 不应把它文档化为稳定的
公开 API。

初始 endpoints 可以是：

```text
GET /api/namespaces
GET /api/namespaces/{namespace}/runs
GET /api/namespaces/{namespace}/runs/{name}
GET /api/namespaces/{namespace}/runs/{name}/logs?tail=&follow=
```

Run list endpoint 应尽量支持 server-side pagination 和过滤：

- phase；
- runtime；
- assigned pod；
- label selector；
- created-after 或 age window。

## 用户界面

第一版 UI 应保持聚焦、面向运维：

- namespace selector；
- Run table，包含 phase、runtime、assigned pod、age、attempts 和 last transition reason；
- phase 和 runtime filters；
- Run detail page 或 drawer；
- conditions timeline；
- bounded outputs 和 artifact references；
- logs panel，包含 tail 和 follow controls；
- 当用户有权限时，链接到相关 Runtime Pod metadata。

在只读授权模型被验证之前，不应加入 mutation buttons。

## 实现顺序

1. 增加本文档，并在 roadmap 中保持 TODO 明确。
2. 增加 dashboard backend package，接入只读 Kubernetes client。
3. 实现已 review 的 bearer-token production mode，以及 local-only kubeconfig proxy mode。
4. 实现 Run list/detail APIs，并增加 unit tests。
5. 通过 backend-controlled 路径实现 log tail/follow。
6. 增加 frontend Run list/detail/log views。
7. 增加可选 Helm chart value 或独立 dashboard chart。
8. 增加 E2E smoke coverage：安装 dashboard、创建 Run、列出 Run、打开 detail、读取 logs。
9. 在相关 APIs 稳定后增加 WorkflowRun/Workflow/Action/PersistentWorkspace views。

## 剩余问题

- dashboard 应该放在主 kruntimes chart、独立 chart，还是两者都支持？
- log access 是否继续使用 port-forward 语义，还是迁移到专用的 cluster-internal log proxy
  service？
- 当 artifact stores 位于集群外部时，artifact downloads 应如何授权和代理？
- 第一版 list/watch 实现应该支持怎样的规模目标？
