# Runtime 工具缓存与 Workflow step 环境

每个 Runtime Pod 都会通过 `KRUNTIME_TOOL_CACHE` 暴露一个缓存目录。它位于 Pod
共享的 workspace 中，因此调度到同一 Pod 的所有 Run 都可以复用已经安装的工具。该目录
是性能缓存而非持久化状态：替换 Pod 后缓存为空；同时 Runtime 中执行的代码属于可信代码。

## 使用 `ensure` 安装工具

缓存工具位于 `$KRUNTIME_TOOL_CACHE/bin/kruntime-cache`。当安装过程需要由用户提供命令或脚本时，
使用 `ensure`：

```sh
"$KRUNTIME_TOOL_CACHE/bin/kruntime-cache" ensure <key> -- <command> [args...]
```

`<key>` 表示完整的已安装工具目录，例如 `protoc/31.1/linux-amd64`。它必须是相对、以 `/`
分隔的路径，不能逃逸缓存根目录。多个并发的相同 key `ensure` 调用只会运行一次安装命令；
其他调用会等待并复用成功发布的条目。

安装命令会获得以下环境变量：

| 变量 | 含义 | 脚本应如何使用 |
| --- | --- | --- |
| `KRUNTIME_CACHE_STAGING` | 一个初始为空且私有的临时目录。具体名称由实现管理（当前位于 `$KRUNTIME_TOOL_CACHE/.staging/entry-*` 下）。 | 将下载的压缩包、解压后的文件和所有安装结果写入这里；不要依赖其具体路径。 |

helper 不会推断工具的目录结构。只有安装命令成功退出后，它才会发布
`KRUNTIME_CACHE_STAGING` 下的**整棵目录树**。例如安装脚本创建：

```text
$KRUNTIME_CACHE_STAGING/
├── protoc.zip
├── bin/
│   └── protoc
└── include/
```

若在退出前删除临时压缩包，最终可以复用的条目就是：

```text
$KRUNTIME_TOOL_CACHE/protoc/31.1/linux-amd64/
├── bin/
│   └── protoc
└── include/
```

安装失败时 staging 目录会被删除。成功时 helper 会添加 `.complete` 标记，并将其原子发布到
`$KRUNTIME_TOOL_CACHE/<key>`。使用者必须使用这个最终路径，不能使用 staging 路径。`bin`
为 helper 保留；首个路径组件以 `.` 开头的 key 为缓存内部目录保留。

以下示例使用 Runtime 镜像提供的 `curl` 和 `unzip` 安装 protoc ZIP：

```sh
version=31.1
key="protoc/${version}/linux-amd64"

"$KRUNTIME_TOOL_CACHE/bin/kruntime-cache" ensure "$key" -- \
  bash -ceu '
    archive="$KRUNTIME_CACHE_STAGING/protoc.zip"
    curl --fail --location --silent --show-error "$1" --output "$archive"
    unzip -q "$archive" -d "$KRUNTIME_CACHE_STAGING"
    rm -f "$archive"
  ' bash \
  "https://github.com/protocolbuffers/protobuf/releases/download/v${version}/protoc-${version}-linux-x86_64.zip"

protoc_root="$KRUNTIME_TOOL_CACHE/$key"
test -x "$protoc_root/bin/protoc"
```

`fetch <key> <https-url> --extract=zip` 是 HTTPS ZIP 下载和解压的便捷形式。它与 `ensure`
使用相同的锁、staging 目录和原子发布语义；工具需要其他安装流程时应使用 `ensure`。

## Workflow 环境传递

同一 WorkflowRun job 的 steps 会共享 workspace，但每个 step 是独立的 Run。因此环境变化
必须通过持久化的 Run status 传递，而不是通过 Runtime Pod 内的文件传递。一个 step 可以写入
保留 output 来发布环境变量：

```sh
printf 'kruntimes.io/env/GOROOT=%s\n' "$go_root" >> "$KRUNTIME_OUTPUTS"
printf 'kruntimes.io/env/PATH=%s:%s\n' "$go_root/bin" "$PATH" >> "$KRUNTIME_OUTPUTS"
```

WorkflowRun controller 会把这些值应用到同一 job 后续的 steps。后写入的值覆盖先前值；显式的
`step.env` 具有最高优先级。Action 内部 step 会向后续 Action step 传递；Action 完成后也会向
后续 job step 传递。

保留 key 不是应用 output：`${{ steps.setup.outputs.kruntimes.io/env/PATH }}` 之类的表达式无法
读取它们。这些值不会跨 job 或 WorkflowRun 传递。
