# Function Inline Source 物化

状态：**提案，等待 review**

function-mode Run 可以使用 repository source 或 inline source。repository source 会保留原有
文件布局；当前 inline source 不会：runtimed 会将其写到固定的 `script` 文件。对于由 runtime
定义、并通过文件或 module 标识的 handler，这并不足够，例如 Python `app.invoke` 或 Bash
`handler.handle`。

本文提出一个最小 API，使单个 inline function source 能以可预测的文件路径物化，同时不让
runtimed 理解具体 runtime 的语言。

## 提议的 API

在 `Run.spec.source` 增加 `inlinePath`：

```go
type CodeSource struct {
    Inline     string `json:"inline,omitempty"`
    InlinePath string `json:"inlinePath,omitempty"`
    Git        *GitSource `json:"git,omitempty"`
}
```

第一版中，`inlinePath` 只允许在 function-mode Run 中使用；当 function-mode Run 设置了
`source.inline` 时它必须存在。它表示 runtimed 在准备后的 Run working directory 下写入
inline source 的相对路径。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: kube-diagnose-agent
spec:
  runtime: python
  source:
    inlinePath: app.py
    inline: |
      def invoke(request):
          return {"summary": "diagnosis complete"}
  mode:
    function:
      handler: app.invoke
```

同一机制也适用于 Bash function runtime：

```yaml
spec:
  runtime: bash
  source:
    inlinePath: handler.sh
    inline: |
      handle() {
        printf '%s\n' "$1"
      }
  mode:
    function:
      handler: handler.handle
```

`inlinePath` 只描述 source 的物化方式。`handler` 仍然是由 runtime 定义的 callable
entrypoint。runtimed 负责验证路径，但不推断语言相关扩展名，也不试图验证它是否和 handler
匹配。Runtime Server 仍负责验证和加载其 handler 格式。

## 验证与准备

当 `inlinePath` 被要求时，它必须是非空的相对文件路径。验证必须拒绝绝对路径、`.` 或 `..`
path segment、空 basename 以及超过 4096 bytes 的值。runtimed 必须只在准备后的 working
directory 下创建父目录，并将 inline 内容写到已验证的路径。

本提案不改变 task-mode inline 的语义：未使用 function mode 时，inline source 继续写到已有的
默认 `script` 文件。task-mode Run 上的 `inlinePath` 会被拒绝，避免它和
`mode.task.entrypoint` 产生歧义。

一份 inline source 只表示一个文件。需要 package、多个文件、生成的依赖或其他 repository
布局的 function，应使用 `source.git` 或引用 persistent workspace。

## 为什么采用这个边界

在 runtimed 中根据 handler 推断 `app.py` 或 `handler.sh`，会把共享 source-preparation 层和
内置 runtime 的约定耦合起来，并阻止 custom runtime 定义自己的 handler 格式。限制 function
mode 只能使用 repository source，又会使最常见的小型 function 场景不必要地复杂。

显式路径使 source contract 保持通用，也让用户可以直接看出生成后的文件布局。同时，在 task
process-start 语义经过单独 API review 前，它不会改变 task 的行为。

## 后续实现

该 API 获得批准后：

1. 增加 API field、CEL validation、generated deepcopy code 和 CRD manifests；
2. 更新通用 source preparation，使其物化已验证的路径；
3. 更新 function 示例和 Python、Bash、repository、invalid-path 的 validation tests；
4. 实现从 `Scheduled` 到 `Ready` 的 function registration lifecycle。
