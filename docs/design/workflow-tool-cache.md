# Runtime tool cache and Workflow step environment

Runtime Pods expose a cache at `KRUNTIME_TOOL_CACHE`. It is on the Pod's
shared workspace, so every Run assigned to that Pod can reuse an installed
tool. It is intentionally a performance cache rather than durable state: a
replacement Pod starts with an empty cache, and Runtime code is trusted.

## Installing a tool with `ensure`

The cache helper is available at `$KRUNTIME_TOOL_CACHE/bin/kruntime-cache`.
Use `ensure` when the installation is a command or script that you provide:

```sh
"$KRUNTIME_TOOL_CACHE/bin/kruntime-cache" ensure <key> -- <command> [args...]
```

`<key>` names the complete installed tool tree. It must be a relative,
slash-separated path, such as `protoc/31.1/linux-amd64`; it cannot escape the
cache root. Concurrent `ensure` calls with the same key run one installation
command only. Other callers wait and then reuse the successfully published
entry.

The installation command receives these environment variables:

| Variable | Meaning | How to use it |
| --- | --- | --- |
| `KRUNTIME_CACHE_STAGING` | An initially empty, private temporary directory. Its actual name is implementation-owned (currently under `$KRUNTIME_TOOL_CACHE/.staging/entry-*`). | Put the downloaded archive and all extracted or installed files here. Treat its path as opaque. |

The helper does not infer the tool's file layout. It publishes the **entire
directory tree** created below `KRUNTIME_CACHE_STAGING` only if the command
exits successfully. For example, if an installer creates:

```text
$KRUNTIME_CACHE_STAGING/
├── protoc.zip
├── bin/
│   └── protoc
└── include/
```

and removes the temporary archive before it exits, the final reusable entry
is:

```text
$KRUNTIME_TOOL_CACHE/protoc/31.1/linux-amd64/
├── bin/
│   └── protoc
└── include/
```

The staging directory is removed after a failed installation. On success the
helper adds its `.complete` marker and atomically publishes it at
`$KRUNTIME_TOOL_CACHE/<key>`. Consumers must use this final path, never the
staging path. `bin` is reserved for the helper and cache keys whose first path
component begins with `.` are reserved for cache internals.

For example, this installs the protoc ZIP using `curl` and `unzip` supplied by
the Runtime image:

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

`fetch <key> <https-url> --extract=zip` is a convenience form for exactly an
HTTPS ZIP download and extraction. It uses the same lock, staging directory,
and atomic publication as `ensure`; use `ensure` when the tool needs any
other installation procedure.

## Workflow environment propagation

Steps in one WorkflowRun job share their workspace but are separate Runs, so
an environment change must travel through durable Run status rather than a
file in the Runtime Pod. A step publishes an environment value by writing a
reserved output:

```sh
printf 'kruntimes.io/env/GOROOT=%s\n' "$go_root" >> "$KRUNTIME_OUTPUTS"
printf 'kruntimes.io/env/PATH=%s:%s\n' "$go_root/bin" "$PATH" >> "$KRUNTIME_OUTPUTS"
```

The WorkflowRun controller applies these values to subsequent steps in the
same job. Later values replace earlier ones; an explicit `step.env` value has
the final precedence. Action-internal steps propagate values to later steps
in the same Action, and a completed Action propagates them to later job steps.

Reserved keys are not application outputs: expressions such as
`${{ steps.setup.outputs.kruntimes.io/env/PATH }}` cannot read them. Values do
not cross job or WorkflowRun boundaries.
