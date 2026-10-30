# Runtime Gateway 传输安全与传输边界

状态：**已废弃。** 共享 Runtime gateway 已删除；Runtime access 现在通过聚合 Run extension API 完成，Console 不再提供 data plane。本文仅保留作历史参考。

## 问题

共享 Runtime gateway 当前提供已认证但明文的集群内 HTTP endpoint。`Run.status.endpoint`
支持 `HTTP`、`HTTPS` 和可选 PEM trust bundle，但 chart 尚未定义证书挂载、HTTPS 选择或
trust material 发布的约定。gateway 也依赖实现内固定的 JSON request body 和部分 response
大小限制，而非一个小而可由 operator 配置的策略。

本文定义渐进式 v0.x transport contract；它不会把 gateway 变为 Internet-facing ingress 或证书
颁发机构。

## 目标与非目标

- 保持 gateway Service 为 cluster-local，并保留 Kubernetes bearer-token authentication 和按 Run authorization。
- 将 listener protocols 定义为显式集合。operator 选择 `http`、`https` 或两者。
- 默认由 chart 生成并保留 gateway TLS Secret，同时允许 operator-provided Secret 和后续
  cert-manager producer。
- 以保守默认值显式配置 request/response bounds。
- 默认继续兼容当前 plain-HTTP v0.x deployment。

不创建 Ingress、Gateway、LoadBalancer 或外部 DNS；Runtime controller 不签发证书，也不向
Run status 暴露 private key；不自动把 HTTP 安装切换为 HTTPS，更不公开 Runtime Server gRPC。

## Helm API

```yaml
gateway:
  # 至少一个去重 value；HTTPS 需要证书。两者同时存在时 status 发布 HTTPS endpoint。
  protocols: [http] # http, https
  httpBindAddress: ":8084"
  httpsBindAddress: ":8444"
  httpServicePort: 80
  httpsServicePort: 443
  maxRequestBodyBytes: 1048576
  maxResponseBodyBytes: 1048576
  maxHeaderBytes: 1048576
  tls:
    # 空值选择 chart-managed <gateway-name>-tls Secret；非空值选择 existing
    # operator-managed Secret，除非启用 certManager。
    secretName: ""
    # HTTPS endpoint trust 需要 certificate、private key 和 CA bundle key。
    certificateKey: tls.crt
    privateKeyKey: tls.key
    caBundleKey: ca.crt
    certManager:
      enabled: false
      issuerRef:
        name: ""
        kind: Issuer
        group: cert-manager.io
```

`protocols: [http]` 只启动 HTTP listener 并发布 `http://` endpoint；
`protocols: [https]` 只启动 TLS listener 并发布 `https://`；
`protocols: [http, https]` 同时启动两者，并暴露命名为 `http`、`https` 的 Service ports。为避免
downgrade new client，同时启用时只在 `Run.status.endpoint` 发布 HTTPS URL；等价 HTTP route 在
operator 控制的迁移期内仍可用。

选择 `https` 时，空 `secretName` 选择 chart-managed `<gateway-name>-tls` Secret。
Helm 通过 `lookup` 在 upgrade 时保留已有 Secret，只有不存在时才生成 CA 与 Service-DNS
certificate。非空 `secretName` 使用 existing operator-managed Secret。两种方式都只读挂载 key，
gateway 在 certificate/private-key 任一缺失时拒绝提供 TLS；controller 还需要配置的 CA bundle key
以发布可验证的 HTTPS endpoint。

启用 cert-manager 时，chart 创建一个 namespaced `cert-manager.io/v1 Certificate`，其
`secretName` 为 `gateway.tls.secretName`，DNS names 包含 gateway Service 的 short、
namespace-qualified 和 cluster-local 名称。缺少 secret name 或 issuer reference 必须令 Helm
rendering 失败。所选 issuer 还必须向 Secret 写入配置的 CA bundle key，以便 HTTPS client
验证 `Run.status.endpoint.caBundle`。此资源完全 opt-in，没有 cert-manager CRD 的安装不需要它。

chart 负责默认 Secret、Deployment、Service 与可选 Certificate；Runtime controller 不负责证书。
修改 protocols 会滚动 gateway Pod 并更新 controller flag；已经 Ready 的 Run 在 owner
runtimed reconcile 前仍保留旧 endpoint。operator 只能在 HTTP client 都迁移后删除 `http`。

## Endpoint 信任发布

controller 接收 selected public gateway URL 和不超过 64 KiB 的 CA bundle，绝不接收 private key。Helm 将
chart-managed CA 或 existing Secret 中必需的 `ca.crt` key 以只读文件挂载到 controller，
再传入文件路径。controller 将该 public trust bundle 写入 Runtime Pod-template annotation；
runtimed 通过 Downward API 将 annotation 投影为文件，并复制到 HTTPS
`Run.status.endpoint.caBundle`。controller 不获得 Secret-read RBAC；runtimed 不挂载 TLS Secret。

## 传输边界

| Value | 默认值 | 执行方式 |
| --- | ---: | --- |
| `gateway.maxRequestBodyBytes` | 1 MiB | JSON request 大于此值即以 `413 Payload Too Large` 拒绝，不进入 runtimed |
| `gateway.maxResponseBodyBytes` | 1 MiB | 先缓冲 JSON response；超限在写 header 前返回 `413`，绝不输出 partial JSON |
| `gateway.maxHeaderBytes` | 1 MiB | Go HTTP server 在路由前拒绝超大 request headers |

所有 value 必须为正数，并以显式 gateway flags 传入。response limit 仅作用于 gateway 生成的
JSON；Runtime Server 仍各自限制 direct gRPC output/file，session paging 仍负责目录列举边界。
较大的持久结果应进入 ArtifactStore。

## 安全、失败与交付

TLS 仅保护 client-to-gateway；同时启用两种 protocol 时 HTTP 是刻意的兼容 listener，并非 confidential
boundary。内部 gRPC 仍是受 NetworkPolicy 和认证 gateway boundary 保护的 cluster-local hop。
缺失、不可读、格式错误或不匹配的 TLS Secret 必须使 gateway Pod 不 Ready，不能降级到 HTTP。
key 仅只读挂载到 gateway Pod，绝不进入日志、ConfigMap、Run status 或 flags。

交付分为可独立 review 的三项：

1. protocol-set listener 与 Service configuration、chart-managed/existing Secret selection、endpoint CA
   publication、Helm/unit coverage，以及同时验证两种协议的 E2E；
2. Opt-in cert-manager `Certificate` rendering 与 chart tests；
3. 可配置 HTTP body/header bounds、gateway unit tests，以及 focused rejection E2E。
