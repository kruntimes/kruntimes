export interface RunSummary {
  name: string;
  namespace: string;
  uid: string;
  runtime: string;
  mode: string;
  phase: string;
  assignedPod?: string;
  attempt?: number;
  creationTimestamp: string;
  lastTransitionReason?: string;
}
export interface RunDetail extends RunSummary {
  spec: Record<string, unknown>;
  status: Record<string, unknown>;
  message?: string;
  conditions?: Array<{
    type: string;
    status: string;
    reason?: string;
    message?: string;
  }>;
  outputs?: Record<string, string>;
  artifactRefs?: Array<{
    name: string;
    driver: string;
    type: string;
    sizeBytes: number;
  }>;
}
export interface LogEntry {
  stream: string;
  message: string;
  invocationId?: string;
  operation?: string;
  outcome?: string;
  statusCode?: string;
  exitCode?: number;
  timedOut?: boolean;
  durationMilliseconds?: number;
}

export type SessionOperationRequest = {
  command?: {
    argv?: string[];
    shell?: string;
    workingDirectory?: string;
    env?: Record<string, string>;
    stdin?: string;
    timeoutMillis?: number;
  };
  writeFile?: {
    path: string;
    contents: string;
    createParents?: boolean;
  };
  createDirectory?: { path: string };
  deleteFile?: { path: string; recursive?: boolean };
  renameFile?: {
    sourcePath: string;
    destinationPath: string;
    overwrite?: boolean;
  };
};

export type SessionOperationEvent = {
  sequence: number;
  type: "accepted" | "output" | "progress" | "completed" | "failed";
  accepted?: { operationID: string };
  output?: { stream: "stdout" | "stderr"; data: string };
  progress?: {
    kind: "status" | "text_delta" | "tool_call_started" | "tool_call_finished";
    message?: string;
    toolCallID?: string;
    toolName?: string;
    data?: string;
    contentType?: string;
  };
  completed?: {
    command?: {
      exitCode: number;
      stdout: string;
      stderr: string;
      timedOut: boolean;
    };
  };
  failed?: { code: number; message: string };
};

export type SessionOperationTransportError = {
  type: "error";
  error: string;
};
export interface RuntimeSummary {
  name: string;
  namespace: string;
  replicas: number;
  readyReplicas: number;
  capacity?: Record<string, string>;
  runCount: number;
  healthy: boolean;
}
export interface RuntimeDetail {
  runtime: RuntimeSummary;
  spec: Record<string, unknown>;
  status: Record<string, unknown>;
  pods: Array<{
    name: string;
    phase: string;
    ready: boolean;
    runtimedReady: boolean;
    runs: RunSummary[];
  }>;
}
export interface WorkflowRunSummary {
  name: string;
  namespace: string;
  uid: string;
  phase: string;
  jobCount: number;
  creationTimestamp: string;
}
export interface WorkflowStepStatus {
  name: string;
  phase: string;
  runName?: string;
  actionSteps?: Array<{ name: string; phase: string; runName?: string }>;
}
export interface WorkflowRunDetail extends WorkflowRunSummary {
  spec: {
    jobs: Record<
      string,
      { needs?: string[]; steps?: Array<{ name?: string; run?: string }> }
    >;
  };
  status: {
    message?: string;
    jobs?: Record<
      string,
      {
        phase: string;
        pre?: string[];
        workflowRunName?: string;
        outputs?: Record<string, string>;
        steps?: WorkflowStepStatus[];
      }
    >;
  };
}
