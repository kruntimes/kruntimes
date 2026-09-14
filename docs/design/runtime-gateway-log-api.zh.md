# 聚合 Run Log API

## 状态

v0.x 已实现。Kubernetes 聚合 API 是唯一的 Run-log transport。Runtime Gateway 只提供
Session 和 Function operation。

## 问题

Run 是逻辑执行单元，但多个 Run 可以共享一个 Runtime Pod。`runtimed` container 输出以 Run UID
为 key 的 structured records。若直接授予用户 `pods/log`，会暴露无关执行并要求 Pod discovery，
因此 Dashboard 和 `krt logs` 需要 Run-scoped API。

## API 与授权

chart 安装 `logs.kruntimes.io/v1alpha1` 的 `APIService`，并提供 namespaced subresource：

```
GET /apis/logs.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/log
```

Kubernetes 用正常 kubeconfig 机制完成 caller authentication，包括 bearer token、exec credential
和 client certificate。转发到 backend 前，API server 授权：

```yaml
apiGroups: ["logs.kruntimes.io"]
resources: ["runs/log"]
verbs: ["get"]
resourceNames: ["<run-name>"]
```

backend 只在 aggregation layer 已验证的 request-header mTLS 连接上接受 identity headers。它不再
重复执行 `runs get` SubjectAccessReview：`runs/log` authorization 就是 API contract。

backend 的 ServiceAccount 只拥有打开 assigned Runtime Pod 的 `runtimed` log 所需的
`get pods/log` 权限。它会按 target Run UID 过滤 records，绝不直接向 caller 暴露 Pod log stream。

## Response 与 streaming

snapshot 返回 JSON：

```json
{"items":[{"stream":"stdout","message":"..."}],"cursor":"..."}
```

`tailLines` 默认 100，最大 500；snapshot payload 最大 1 MiB。`follow=true` 返回
`application/x-ndjson`，每行带 opaque cursor。reconnect 时传递 `cursor`，从最后一条已交付记录
之后继续。backend 在写 response headers 前打开 Pod log stream，因此 log service 不可用仍是普通
HTTP error。
所有 non-success response 都使用 Kubernetes `metav1.Status` 格式，保留 HTTP code、reason
和可操作的 message，供 `krt` 与 Dashboard 展示。特别地，`409 Conflict` 表示该 Run 分配的
Runtime Pod 已不存在，因此其 ephemeral container logs 无法恢复。

Go caller 使用与 client-go log request 类似的 `internal/logapi.Client`：

```go
request := logs.GetLogs(namespace, runName, &logapi.RunLogOptions{Follow: true})
stream, err := request.Stream(ctx)

raw, err := logs.GetLogs(namespace, runName, options).Do(ctx).Raw()
```

## Consumers

`krt logs` 始终使用当前 kubeconfig 的 Kubernetes API endpoint，不再有 Gateway URL、Gateway CA 或
跳过 TLS verification 的 flags。Dashboard 将 login token 保存在 HttpOnly cookie 中，并只在 server
side 转发给聚合 API。两个 client 读取日志均不需要 `runs get`、Pod discovery、`pods/log` 或
Pod port-forward 权限。

Runtime Gateway 没有 Run-log route，也没有 `pods/log` RBAC；其 Session 和 Function authorization
boundary 与 Kubernetes aggregation boundary 保持分离。

## 安装

`logAPI.enabled` 默认是 true，且独立于 `gateway.enabled`。APIService 是 cluster-scoped，因此同一
集群中一个 group/version 必须恰好由一个 platform release 拥有；其它 release 设置
`logAPI.enabled: false`。

## 验证

E2E 证明仅拥有指定 `logs.kruntimes.io/runs/log` resource 的 `get` 的 credential 可以通过 `krt`
获取 snapshot，并可 follow 仍在运行的 one-shot Run；缺少该 subresource permission 的 credential
会被 API server 拒绝。
