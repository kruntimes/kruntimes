# Kruntimes Console

Kruntimes Console 是平台 chart 安装的唯一公共访问组件。它合并浏览器 UI、Kubernetes
资源查询、Run 日志与版本化 Runtime access API；不再部署独立的 Runtime Gateway。

```
browser / krt / SDK
        |
        v
Kruntimes Console (HTTPS)
  |- /api/...  Console 资源与日志 API
  |- /v1/...   Runtime access handler
        |
        v
选中 Runtime Pod 内的 runtimed
```

## Runtime access

`internal/gateway.Server` 仍是 `/v1/` 的协议适配器，但仅作为 Console 注册的内部
`http.Handler`。它没有 listener、Deployment、Service、TLS 配置或独立生命周期。它解析
Ready Run，执行 Kubernetes TokenReview 与 SubjectAccessReview，再转发到 owner Runtime
Service。

Controller 将 Console Service URL 写入 Ready Function/Session Run 的
`status.endpoint`，并把 Console CA bundle 写入 endpoint。Runtime Pod 通过已有的
downward API 路径挂载 CA，因此 runtimed 能验证 Console HTTPS Service。

## 认证

Console 接受普通 `Authorization: Bearer ...`；配置
`console.tls.clientCASecretName` 后，也接受经验证的 Kubernetes client certificate。
浏览器登录会把输入的 bearer token 放入 host-only、`HttpOnly`、`Secure`、
`SameSite=Strict` 的 Console session cookie。浏览器请求 `/v1/` 时，Console 在服务端
将 cookie token 注入内部 handler，JavaScript 不会取得凭据。`krt` 与 SDK 显式提供的
Authorization header 始终优先。

## 浏览器中的 Session operation

Ready Session Run 的详情页提供 shell command 面板。它建立同源、已认证的
`operations:ws` 连接，将 command 作为第一个 client frame 发送，并在到达时按顺序渲染
accepted、output、progress 与 terminal event。协议中 JSON base64 representation 的二进制
输出会先解码再显示。

Session Run 进入 `Ready` 前，该面板保持禁用。Cancel 控件会通过 WebSocket 发送
`{"type":"cancel","operationID":"..."}`。Console client 在连接打开时发送
`{"type":"send","operation":...}`，并在 Run 配置 lease 时，只要 connection 存在就私下发送
heartbeat frame。关闭 connection 不会 release backing Session Run。传输错误或连接在 terminal event 前关闭时会显示在面板内。选择
**Run again** 始终会创建新的 operation；当前 WebSocket protocol 没有按 operation ID
恢复的 client frame。需要 resumability 的 client 可以使用现有 NDJSON stream endpoint 与
`after` query parameter。

## Helm 配置

Console 始终安装。使用 `console.image`、`console.replicas`、`console.publicRead`、
`console.tls` 与 `console.access` 配置。`console.tls` 支持 chart 自签、已有 Secret 和
cert-manager。用于 Runtime endpoint 的 Secret 除证书与私钥外，必须含有
`console.tls.caBundleKey`（默认 `ca.crt`）。`console.access` 包含原独立 Gateway 的
authorization cache 与有界 request/response/header 设置。
