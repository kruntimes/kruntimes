# Runtime Gateway Run Log API

状态：**已实现——直连 Gateway 与 Kubernetes 聚合 API**

## 问题

Run output 会作为 structured records 写入 owner Runtime Pod 的 `runtimed` container log。
在这个 API 之前，Dashboard 使用 caller 的 Kubernetes credential 读取该 Pod log；`krt logs`
则 port-forward 到 Runtime Pod，并可能回退到相同的 Pod-log subresource。因此，可以读取 Run 的
用户未必可以读取其日志：Dashboard 要求 `get pods/log`，而 `krt logs` 还需要 Pod discovery
以及 `create pods/portforward`。

这些是 Kubernetes 实现层权限，并不是 kruntimes 希望向用户提供的 capability：*读取这个 Run
的日志*。

## 决定

Runtime Gateway 保留为直连、外部 HTTP Run-log API；它不会变成 aggregation server，仍适合
独立部署的 client。同时 chart 会部署独立的 `runtime-log-apiserver`，并通过 Kubernetes API
aggregation 注册 `logs.kruntimes.io/v1alpha1`。两条路径都不创建 log store，只读取相同的现有
Kubernetes container-log stream，并只暴露经过过滤且有界的 Run records。

`krt logs` 缺省使用聚合 endpoint，因此用户普通 kubeconfig（bearer token、exec credential 或
client certificate）由 API server 认证。显式提供 `--gateway-url` 才选择直连 Gateway。Dashboard
也缺省使用 aggregation；chart 可显式选择直连 Gateway mode。

初始 endpoint 为：

```text
GET /v1/namespaces/{namespace}/runtimes/{runtime}/runs/{runUID}/logs?tailLines={lines}&limitBytes={bytes}&follow={true|false}
```

`namespace`、`runtime` 和 `runUID` 与现有 Gateway API 使用相同的 immutable Run selection。
`runUID` 绝不替换为可变的 Run name，从而避免被删除后重建的 Run 收到前一个对象的 records。

聚合 endpoint 故意使用独立 API group，避免接管 CRD 的 `kruntimes.io/v1alpha1` group/version：

```text
GET /apis/logs.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/log?tailLines={lines}&follow={true|false}&cursor={opaque}&timeoutSeconds={seconds}
```

它受 Kubernetes `get` `logs.kruntimes.io/runs/log` 保护；backend 还会对目标
`kruntimes.io/runs` 做 exact `get` SubjectAccessReview。aggregated server 仅在 mTLS 验证
Kubernetes aggregation proxy 后才信任 identity headers；启动时读取 API server 所有的
`kube-system/extension-apiserver-authentication` ConfigMap，其中包括 request-header client CA
和允许的 proxy identity。

## Request 和 Response Contract

query shape 刻意遵循 Kubernetes `pods/log` 的相关语义。`tailLines` 可选，默认 100，必须是
1 到 500 的整数。对 non-following request，`limitBytes` 可选，默认 1 MiB，必须是正整数且不超过
1 MiB。Gateway 从 Run 的 `status.startTime`（缺失时使用 creation time）开始读取 `runtimed`
container log，按请求的 Run UID 过滤后，再将 `tailLines` 应用于选中的 records。它将 `limitBytes`
应用于 non-following Kubernetes source read。这样，同一共享 Runtime Pod 上后续 Run 的活动不会把
仍保留的目标 Run 挤出全局 Pod-log tail。它仍不承诺持久或完整日志保留：繁忙 Pod 可能超过 source
bound，持久保留仍由 cluster log collector 负责。

`follow=true` 时不接受 `limitBytes`。Kubernetes 将 `PodLogOptions.LimitBytes` 作用于整个
followed source stream；若在此使用 1 MiB snapshot bound，会让本来健康的 follow connection 静默
结束。follow stream 改由 Gateway concurrency、client cancellation 和 2 MiB per-record limit 限制。

`follow` 缺省或为 `false` 时，response 为 `application/json`：

```json
{
  "items": [
    {
      "timestamp": "2026-09-02T10:00:00Z",
      "stream": "stdout",
      "message": "hello",
      "invocationId": "optional-invocation-id",
      "operation": "execute",
      "outcome": "succeeded"
    }
  ]
}
```

structured runtimed record 中不存在的字段会省略。稳定且安全的字段为 timestamp、stream、message、
由 Run UID 派生的 selection、invocation ID、operation、outcome、status code、exit code、timeout
marker 与 duration。绝不返回 raw Kubernetes log metadata 或其它 Runs 的 records。

`follow=true` 是一等的 streaming operation，类似 Kubernetes `pods/log?follow=true`，而不是
polling convention。response 为 `application/x-ndjson`；在发送 headers 后，Gateway 会在每条
matching record 到达时立即 flush，不会等待构建完整 response。stream 从 Run start 而不是全局
Pod tail 开始读取，因此目标 Run 仍保留的 matching records 不会被后续 Run 隐藏。其它 Runs 的
records 会被丢弃，不能写出空行。caller 断开连接、Pod log stream 关闭或 Gateway shutdown 时 stream
结束。`follow` 不会让 Gateway 成为无界 persistence service；request-concurrency limit 在整个
stream 生命周期内仍然生效。

endpoint 是只读的。它支持具有 assigned Runtime Pod 的 task、function 和 session Runs。没有
assigned Pod 的 Run，或其记录的 assigned Runtime Pod 此后已删除，均返回 `409 Conflict`；
不存在的 Run 或不再属于指定 Runtime 的 Run 返回 `404 Not Found`。

## Streaming 实现

stream 由 Gateway 自己实现；它不会将 caller redirect 到 Kubernetes API，也不会 proxy 任意的
Pod-log URL。

1. 在写出 response 前 parse 并 validation route 与 query。
2. 从 Gateway cache resolve immutable Run，验证其 UID 和指定的 Runtime，再对 caller 做该精确
   Run 的 authorization。
3. 用 Gateway ServiceAccount 的 typed core client 打开唯一一个 Kubernetes log stream：

   ```go
   coreClient.Pods(run.Namespace).GetLogs(run.Status.AssignedPod, &corev1.PodLogOptions{
       Container:  "runtimed",
       Follow:     follow,
       SinceTime:  run.Status.StartTime, // unset 时使用 creation time
       // LimitBytes 只在 Follow 为 false 时设置。
   }).Stream(ctx)
   ```

   stream 必须在 Gateway 写 HTTP headers 前成功打开；这样 Pod-log service 不可用时仍能返回普通的
   有界 JSON error。
4. 以有界 reader 每次 decode 一条 newline-delimited record。structured record 上限为 2 MiB
   （1 MiB raw-log request limit 加 JSON framing headroom）。更大的 line 会被丢弃，且不能分配
   无界 memory。无效 JSON 或 `run_uid` 不同于 selected UID 的 record 也会丢弃。
5. 对 matching record，只 encode 文档定义的 safe fields 为一行 JSON，写入 response 后调用
   `http.NewResponseController(w).Flush()`。response 使用
   `Content-Type: application/x-ndjson`，不设置 content length；HTTP/1.1 使用 chunked transfer，
   HTTP/2 则正常 stream data frames。
6. 在 `request.Context().Done()`、source EOF 或 source read error 时停止并关闭 Kubernetes
   stream。streaming headers 写出后，后续 source error 不能转换为 HTTP error response；Gateway
   结束 stream 并在服务端记录 error。client 把 unexpected EOF 视为 interrupted follow，并可用新的
   bounded tail request reconnect。

`follow=false` 时，同一个 decoder 在 bounded ring 中最多收集 `tailLines` 条 matching records，
只在 source stream 关闭后才写出文档定义的 JSON envelope。它刻意与 streaming writer 分离，以保持
普通 tail response 始终是有效 JSON，绝不成为 partial document。

## One-Shot Runtime Output 投影

one-shot `Runtime.Execute` 是异步操作：它创建本地 execution 后立即返回其 ID，不会等待 command
完成。Runtime Server 的 unary `Status` response 会在 execution 运行期间返回当前有界的 cumulative
stdout/stderr buffer；exit code 和 final state 只有在进入 terminal state 后才是 authoritative。

owner runtimed 在 one-shot Run active 期间轮询既有 status，并只把新观察到的**完整行**投影到自身的
structured container log。它为 stdout/stderr 分别保存 cursor 和尚未完成的 trailing line。terminal
status 在 Run terminal status update 成功后才 flush 剩余的未换行内容。因此 Gateway `follow=true`
reader、Dashboard 和 `krt logs --follow` 都能在 command exit 前观察到 output，而不需要把日志内容
放进 `Run.status`，也不需要引入第二种 log transport。

该投影刻意采用 at-least-once 语义。cursor 只存在于一个 runtimed process；在 runtimed 或 Runtime
Server recovery 后，仍保留的 output 可以再次写出，而不是被静默丢失。重新连接 Gateway follow stream
的 client 同样必须容忍 retained record 重复。Runtime Server 的 per-stream output limit 仍是上界：其
bounded buffer 截断后会写出 truncation marker，但之后被丢弃的 bytes 无法恢复。Kubernetes log
collection 仍负责 durable retention。

## Client Streaming Contract

client 以普通的 authenticated HTTP `GET` stream logs；不使用 WebSocket、Server-Sent Events
protocol、polling endpoint，也没有 client-visible Pod connection。例如，command-line client 发送：

```sh
curl --no-buffer \
  --header "Authorization: Bearer $TOKEN" \
  --header "Accept: application/x-ndjson" \
  "${GATEWAY_URL}/v1/namespaces/team-a/runtimes/python/runs/${RUN_UID}/logs?tailLines=100&follow=true"
```

`--no-buffer` 对 terminal client 很重要：它让 curl 在 response body 到达时立即打印每条 NDJSON
record。Go client 保持 response body，并在 context cancelled 或 server 关闭 body 前每次 decode
一个 JSON value：

```go
request, err := http.NewRequestWithContext(ctx, http.MethodGet, logsURL, nil)
if err != nil { /* handle */ }
request.Header.Set("Authorization", "Bearer "+token)
request.Header.Set("Accept", "application/x-ndjson")

response, err := httpClient.Do(request)
if err != nil { /* handle */ }
defer response.Body.Close()
if response.StatusCode != http.StatusOK { /* decode bounded error */ }

decoder := json.NewDecoder(response.Body)
for {
    var record RunLogRecord
    if err := decoder.Decode(&record); errors.Is(err, io.EOF) { break
    } else if err != nil { /* interrupted or malformed stream */ }
    render(record)
}
```

Dashboard browser 不直接调用 Gateway：login token 为 HttpOnly，不能暴露给 JavaScript。Dashboard
backend 会使用 session token 打开这个 Gateway stream，再通过同源的内部 log endpoint relay NDJSON
body。React frontend 消费该 response 的 `ReadableStream`，incrementally split newline-delimited JSON，
逐条渲染 record。这样既保持 browser 原有的 cookie boundary，也保持端到端 streaming，而不是 polling。

Gateway base URL 是 deployment configuration，而不是 Pod address。Dashboard backend 使用
in-cluster Gateway Service；in-cluster `krt` client 可以使用该 Service DNS name。external `krt`
client 必须获得 operator-managed、可达的 Gateway URL 及其 TLS trust material（例如
`--gateway-url` 加 normal system trust store 或 `--gateway-ca-file`）。它不静默创建
Runtime-Pod 或 Gateway Pod port-forward，否则会重新引入 caller `pods/portforward` requirement。
chart 默认仍将 Gateway 保持为 ClusterIP；是否暴露给集群外是另一个 operator deployment decision。

`krt logs` 同时为初始 `get runs` request 和 Gateway request 使用 kubeconfig 所选的 bearer token
或 exec-token credential：

```sh
krt logs my-run -n team-a --gateway-url https://gateway.example \
  --gateway-ca-file ./gateway-ca.crt --tail 100 --follow
```

它使用独立的 Gateway TLS transport，信任 normal system store 或 `--gateway-ca-file`；
`--gateway-insecure-skip-tls-verify` 是明确的 development-only escape hatch。transport 会在
kubeconfig 包含 client certificate 时保留它。要授权该 certificate，operator 将签发 Kubernetes
user certificate 的 CA Secret 配置为 `gateway.tls.clientCASecretName`。HTTPS Gateway 验证 mTLS，
并将已验证 certificate 的 CN 作为 Kubernetes username、O values 作为 groups 创建
SubjectAccessReview。未配置 Gateway client CA 时，bearer 和 exec tokens 仍是支持的认证方式。
`--tail` 默认为 100，范围为 1 至 500。CLI 解码文档定义的 JSON/NDJSON records，只将
`stdout` 和 `stderr` records 分别写到对应的 terminal streams。

当 HTTP 已启用（包括两种协议都启用）时，Dashboard chart 优先使用 Gateway 的 in-cluster
HTTP Service port。仅 HTTPS 的部署中，它只读挂载已配置的 Gateway CA-bundle key，并用该
bundle 验证 Gateway Service certificate。Dashboard 不挂载 Gateway private key，也不向其
ServiceAccount 授予 `pods/log`。

### HTTP Protocol 和 Server 要求

`ReadableStream` 是 Fetch response-body feature，不是 HTTP upgrade protocol。它可用于 HTTP/1.1
中通过 chunked transfer 的 response、HTTP/2 中通过 DATA frames 的 response，或 HTTP/3。初始
Gateway slice 必须支持 HTTP/1.1；其 HTTPS listener 必须通过 ALPN 协商 HTTP/2（`h2`，并以
`http/1.1` fallback）。client 不发送 `Upgrade: websocket`，Gateway 也不返回
`101 Switching Protocols`。

Go handler 必须设置 `Content-Type: application/x-ndjson` 和 `Cache-Control: no-cache`，省略
`Content-Length` 与 `Content-Encoding`，只在 Kubernetes log stream 已打开后才调用
`WriteHeader(http.StatusOK)`，再逐条 matching record encode/write/flush。HTTP/1.1 场景中，Go
的 `net/http` 在没有 content length 时自动使用 chunked transfer；handler 不能自行设置
`Transfer-Encoding`。

TLS listener 中，Gateway 必须配置其 `http.Server`，并在 raw TCP listener 上调用
`server.ServeTLS`（或等价地在 serving 前配置 `TLSConfig.NextProtos` 和 HTTP/2）。只把 listener
包装为 TLS 后调用普通 `Serve`、却没有 ALPN setup，不能承诺 HTTP/2。long-lived log response
不能受 server-wide `WriteTimeout` 限制；Gateway 对 streaming listener 保持该值 disabled，同时
保留 header 和 request-concurrency bounds。

chart 的 ClusterIP Service 是 L4 hop，不会 buffer response。operator 未来若在 Gateway 前放置
ingress、reverse proxy 或 service mesh proxy，该 proxy 必须保留 streaming：对此 route 禁用
response buffering/compression，并设置适合 `follow=true` 的 idle timeout。这是 operator exposure
问题，不是替代的 API transport。

## Authentication、Authorization 和 RBAC

Gateway 要求 `Authorization: Bearer` token。它通过 Kubernetes `TokenReview` 认证 token，再对
已解析的精确 namespace 中 `kruntimes.io` `runs` resource 的 `get` 创建
`SubjectAccessReview`。SAR 的 resource name 是已解析的 Run name；immutable UID 用于 endpoint
selection 和 log filtering。

在该 decision 成功后，由 Gateway（而不是 caller）推导 `Run.status.assignedPod`，并只读取这个
Pod 的 `runtimed` container。caller 不能选择 Pod、container、namespace，也不能选择路由选中
Run 之外的 UID。Gateway ServiceAccount 除现有 Run cache、TokenReview 和 SubjectAccessReview
权限外，还需要：

```yaml
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
```

该 feature 不得为 Gateway 授予广泛的 Pod `get`、`list`、`watch` 或 write 权限。

因此，Dashboard 或 CLI 用户只需对相关 Run 有 `get`，不再需要仅为读取该 Run 日志而拥有
`get pods/log`、`get/list pods` 或 `create pods/portforward`。其它 Dashboard detail pages 保持其
单独定义的 authorization policy。

## Errors

| 条件 | HTTP status |
| --- | ---: |
| 缺少或无效的 bearer token | 401 |
| 已认证 caller 没有已解析 Run 的 `get` 权限 | 403 |
| route 中的 `tailLines`、`limitBytes` 或 `follow` 无效 | 400 |
| 没有匹配的 Run / runtime | 404 |
| 匹配的 Run 没有 assigned Pod，或其 assigned Runtime Pod 已不存在 | 409 |
| Gateway log reader 未配置，或 Kubernetes log service 不可用 | 503 |
| 达到 Gateway request concurrency limit | 429 |

response body 是有界的 Gateway error object，不能暴露未选中 Pod name、container name、token 或
Kubernetes authorization detail。

## Client Migration 和交付

现有 Dashboard endpoint 仍是内部 Dashboard API。其 backend 使用 login token 调用这个 Gateway
endpoint。`krt logs` 调用同一 endpoint，不再 port-forward Runtime Pods 或回退到直接
`pods/log`。两个 client 都不保留旧的特权路径。

Gateway chart component 是 opt-in；当此 API 不可用时，Dashboard 和 `krt logs` 返回明确的
configuration error，不能回退到直接 Pod access。operator 在该模型下启用 Dashboard log access
时，也必须启用并使 Gateway 可达。标准 E2E installation 同时启用两个组件。

实现拆分为可以独立 review 的 commits：

1. Gateway route、structured-record filtering/bounds、ServiceAccount `pods/log` permission、
   HTTP/1.1 与 HTTPS/HTTP/2 streaming setup、follow-stream flush/disconnect tests，以及 Helm
   coverage。
2. Dashboard 迁移到 Gateway client，移除 caller-scoped Pod-log reader 及对应 RBAC，并增加
   focused tests。
3. `krt logs` 迁移到 Gateway client，移除 Runtime-Pod port-forward 与 direct Pod-log fallback，
   并增加 focused tests。
4. E2E 证明仅有 `get runs` 的 token 可以读取自己的 Run logs，而缺少该权限的 token 不可以。
