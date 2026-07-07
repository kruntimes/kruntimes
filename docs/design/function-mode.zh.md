# Function Mode 和 Agent Sandboxes

本文描述 v0.x 的目标设计，当前尚未实现。

目标是让 kruntimes 成为 agent platform 的低延迟 sandbox execution substrate。Agent
应该可以 reserve 一个预热 Runtime Pod，准备 callable function 或 actor，通过 dataplane
API 多次 invoke，并在不把每次 invocation 都交给 Kubernetes reconciliation 的情况下释放它。

## 动机

One-shot Runs 适合短任务、CI steps 和自动化命令。Agent workload 往往需要另一种执行形态：

- LLM planner 决定要运行什么 task；
- 生成脚本或 sub-agent task 需要隔离执行环境；
- 多次 invocation 应复用同一份已准备好的代码、环境和 workspace；
- invoke path 必须足够快，才能支撑交互式 agent loop；
- 高频 invocation 不应向 etcd 写入无界历史。

Kubernetes 仍然是生命周期 control plane。invoke path 应该是 runtime dataplane path。

## 目标

- 使用 `Run` 同时表示 one-shot task 和 function-mode sandbox 的生命周期对象。
- 增加 `Run.spec.mode.function`，使 Run 可以 reserve Runtime Pod，并在删除或 idle
  timeout 前保持 callable。
- 在 Run status 中暴露稳定 runtime gateway endpoint。
- 通过 runtimed 将 invoke request 路由到拥有该 Run 的 Runtime Pod。
- 保持 scheduler 和 runtimed 的通用性。它们不应该理解 agent、workflow 或 MCP 语义。
- 提供 SDK，使 agent 开发者不需要手写 Kubernetes watches、gateway discovery、
  port-forwarding、cleanup 和 error handling。

## 非目标

- kruntimes 不成为 agent framework。
- kruntimes 不负责 prompt management、model routing、memory、tool catalogs 或
  multi-agent planning。
- Function mode 不是 Workflow API 的替代品。
- Function mode 不会让内置 runtimes 默认成为 hostile-code sandboxes。更强隔离仍然是
  独立 runtime 和部署选择。

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

`mode.task` 和 `mode.function` 必须且只能设置一个。为了兼容现有 one-shot Runs，如果
`mode` 为空，Run 默认使用 task mode。

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
  name: kube-diagnose-agent
spec:
  runtime: python
  source:
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
    protocol: HTTP
    url: http://python-runtime-gateway.kruntimes-demo.svc.cluster.local/v1/runs/kube-diagnose-agent/invoke
  conditions:
    - type: Ready
      status: "True"
      reason: FunctionRegistered
```

对于 function-mode Runs，`Ready` 不是 terminal。删除、取消、注册失败或 idle timeout 会结束
reservation。

## 调度和 Capacity

Function-mode Runs 仍然使用普通 Runtime capacity 模型。当 Runtime capacity 允许时，一个
Runtime Pod 可以同时拥有多个 function-mode Run。例如配置了 `runs: "2"` 的 Runtime 可以在
同一个 Runtime Pod 上注册两个 ready function-mode Runs。

这对保持 scheduler 通用性很重要。Function mode 不应隐含 Pod 独占。scheduler 只判断
Runtime Pod 是否还有 capacity 接收另一个 Run；它不理解这个 Run 代表的是 agent sandbox、
内部 tool，还是其他产品层概念。

Agent sandbox 场景不同。它通常期望更强的 workspace ownership、可预测 cleanup，以及更少
令人意外的 cross-run interaction。对于这类场景，推荐的部署形态是每个 Runtime Pod 只承载
一个 function-mode Run：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: python-agent-sandbox
spec:
  capacity:
    resources:
      runs: "1"
```

SDK 可以为这个建议提供 guardrails。例如 agent sandbox SDK 可以在目标 Runtime capacity
大于 1 时给出 warning，或者提供 helper 创建/选择配置了 `runs: "1"` 的 dedicated Runtime。
这个 guardrail 应该属于 SDK 或更高层 integration；scheduler 仍然只执行通用 capacity
contract。

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

## Runtime Gateway

每个 Runtime 应该有一个 gateway Service：

```text
python-runtime-gateway Service
  -> Runtime Pods for Runtime=python
     -> runtimed sidecar
        -> local Runtime Server
```

Service 地址是稳定的。Kubernetes Service 负载均衡可能把 invoke request 发到该 Runtime
pool 中的任意 runtimed，因此每个 runtimed 都需要 ownership cache：

```text
Run UID/name -> assigned Runtime Pod -> readiness -> function ID
```

Invoke 行为：

- 如果请求落到 owning runtimed，则调用本地 Runtime Server；
- 如果请求落到其他 runtimed，则 proxy 到 owning Runtime Pod；
- 如果 Run 尚未 ready，返回 typed 409 或 503 error；
- 如果 Run 不存在或不属于该 Runtime，返回 404；
- invoke path 不同步读取 Kubernetes API。

## Runtime Server Contract

除了 one-shot execute，Runtime Server 还需要 function-mode contract：

- `RegisterFunction`：准备代码并返回 function ID。
- `InvokeFunction`：针对已注册 function 执行一次 request。
- `UnregisterFunction`：释放 runtime-local state。
- `FunctionStatus`：报告 readiness 和 runtime-local errors。

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

## Agent SDKs

Agent 开发者不应该手动拼接这条链路。第一批 SDK 应优先支持 Python 和 Go。

SDK 应暴露 sandbox 语义，即使 kruntimes 底层 control-plane object 是 function-mode Run。
这和 Kubernetes SIG Apps agent-sandbox 这类项目的开发者模型一致：调用方 create 或 attach
到 sandbox，运行命令或调用 tool，传输文件，需要保留 session 时 disconnect，需要清理时
terminate。

SDK 应提供 sandbox-facing API：

- create、open、reattach、disconnect 和 terminate sandbox session；
- 默认隐藏 function-mode Run 对象，除非调用方需要低层 Kubernetes metadata；
- 等待底层 Run 进入 `Ready`；
- 发现 runtime gateway endpoint；
- 可选地验证所选 agent-sandbox Runtime 是否配置为每个 Runtime Pod 只承载一个 Run；
- 在 Kubernetes 内运行时使用 direct in-cluster URLs；
- 本地开发时 fallback 到 port-forward；
- 使用 typed request 和 response objects invoke tools 或 run commands；
- 将文件操作暴露为 sandbox operations，例如 write、read、list 和 exists；
- 通过 sandbox methods 读取 outputs、artifacts 和 logs；
- 为幂等操作应用 timeouts 和 retries；
- 本地网络中断后 reconnect；
- 默认清理底层 function-mode Run，并提供显式 preserve、disconnect 或 reattach 选项。

示例形态：

```python
from kruntimes import SandboxClient

client = SandboxClient(namespace="kruntimes-demo")

with client.create_sandbox(
    name="kube-diagnose-agent",
    runtime="python",
    source_file="agent_tool.py",
    idle_timeout_seconds=600,
) as sandbox:
    sandbox.files.write("request.json", b'{"namespace":"default"}')

    result = sandbox.commands.run({
        "task": "diagnose-kubernetes",
        "clusterSnapshot": {
            "namespace": "default",
            "pods": []
        },
    })

    report = sandbox.files.read("report.md")
    print(result.outputs["summary"])
```

SDK API 可以使用 `Sandbox` 作为 developer-facing 概念，但不引入 Kubernetes `Sandbox`
CRD。一个 sandbox handle 映射到底层 function-mode Run、gateway connection state、
file/log/artifact helpers 和 lifecycle cleanup。

推荐 SDK objects：

- `SandboxClient`：负责 Kubernetes client configuration、gateway discovery 和已跟踪的
  sandbox sessions。
- `Sandbox`：表示一个已打开的 sandbox handle，并暴露 lifecycle、command/tool、file、
  log、artifact 和 identity helpers。
- `Commands` 或 `Tools`：在 sandbox 内执行 command 或 structured tool request。
- `Files`：上传、下载、列出和检查 workspace files。
- `Info`：只读 identity metadata，例如 Run name、Run UID、namespace、Runtime、assigned
  Pod、gateway URL 和 readiness。

SDK 至少应支持三种 connection modes：

- gateway mode：生产流量通过 runtime gateway Service 或 external Gateway；
- local port-forward mode：用于本地开发和 CI，不要求用户手动执行 `kubectl port-forward`；
- direct URL mode：用于 in-cluster agents 或 custom domains。

Retry 行为应区分幂等操作和执行操作。File read/write/list/exists 与 readiness checks 可以在
transient transport errors 时重试。Tool 或 command execution 默认应只尝试一次，除非调用方
显式 opt in，因为任意执行不一定幂等。

Typed errors 应让恢复动作明确，例如 not ready、timeout、gateway unavailable、
port-forward died、sandbox deleted、orphaned Run、retries exhausted，以及 non-OK invoke
response。

## Workspace、Files、Logs 和 Artifacts

Agent tasks 经常需要上传生成脚本、检查文件并获取报告。这些操作不应该变成 Kubernetes
reconciliation loop。

必需 API：

- 上传文件或目录到 function workspace；
- list 和 read workspace files；
- stream function Run 和单次 invoke 的 logs；
- 通过配置的 ArtifactStore 发布 artifacts；
- 从 invoke responses 返回 artifact references；
- 在删除或 idle timeout 时清理 workspace state。

对于 v0.x，workspace operations 可以先限制在 trusted agent integrations。多租户生产使用
需要清晰的 RBAC、network policy 和 runtime isolation guidance。

## 集成边界

Agent frameworks 和 MCP-style tool servers 可以通过把 tool call 映射到 function-mode Run
来集成 kruntimes：

```text
Agent / MCP tool server
  -> kruntimes SDK
     -> Run lifecycle through Kubernetes API
     -> invoke through runtime gateway
     -> structured result back to agent
```

kruntimes 不应要求 agent framework 采用 kruntimes-specific planning concepts。集成边界是
sandbox handle 和 invoke API。

## 可靠性和安全要求

Function mode 需要 E2E 覆盖：

- function registration 和 ready status；
- local invoke 和 proxied invoke；
- repeated invocation；
- artifact reuse；
- idle timeout；
- explicit release；
- runtime pod restart recovery；
- cleanup；
- service account selection；
- runtime pod security context；
- resource limits；
- network policy guidance。

未来更强的 runtime backends，例如 gVisor、Kata 或 Firecracker，应能接在同一个 Runtime
abstraction 后面。

## 实现顺序

1. 增加 mutually exclusive `spec.mode.task` 和 `spec.mode.function` 的 API 设计和
   validation。
2. 删除 top-level `Run.spec.handler`、`Run.spec.entrypoint` 和 `Run.spec.args`；改用
   `Run.spec.mode.function.handler` 和 `Run.spec.mode.task`。
3. 为每个 Runtime 增加 runtime gateway Service reconciliation。
4. 增加 runtimed ownership cache 和 invoke routing。
5. 增加 Runtime Server register、invoke、unregister 和 status APIs。
6. 实现内置 Bash/Python function-mode adapters。
7. 增加 `krt invoke` 和第一版 SDK 形态。
8. 增加覆盖 ready、invoke、proxy、cleanup 和 restart recovery 的 E2E tests。
9. 将 agent demo 从 target design 更新为 supported path。
