# Dashboard

本文描述已接受的 v0.x 设计以及已实现的初始 Dashboard 功能。

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
- 浏览 Runtime pool、其 Pod health/capacity 以及分配给它的 Runs。
- 浏览 WorkflowRun、其 job DAG 和 step 到 Run 的链接。

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

### v0.x 决策

dashboard 是 `kruntimes` chart 的 opt-in 组件，默认 `dashboard.enabled: false`。其
Deployment、ServiceAccount、Service 和 TLS resources 与控制面一起由同一个 Helm release 安装。
这样在不增加不需要该组件的安装面时，仍保持统一的升级和 RBAC 边界。

生产环境的 dashboard 仅允许 HTTPS。Service 保持 ClusterIP，且 bearer-token login 页面绝不能
暴露在 plaintext HTTP 上。chart 允许 operator 选择一种 certificate source：

- 已存在的 TLS Secret，通常包含 dashboard 用户信任的 certificate；
- chart 生成的 self-signed certificate，适用于本地开发和已显式信任该 certificate 的私有部署；
  或
- 使用已有 Issuer 或 ClusterIssuer 的 cert-manager Certificate；所选 issuer 本身也可以是
  cert-manager self-signed issuer。

所选 source 都写入同一个挂载的 TLS Secret。chart 必须拒绝 ambiguous combination，而不能
静默选择 certificate source。

Helm values 将该选择明确化：默认 `dashboard.tls.selfSigned` 会让 chart 创建 TLS Secret；要
挂载 operator 已提供的 Secret，设置 `selfSigned: false`，保持
`certManager.enabled: false`，并设置 `secretName`；要使用 cert-manager，则设置
`selfSigned: false` 和 `certManager.enabled: true`，同时引用已经存在的 `issuerRef`。
cert-manager 可以写入默认 Dashboard TLS Secret，也可以写入 operator 设置的 `secretName`。
因此，使用已有 self-signed Issuer 时不需要额外的 dashboard 专用 mode。

backend 不拥有代表用户读取受保护资源的 ambient authority。它只复制 in-cluster transport
配置、清空挂载的 credential，并安装 caller bearer token。chart 默认启用极窄的 public-read：
Dashboard ServiceAccount 只能 get/list Namespaces、Runs、Runtimes 和 WorkflowRuns，且无 token
时 API 只暴露它们的 summary。operator 可以通过 `dashboard.publicRead.enabled=false` 禁用它。
资源详情仍由 caller 授权。

第一版应读取以下数据源：

- 通过 Kubernetes API 读取 `Run` objects；
- 读取 `Run.status.assignedPod` 引用的 Runtime Pod metadata；
- 在可用时读取与 Runs 和 Runtime Pods 相关的 Kubernetes Events；
- 通过 backend-controlled 路径访问 runtimed log/status endpoints；
- 读取 `Run.status.outputs` 和 `Run.status.artifactRefs`。

后续版本可以增加 PersistentWorkspace detail pages 以及基于 Prometheus 或其它 metrics backend
的 metrics panels。

## 日志访问

Dashboard backend 不能把 Runtime Pods 直接暴露给浏览器。

v0.x 已实现的路径是：

1. 用户打开某个 Run 的 logs。
2. Dashboard backend 只将 caller token 转发给 Kubernetes 聚合 Run-log API。
3. API server 对 exact `logs.kruntimes.io/runs/log` subresource 完成 authentication 和
   `get` authorization。
4. 聚合 backend 定位 assigned Runtime Pod，并用自身极窄的 `pods/log` permission 读取其
   `runtimed` log。
5. backend stream 或返回按 UID 过滤后的 log records。

结构化 runtimed logs 应继续以 Run UID 作为 key，这样即使 Runtime Pods 同时处理多个 Runs，
dashboard 也能展示正确的 logs。

Dashboard 使用 in-cluster Kubernetes API Service，不创建 browser-visible port-forward，也不把
Runtime Pods 直接暴露给 browser。caller 需要 exact `runs/log` subresource 的 `get`，而不是
`get pods/log`；Log API ServiceAccount 执行极窄的 Pod-log read。artifact references 作为 Run
metadata 展示；artifact download 不属于第一阶段 Dashboard。完整的 endpoint、authorization、
bounds、error 和 migration contract 见[聚合 Run Log API 设计](runtime-gateway-log-api.zh.md)。

## 安全模型

dashboard 默认必须是只读的。

建议的 v0.x 生产模型是 Kubernetes bearer-token login：

- 用户通过 HTTPS 将 Kubernetes bearer token 输入 Dashboard。backend 以 host-only 的
  `HttpOnly`、`Secure`、`SameSite=Strict` session cookie 返回 token，时限八小时。JavaScript
  永远不读取或写入 token，且 token 不会写入 localStorage、sessionStorage 或 logs；
- backend 使用该 bearer token、in-cluster API server 地址和 cluster CA 创建 request-scoped
  Kubernetes client，并用它访问受保护页面和聚合 Run logs；
- chart 默认以权限极窄的 Dashboard ServiceAccount 提供免 token 的 namespace、Run、Runtime 和
  WorkflowRun summary。它只有这些资源的 `get`/`list` 权限，并可以显式禁用；
- Kubernetes API authorization 决定受保护页面的访问。token 需要 exact
  `logs.kruntimes.io/runs/log` subresource 的 `get` 才能读取 logs；
- v0.x 只将 artifact references 作为 Run metadata 展示，不下载或代理 artifact content。
  将来的 artifact-download 设计必须单独定义 authorization 与 external-store 边界；
- 默认隐藏 secrets、service account tokens、environment variables 和 raw pod specs，
  除非未来明确增加 privileged operator view。

这与 Kubernetes Dashboard token login 的初始用户体验一致。集群 identity integration 可以在
dashboard 外部 mint 或 exchange bearer token，但 v0.x 不定义 external-auth header protocol、
impersonation model 或 custom identity provider。

本地开发中，`krt dashboard` 启动 loopback-only proxy 并 port-forward dashboard Service。
proxy 获取当前 kubeconfig credential，只将其注入被转发的请求；browser 永远不会得到该
credential。它只能绑定 127.0.0.1 或显式选择的 loopback address、必须拒绝 non-loopback
bind、不持久化或记录 credential，并在命令退出时关闭 port-forward。这不是生产 authentication
mode。

### 创建 Dashboard 登录 Token

operator 应在每个允许 dashboard 用户查看的 namespace 中，为最小权限的 *viewer*
ServiceAccount 创建短期 token。该 ServiceAccount 是登录 token 所代表的用户身份，与 dashboard
Deployment 自身使用的 ServiceAccount 不同。以下示例授予单个 namespace 的只读 Run、Runtime、
Workflow 和日志访问，不授予 Secrets、workload mutation verb、port-forwarding 或 artifact
download：

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

应用该 manifest 后，生成有时限的 token，并将其粘贴到 dashboard 登录页面。Dashboard 会将它保存
在八小时 HTTPS-only HttpOnly session cookie 中：

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

已实现 endpoints 为：

```text
GET /api/namespaces
GET /api/namespaces/{namespace}/runs
GET /api/namespaces/{namespace}/runs/{name}
GET /api/namespaces/{namespace}/runs/{name}/logs?tail=&follow=
GET /api/namespaces/{namespace}/runtimes
GET /api/namespaces/{namespace}/runtimes/{name}
GET /api/namespaces/{namespace}/workflowruns
GET /api/namespaces/{namespace}/workflowruns/{name}
POST /api/session
DELETE /api/session
```

日志 endpoint 只会通过 request-scoped Kubernetes client 读取 assigned Pod 的 `runtimed`
container，并丢弃 `run_uid` 不匹配该 Run immutable UID 的记录。`tail` 最多返回 500 条记录；
每个 request 最多读取 500 条最近 container lines 和 1 MiB。普通 tail response 为 JSON；当
`follow=true` 时，endpoint 会返回过滤后的 newline-delimited JSON records，直到 caller 断开或
Kubernetes log stream 关闭。它绝不会变成 browser-visible Pod proxy。

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

WorkflowRun detail page 会将 `spec.jobs[*].needs` 渲染为只读的 GitHub Actions-style staged DAG：
root jobs 位于最左 stage，每条 dependency 都会把 consumer 放到更靠后的 stage，SVG edges 会清晰连接
dependency 与 consumer。一个 stage 会在同一个 card 中聚合 parallel job rows；有多个 dependencies 的
node 会可见地汇合 incoming edges。每个 row 展示 observed phase，并在存在时展示有界的
`status.jobs[*].outputs.result`。

选择 job 会进入可 bookmark 的 frontend route：
`/namespaces/{namespace}/workflowruns/{workflowrun}/jobs/{job}`。该 detail page 提供 all-jobs
navigation rail 和可展开的 step list。打开具有 child Run 的 step 时，会自动请求已有的 Run-log endpoint；
browser 永远不会得到 Pod endpoint 或 Kubernetes credential。graph 和 detail pages 都只是 declared
execution DAG 的视图，不能提供 mutation 或 graph-editing controls；在窄屏幕上 graph 可以水平滚动，不能
丢失 dependency information。视口支持从非 Job canvas 区域 click-and-drag 向任意方向平移；Job node
仍是普通链接，同时保留 scrollbar 和 mouse wheel 作为替代操作。

在只读授权模型被验证之前，不应加入 mutation buttons。

Settings 页面承载全局 Style 和 Theme 选择器，左侧导航提供可直接访问的 `/settings` 链接。
Style 提供 GitHub（默认）、Stripe-inspired 和 Neumorphism，与 Light、Dark、System 的
Theme 选择器相互独立。GitHub 使用克制的 GitHub Primer Light/Dark 运维配色：
`#f6f8fa` 背景、`#1f2328` 文字、`#d0d7de` 边框、蓝色链接与焦点、紧凑的 6px 圆角，
以及绿色主操作；它不使用装饰性网格和 hover 上浮。Stripe-inspired 使用细边框、轻阴影、
小圆角及克制的紫色强调；Neumorphism 保留同材质表面和凸起／凹陷双阴影。三套风格覆盖
资源页面、DAG、Job Step 和日志，不复制页面组件，也不改变路由、依赖或自动加载日志。
侧栏将 namespace-scoped 的 Runs、Runtimes 和 Workflow Runs 归入 `Kruntimes resources`，
Settings 和 About 归入独立的 `Dashboard` 组。每个导航项在文字标签之外提供小型语义 SVG 图标。
图标使用当前风格的 token：GitHub 与 Stripe-inspired 保持平面线条图标，Neumorphism 使用凸起
或选中时凹陷的同材质图标底座。
两个偏好分别保存在浏览器 localStorage，在 React 渲染前应用。无效风格回退到
GitHub，无效主题回退到 System；存储被禁用时仍可在当前页面切换。
System 跟随浏览器的颜色偏好。密集表格行、状态标记和日志保持清晰可读，保留
可见键盘焦点及减少动态效果支持。不新增 Helm 配置或后端 API。
Stripe 的 Dashboard 适配使用精确浅色品牌色（`#635bff`、`#0a2540`、`#f6f9fc`）、
40px 背景网格、多层面板阴影、12px 面板圆角和 8px 控件圆角。保留紧凑运维排版，
不照搬营销页大标题与留白；说明正文限制为 75ch。主按钮保持精确品牌紫，浅色模式中
有底色区域的链接使用 `#554bd6` 保证文字对比度。Connect 使用紫色主按钮，次按钮和
图标按钮保持弱化表面。按钮 hover 上浮 2px，按下缩放至 0.98 并只保留内阴影，
统一使用 300ms ease-out；减少动态效果时禁用变换与过渡。DAG、表格、日志不随 hover
移动。内嵌表格平面化，状态标签使用小圆角；圆形状态图标和可读的暗色配色作为
StyleKit 的明确例外。仅 Stripe 使用不显式指定 Inter 的系统字体。这些是明确的
Dashboard 适配，并非宣称逐字符合存在冲突的原始提示词。
验证和扩展风格的方法见 `dashboard/frontend/tests/README.md`。

frontend 使用 React 和 TypeScript，构建为与 Dashboard backend 一同打包到镜像中的静态 assets，并与内部 API 从
同一 HTTPS origin 提供。source、backend、process entrypoint 和 image definition 都位于顶层
`dashboard/` 目录。它没有独立 frontend Service、没有 browser-to-Kubernetes connection，并使用
same-origin Content Security Policy。bearer token 只会保存在 HTTPS-only HttpOnly cookie；刷新可
恢复 session，Disconnect 会清除它。

## 实现顺序

1. 增加本文档，并在 roadmap 中保持 TODO 明确。
2. 增加 dashboard backend package，接入只读 Kubernetes client。
3. 实现已 review 的 bearer-token production mode，以及 local-only kubeconfig proxy mode。
4. 实现 Run list/detail APIs，并增加 unit tests。
5. 通过 backend-controlled 路径实现 log tail/follow。
6. 增加 frontend Run list/detail/log views。
7. 在 `kruntimes` chart 中增加可选的 `dashboard.enabled` resources。
8. 在标准 E2E environment 中部署 Dashboard。browser-specific E2E coverage 延后到具备稳定的
   browser test harness 时再增加。
9. 在相关 APIs 稳定后增加 WorkflowRun/Workflow/Action/PersistentWorkspace views。

## 剩余问题

- log access 是否继续使用 port-forward 语义，还是迁移到专用的 cluster-internal log proxy
  service？
- 当 artifact stores 位于集群外部时，artifact downloads 应如何授权和代理？
- 第一版 list/watch 实现应该支持怎样的规模目标？
