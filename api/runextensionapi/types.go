package runextensionapi

// Command defines one workspace-relative process execution request.
type Command struct {
	Argv             []string          `json:"argv,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Stdin            []byte            `json:"stdin,omitempty"`
	TimeoutMillis    int64             `json:"timeoutMillis,omitempty"`
}

// Operation is one mutation submitted over a persistent Session connection.
// Exactly one field must be set.
type Operation struct {
	Command         *Command         `json:"command,omitempty"`
	WriteFile       *FileWrite       `json:"writeFile,omitempty"`
	CreateDirectory *DirectoryCreate `json:"createDirectory,omitempty"`
	DeleteFile      *FileDelete      `json:"deleteFile,omitempty"`
	RenameFile      *FileRename      `json:"renameFile,omitempty"`
}

// FileWrite writes one bounded file relative to the Session workspace.
type FileWrite struct {
	Path          string `json:"path"`
	Contents      []byte `json:"contents"`
	CreateParents bool   `json:"createParents,omitempty"`
}

// DirectoryCreate creates one workspace-relative directory.
type DirectoryCreate struct {
	Path string `json:"path"`
}

// FileDelete deletes one workspace-relative path.
type FileDelete struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// FileRename renames one workspace-relative path.
type FileRename struct {
	SourcePath      string `json:"sourcePath"`
	DestinationPath string `json:"destinationPath"`
	Overwrite       bool   `json:"overwrite,omitempty"`
}

// CommandResult is the bounded result of one command operation.
type CommandResult struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
}

// OperationEvent is one ordered event emitted by a streaming Session operation.
type OperationEvent struct {
	Sequence int64  `json:"sequence"`
	Type     string `json:"type"`
	Accepted *struct {
		OperationID string `json:"operationID"`
	} `json:"accepted,omitempty"`
	Output    *OperationOutput   `json:"output,omitempty"`
	Progress  *OperationProgress `json:"progress,omitempty"`
	Completed *struct {
		Command *CommandResult `json:"command,omitempty"`
	} `json:"completed,omitempty"`
	Failed *OperationFailure `json:"failed,omitempty"`
}

// OperationOutput carries bounded stdout or stderr bytes.
type OperationOutput struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data,omitempty"`
}

// OperationProgress carries one Runtime-defined bounded progress event.
type OperationProgress struct {
	Kind        string `json:"kind"`
	Message     string `json:"message,omitempty"`
	ToolCallID  string `json:"toolCallID,omitempty"`
	ToolName    string `json:"toolName,omitempty"`
	Data        []byte `json:"data,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

// OperationFailure is a terminal gRPC-compatible operation failure.
type OperationFailure struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

// SessionFileInfo identifies one direct child of a workspace-relative directory.
type SessionFileInfo struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	SizeBytes int64  `json:"sizeBytes"`
}

// SessionFilePage is one bounded directory listing response.
type SessionFilePage struct {
	Entries       []SessionFileInfo `json:"entries"`
	NextPageToken string            `json:"nextPageToken"`
}

// FunctionInvocationResult is the bounded result of one Function invocation.
type FunctionInvocationResult struct {
	InvocationID string            `json:"invocationId"`
	Output       []byte            `json:"output,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	Outputs      map[string]string `json:"outputs,omitempty"`
}
