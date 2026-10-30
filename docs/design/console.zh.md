# Kruntimes Console

Kruntimes Console 是始终安装的浏览器 UI 与只读 API。它合并浏览器 console、Kubernetes
资源查询、Run 日志，以及为 Run data-plane 操作提供的"只转发凭据"的代理。Console 不再是
data plane：它自身不做 Runtime 操作授权，也不会直接连接 Runtime Service。

```
browser / krt / SDK
        |
        v
Kubernetes API server ---> Run extension API ---> runtimed (owner Runtime Pod)
        ^
        |  (仅浏览器，同源代理)
Kruntimes Console (HTTPS)
  |- /api/...  Console 资源与日志 API
  |- /api/.../session  代理的 Session WebSocket
```

## Data plane

`run-extension-api` 中的 Kubernetes 聚合 Run extension API 拥有 Run data plane：Run logs、
Session operation 与文件、Function invocation。kube-apiserver 使用普通 kubeconfig 机制
认证每个 caller，并在转发到 backend 前授权对应的 Run subresource。Console 从不为
data-plane 请求执行 TokenReview/SubjectAccessReview，不直连 Runtime Service，也没有
Runtime Pod 的网络访问权限。

Console 的 Service 不再写入 `Run.status.endpoint`；该字段对 Function 与 Session Run 已
废弃。client 从自己的 Kubernetes REST configuration 推导聚合 API 地址。

## 认证

Console 接受普通 `Authorization: Bearer ...`。浏览器登录会把输入的 bearer token 放入
host-only、`HttpOnly`、`Secure`、`SameSite=Strict` 的 Console session cookie，JavaScript
永远不会取得凭据。`krt` 与 SDK 显式提供的 Authorization header 始终优先。Console 只用
`TokenReview` 验证登录凭据并显示 Kubernetes username，不做授权决策。

## 浏览器中的 Session operation

Ready Session Run 的详情页提供 shell command 面板。Console 使用 caller 的 Kubernetes
credential，把同源的 Session WebSocket 转发到 Kubernetes 聚合 Session API：
`/apis/sessions.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/session`。
kube-apiserver 负责认证 caller 并授权 `sessions.kruntimes.io/runs/session` 的 `get`；
Console 自身不做 Session operation 的授权决策。面板将 command 作为第一个 client frame
发送，并在到达时按顺序渲染 accepted、output、progress 与 terminal event。协议中 JSON
base64 representation 的二进制输出会先解码再显示。

Session Run 进入 `Ready` 前，该面板保持禁用。Cancel 控件会通过 WebSocket 发送
`{"type":"cancel","operationID":"..."}`。Console client 在连接打开时发送
`{"type":"send","operation":...}`，并在 Run 配置 lease 时，只要 connection 存在就私下发送
heartbeat frame。关闭 connection 不会 release backing Session Run。传输错误或连接在 terminal event 前关闭时会显示在面板内。选择
**Run again** 始终会创建新的 operation；当前 WebSocket protocol 没有按 operation ID
恢复的 client frame，也没有单独的 NDJSON fallback。

## Helm 配置

Console 始终安装。使用 `console.image`、`console.replicas`、`console.publicRead`、
`console.tls` 以及标准 pod scheduling/security values 配置。`console.tls` 支持 chart
自签、已有 Secret 和 cert-manager。Runtime access 限额、authorization cache 与
client-certificate 选项已随内嵌 Runtime gateway 一起删除。
