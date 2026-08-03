# Function Inline Source Materialization

Status: **Accepted; API and generic source preparation complete**

Function-mode Runs can use either repository source or inline source. Repository
source already preserves its file layout. Inline source currently does not: runtimed
writes it to a fixed file named `script`. That is insufficient for function handlers
whose runtime-defined handler notation identifies a file or module, such as Python
`app.invoke` or Bash `handler.handle`.

This document defines the minimal API needed to materialize a single inline
function source file predictably, without making runtimed understand any specific
runtime language.

## API

Add `inlinePath` to `Run.spec.source`:

```go
type CodeSource struct {
    Inline     *string `json:"inline,omitempty"`
    InlinePath string `json:"inlinePath,omitempty"`
    RepoURL    string `json:"repoURL,omitempty"`
    CommitSHA  string `json:"commitSHA,omitempty"`
}
```

For the first version, `inlinePath` is only valid for function-mode Runs and is
required when `source.inline` is set for a function-mode Run. It is the relative
path, below the prepared Run working directory, at which runtimed writes the inline
source.

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

The same mechanism supports a Bash function runtime:

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

`inlinePath` describes source materialization only. `handler` remains the
runtime-defined callable entrypoint. Runtimed validates the path but does not infer
language-specific extensions or attempt to prove that it matches the handler. The
Runtime Server remains responsible for validating and loading its handler format.

## Validation and Preparation

`inlinePath` must be a non-empty relative file path when it is required. Validation
must reject absolute paths, `.` or `..` path segments, an empty basename, and values
longer than 4096 bytes. Runtimed must create parent directories beneath the prepared
working directory and write the inline content only to the validated path.

Task-mode inline semantics remain unchanged in this proposal: without function mode,
inline source continues to be written to the existing default `script` file.
`inlinePath` is rejected for task-mode Runs so it cannot introduce ambiguity with
`mode.task.entrypoint`.

An inline source represents exactly one file. Functions needing a package, multiple
files, generated dependencies, or another repository layout should use `source.git`
or a referenced persistent workspace instead.

## Why This Boundary

Inferring `app.py` or `handler.sh` from a handler in runtimed would couple the shared
source-preparation layer to built-in runtime conventions and prevent custom runtimes
from defining their own handler formats. Restricting function mode to repository
source would make the most useful small-function case unnecessarily awkward.

The explicit path keeps the source contract generic and makes the generated file
layout visible to users. It also leaves task process-start semantics unchanged until
they receive their own API review.

## Implementation Follow-Up

The API field, CEL validation, generated deepcopy code, CRD manifests, and
generic source preparation are complete. The remaining implementation is:

1. update function examples and validation tests for Python, Bash, repository, and
   invalid-path cases;
2. implement the function registration lifecycle from `Scheduled` through `Ready`.
