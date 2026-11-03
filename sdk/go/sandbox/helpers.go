package sandbox

import (
	"context"
	"errors"
)

// CommandOptions selects the incremental output and progress callbacks that
// Sandbox.RunCommand delivers while a command runs.
//
// Each callback returns an error to stop the operation and make RunCommand
// return that error. Callers that need the raw ordered events, or that submit
// several operations over one connection, use Session.Exec instead.
type CommandOptions struct {
	// OnStdout receives stdout bytes as they arrive.
	OnStdout func([]byte) error
	// OnStderr receives stderr bytes as they arrive.
	OnStderr func([]byte) error
	// OnProgress receives Runtime-defined progress events as they arrive.
	OnProgress func(OperationProgress) error
}

// WriteFileOptions configures Sandbox.WriteFile.
type WriteFileOptions struct {
	// CreateParents creates missing parent directories for the written file.
	CreateParents bool
}

// DeleteFileOptions configures Sandbox.DeleteFile.
type DeleteFileOptions struct {
	// Recursive removes a directory and everything below it.
	Recursive bool
}

// RenameFileOptions configures Sandbox.RenameFile.
type RenameFileOptions struct {
	// Overwrite replaces an existing destination path.
	Overwrite bool
}

// RunCommand executes one command in this Sandbox and returns its bounded
// terminal result. It opens a short-lived Session connection, submits one
// command operation, delivers incremental output and progress through options,
// and closes the connection before returning.
//
// A non-zero exit code or a timed-out command is a normal terminal result
// (CommandResult.ExitCode and CommandResult.TimedOut), not an error. A failed
// operation returns *OperationFailedError, an uncertain connection failure
// returns *TransportError, and a canceled context returns an error matching
// errors.Is(err, context.Canceled) after sending the operation-ID cancel frame.
// RunCommand never replays the command after a transport failure and never
// releases the Sandbox; call Release for that.
func (s *Sandbox) RunCommand(ctx context.Context, command Command, options *CommandOptions) (*CommandResult, error) {
	var opts CommandOptions
	if options != nil {
		opts = *options
	}
	session, err := s.OpenSession(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	result, err := session.Exec(ctx, Operation{Command: &command}, &ExecOptions{
		OnEvent: func(event OperationEvent) error {
			if output := event.Output; output != nil {
				switch output.Stream {
				case "stdout":
					if opts.OnStdout != nil {
						return opts.OnStdout(output.Data)
					}
				case "stderr":
					if opts.OnStderr != nil {
						return opts.OnStderr(output.Data)
					}
				}
				return nil
			}
			if progress := event.Progress; progress != nil && opts.OnProgress != nil {
				return opts.OnProgress(*progress)
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	if result.Command == nil {
		return nil, errors.New("Session command completed without a command result")
	}
	return result.Command, nil
}

// WriteFile writes one file relative to the Session workspace. It opens a
// short-lived Session connection, submits one writeFile operation, and closes
// the connection before returning. Writes are never replayed after an uncertain
// transport outcome; the error taxonomy matches RunCommand.
func (s *Sandbox) WriteFile(ctx context.Context, path string, contents []byte, options *WriteFileOptions) error {
	operation := Operation{WriteFile: &FileWrite{Path: path, Contents: contents}}
	if options != nil {
		operation.WriteFile.CreateParents = options.CreateParents
	}
	return s.executeOperation(ctx, operation)
}

// CreateDirectory creates one directory relative to the Session workspace. It
// opens a short-lived Session connection, submits one createDirectory
// operation, and closes the connection before returning.
func (s *Sandbox) CreateDirectory(ctx context.Context, path string) error {
	return s.executeOperation(ctx, Operation{CreateDirectory: &DirectoryCreate{Path: path}})
}

// DeleteFile deletes one path relative to the Session workspace. It opens a
// short-lived Session connection, submits one deleteFile operation, and closes
// the connection before returning. Deletes are never replayed after an
// uncertain transport outcome; the error taxonomy matches RunCommand.
func (s *Sandbox) DeleteFile(ctx context.Context, path string, options *DeleteFileOptions) error {
	operation := Operation{DeleteFile: &FileDelete{Path: path}}
	if options != nil {
		operation.DeleteFile.Recursive = options.Recursive
	}
	return s.executeOperation(ctx, operation)
}

// RenameFile renames one path relative to the Session workspace. It opens a
// short-lived Session connection, submits one renameFile operation, and closes
// the connection before returning. Renames are never replayed after an
// uncertain transport outcome; the error taxonomy matches RunCommand.
func (s *Sandbox) RenameFile(ctx context.Context, source, destination string, options *RenameFileOptions) error {
	operation := Operation{RenameFile: &FileRename{SourcePath: source, DestinationPath: destination}}
	if options != nil {
		operation.RenameFile.Overwrite = options.Overwrite
	}
	return s.executeOperation(ctx, operation)
}

// executeOperation submits exactly one file operation over its own short-lived
// Session connection. It never releases the Sandbox or replays the operation.
func (s *Sandbox) executeOperation(ctx context.Context, operation Operation) error {
	session, err := s.OpenSession(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	_, err = session.Exec(ctx, operation, nil)
	return err
}
