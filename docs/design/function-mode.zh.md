# Function Mode

> 共享 Runtime Gateway 与 `Run.status.endpoint` 已删除。ready Function Run 通过聚合的
> `functions.kruntimes.io` `runs/function` API 调用；见 [Run extension API](run-extension-api.zh.md)。

本文描述 v0.x Function-mode 设计及已经实现的 control-plane 和 invoke-dataplane 基线；延后
工作见 [roadmap](../roadmap/)。Agent sandbox 使用独立的
[Agent Sandbox 的 Session Mode](../session-mode/) 设计。

目标是让 kruntimes 提供低延迟的固定 handler 调用，而不把每次 invocation 都交给
Kubernetes reconciliation。可变 workspace、任意命令和文件操作属于独立的
[Agent Sandbox 的 Session Mode](../session-mode/) 设计。

## 动机

One-shot Runs 适合短任务、CI steps 和自动化命令。有些 workload 则通过固定 handler
提供稳定操作：

- 调用方以不同的有界输入重复调用同一个 handler；
- 多次 invocation 复用已准备的 source 和 Runtime Server registration state；
- invoke path 必须足够快，才能支撑 request-response 用例；
- 高频 invocation 不应向 etcd 写入无界历史。

Kubernetes 仍然是生命周期 control plane。invoke path 应该是 runtime dataplane path。

## 目标

- 使用 `Run` 同时表示 one-shot task 和固定 handler function 的生命周期对象。
- 增加 `Run.spec.mode.function`，使 Run 可以 reserve Runtime Pod，并在删除或 idle
  timeout 前保持 callable。
- 在 Run status 中暴露稳定 runtime gateway endpoint。
- 通过 runtimed 将 invoke request 路由到拥有该 Run 的 Runtime Pod。
- 保持 scheduler 和 runtimed 的通用性。它们不应该理解 agent、workflow 或 MCP 语义。

## 非目标

- kruntimes 不成为 agent framework。
- kruntimes 不负责 prompt management、model routing、memory、tool catalogs 或
  multi-agent planning。
- Function mode 不是 Workflow API 的替代品。
- Function mode 不提供 arbitrary-command sandbox、mutable workspace API 或 file API。

## Run 模型

`spec.source` 描述代码或文件来源。它由 task mode 和 function mode 共享。

`spec.mode` 是 mutually exclusive 的 mode-specific configuration object：

```go
type RunMode struct {
    Task     *TaskMode     `json:"task,omitempty"`
    Function *FunctionMode `json:"function,omitempty"`
}

type TaskMode struct {
    Entrypoint string   `json:"entrypoint,omitempty"`
    Args       []string `json:"args,omitempty"`
}

type FunctionMode struct {
    Handler            string `json:"handler,omitempty"`
    IdleTimeoutSeconds *int32 `json:"idleTimeoutSeconds,omitempty"`
}
```

`mode.task` 和 `mode.function` 必须且只能设置一个；`mode` 是 required，不能省略。

One-shot task Runs 仍然是默认模式。`entrypoint` 和 `args` 属于 task mode，因为它们描述
如何启动一次性进程：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: summarize-once
spec:
  runtime: python
  source:
    inline: |
      print("hello")
  mode:
    task:
      entrypoint: main.py
      args:
        - --verbose
```

Function-mode Runs 会 reserve Runtime Pod 并注册 callable code。`handler` 属于 function
mode，因为它用于识别 callable function entrypoint，类似 AWS Lambda 的
`filename.function` 约定：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: diagnose-service
spec:
  runtime: python
  source:
    inlinePath: main.py
    inline: |
      def invoke(request):
          return {
              "outputs": {
                  "summary": "diagnosis complete"
              }
          }
  mode:
    function:
      handler: main.invoke
      idleTimeoutSeconds: 600
```

当 runtimed 完成 source preparation、把 function 注册到本地 Runtime Server，并且可以接受
invoke 流量时，Run 进入 ready 状态：

```yaml
status:
  phase: Ready
  assignedPod: runtime-python-7f587b4668-njcks
  endpoint:
    protocol: HTTPS
    url: https://runtime-gateway.kruntimes-system.svc.cluster.local/v1/namespaces/kruntimes-demo/runtimes/python/runs/2c24c1f0-9f8f-4f80-82d5-3dd16a12d1e6:invoke
    caBundle: <base64-encoded-PEM>
  conditions:
    - type: Ready
      status: "True"
      reason: FunctionRegistered
```

具体 phase、endpoint、retry、timeout、cleanup、routing、authorization 和 invocation 语义见
[Function Mode 生命周期与 Invoke Dataplane](../function-mode-lifecycle/)。

对于 function-mode Runs，`Ready` 不是 terminal。删除、取消、注册失败或 idle timeout 会结束
reservation。

## 调度和 Capacity

Function-mode Runs 仍然使用普通 Runtime capacity 模型。当 Runtime capacity 允许时，一个
Runtime Pod 可以同时拥有多个 function-mode Run。例如配置了 `runs: "2"` 的 Runtime 可以在
同一个 Runtime Pod 上注册两个 ready function-mode Runs。

这对保持 scheduler 通用性很重要。Function mode 不应隐含 Pod 独占；scheduler 只判断
Runtime Pod 是否还有 capacity 接收另一个 Run。Session Mode 则因为拥有 mutable workspace，
在 v0 要求独占 capacity。

## Handler 字段位置

早期草案曾使用 top-level handler 字段：

```yaml
spec:
  handler: module.function
```

handler 概念仍然有必要存在。它是 FaaS 系统中的常见概念，包括 AWS Lambda，handler 用来
选择具体 callable entrypoint。问题在于位置：top-level `handler` 和 task-only 的
`entrypoint`、`args` 并列，会让 execution model 更难理解。

API 将 handler 放到 function mode 下：

```yaml
spec:
  source:
    git:
      url: https://github.com/example/tools.git
      ref: main
  mode:
    function:
      handler: diagnose.invoke
```

Top-level `handler`、`entrypoint` 和 `args` 不属于目标 Run API。Task mode 把
`entrypoint` 和 `args` 放在 `mode.task` 下，function mode 把 `handler` 放在
`mode.function` 下。

## Runtime Gateway（已删除）

详细 gateway routing 和 authorization contract 见
[Function Mode 生命周期与 Invoke Dataplane](../function-mode-lifecycle/)。

所有 Runtime 共享一个 `runtime-gateway` Deployment 及其 ClusterIP Service。Helm chart 在
`gateway.enabled` 为 true 时安装它们。gateway Deployment 运行 stateless HTTP server。Run endpoint
标识 namespace、Runtime 和 Run UID；gateway 会调用该 Runtime 的 Kubernetes Service，由其选择一个
ready Runtime Pod，之后 runtimed 再解析 owner：

```text
client
  -> shared runtime-gateway Service
     -> runtime-gateway Pod
        -> Runtime=python 的 Kubernetes Service
           -> ready Runtime Pod 的 runtimed
              -> owning runtimed（若不同）
                 -> local Runtime Server
```

gateway Service 地址稳定。Runtime controller 创建的每个 Runtime Service 会选择其 ready Pod；每个
runtimed 只为自身 Runtime 的 Run 解析 ownership：

```text
Run namespace/name/UID -> assigned Runtime Pod UID -> attempt -> readiness
```

Invoke 行为：

- 如果请求落到 owning runtimed，则调用本地 Runtime Server；
- 如果请求落到其他 runtimed，则 proxy 到 owning Runtime Pod；
- 如果 Run 尚未 ready，返回 typed 409 或 503 error；
- 如果 Run 不存在或不属于该 Runtime，返回 404；
- invoke path 不同步读取 Kubernetes API。

## Runtime Server Contract

除了 one-shot execute，Runtime Server 还需要 function-mode contract：

- `RegisterFunction`：为 Run UID 和 ownership attempt 准备代码。
- `InvokeFunction`：针对已注册 function 执行一次 request。
- `UnregisterFunction`：释放 runtime-local state。
- `FunctionStatus`：报告 readiness 和 runtime-local errors。

幂等、fencing、timeout 和 bounded invoke 语义见
[Function Mode 生命周期与 Invoke Dataplane](../function-mode-lifecycle/)。

Invoke response 应包含有界结构化数据：

```json
{
  "outputs": {
    "summary": "1 pending pod found",
    "suspected_cause": "insufficient cpu"
  },
  "artifactRefs": [
    {
      "name": "diagnosis.json",
      "uri": "s3://kruntimes-artifacts/runs/kube-diagnose-agent/diagnosis.json"
    }
  ]
}
```

默认不应把高频 invocation history 写入 `Run.status`。后续可以通过显式 audit sinks、
metrics、logs 或 artifact metadata 增加持久化历史。

## 可靠性和安全要求

Function mode 需要 E2E 覆盖：

- function registration 和 ready status；
- local invoke 和 proxied invoke；
- repeated invocation；
- idle timeout；
- explicit release；
- runtime pod restart recovery；
- cleanup；

Function invocation 保持有界 request-response API，不在 `Run.status` 中持久化任意 command
history 或 workspace contents。

## 实现顺序

1. 增加 mutually exclusive `spec.mode.task` 和 `spec.mode.function` 的 API 设计和
   validation。
2. 删除 top-level `Run.spec.handler`、`Run.spec.entrypoint` 和 `Run.spec.args`；改用
   `Run.spec.mode.function.handler` 和 `Run.spec.mode.task`。
3. 为可选 shared runtime-gateway Deployment 和 Service 增加 Helm templates 和 values。
4. 增加 runtimed ownership cache 和 invoke routing。
5. 增加 Runtime Server register、invoke、unregister 和 status APIs。
6. 实现内置 Bash/Python function-mode adapters。
7. 增加 `krt invoke`。
8. 增加覆盖 ready、invoke、proxy、cleanup 和 restart recovery 的 E2E tests。
