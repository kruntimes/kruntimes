import type {
  LogEntry,
  RunDetail,
  RunSummary,
  RuntimeDetail,
  RuntimeSummary,
  SessionOperationEvent,
  SessionOperationRequest,
  SessionOperationTransportError,
  WorkflowRunDetail,
  WorkflowRunSummary,
} from "./types";

export type ConsoleSession = {
  authenticated: boolean;
  accountName?: string;
};

export type SessionOperationSocket = {
  cancel(): void;
  close(): void;
};

export type SessionOperationSocketHandlers = {
  onEvent(event: SessionOperationEvent): void;
  onError(message: string): void;
  onClose(): void;
};

export class ConsoleAPI {
  private async request(
    path: string,
    init: RequestInit = {},
  ): Promise<Response> {
    const response = await fetch(path, {
      ...init,
      credentials: "same-origin",
      headers: { ...init.headers },
    });
    if (!response.ok) {
      let message = `Request failed (${response.status})`;
      try {
        message =
          ((await response.json()) as { error?: string }).error || message;
      } catch {
        // Keep the safe status-derived message for non-JSON error responses.
      }
      throw new Error(message);
    }
    return response;
  }

  async connect(token: string): Promise<ConsoleSession> {
    return (await (
      await this.request("/api/session", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
      })
    ).json()) as ConsoleSession;
  }
  async session(): Promise<ConsoleSession> {
    return (await (
      await this.request("/api/session")
    ).json()) as ConsoleSession;
  }
  async disconnect(): Promise<void> {
    await this.request("/api/session", { method: "DELETE" });
  }

  async namespaces(): Promise<string[]> {
    return (
      (await (await this.request("/api/namespaces")).json()) as {
        items: string[];
      }
    ).items;
  }

  async runs(namespace: string): Promise<RunSummary[]> {
    return (
      (await (
        await this.request(
          `/api/namespaces/${encodeURIComponent(namespace)}/runs?limit=200`,
        )
      ).json()) as { items: RunSummary[] }
    ).items;
  }

  async run(namespace: string, name: string): Promise<RunDetail> {
    return (await (
      await this.request(
        `/api/namespaces/${encodeURIComponent(namespace)}/runs/${encodeURIComponent(name)}`,
      )
    ).json()) as RunDetail;
  }

  async logs(namespace: string, name: string): Promise<LogEntry[]> {
    return (
      (await (
        await this.request(
          `/api/namespaces/${encodeURIComponent(namespace)}/runs/${encodeURIComponent(name)}/logs?tail=100`,
        )
      ).json()) as { items: LogEntry[] }
    ).items;
  }

  openSessionOperation(
    namespace: string,
    runtime: string,
    runUID: string,
    request: SessionOperationRequest,
    handlers: SessionOperationSocketHandlers,
    leaseTimeoutSeconds?: number,
  ): SessionOperationSocket {
    const scheme = location.protocol === "https:" ? "wss:" : "ws:";
    const endpoint = `${scheme}//${location.host}/v1/namespaces/${encodeURIComponent(namespace)}/runtimes/${encodeURIComponent(runtime)}/sessions/${encodeURIComponent(runUID)}/operations:ws`;
    const socket = new WebSocket(endpoint);
    let lastSequence = 0;
    let closed = false;
    let operationID = "";
    let heartbeat: number | undefined;
    socket.addEventListener("open", () => {
      socket.send(JSON.stringify({ type: "send", operation: request }));
      if (leaseTimeoutSeconds && leaseTimeoutSeconds > 0) {
        const interval = Math.min(
          Math.max((leaseTimeoutSeconds * 1000) / 3, 100),
          30000,
        );
        heartbeat = window.setInterval(() => {
          if (socket.readyState === WebSocket.OPEN)
            socket.send(JSON.stringify({ type: "heartbeat" }));
        }, interval);
      }
    });
    socket.addEventListener("message", (message) => {
      let value: SessionOperationEvent | SessionOperationTransportError;
      try {
        value = JSON.parse(String(message.data)) as
          SessionOperationEvent | SessionOperationTransportError;
      } catch {
        handlers.onError("Console received an invalid Session operation event");
        socket.close();
        return;
      }
      if (value.type === "error") {
        handlers.onError(value.error || "Session operation stream failed");
        return;
      }
      if (value.type === "accepted" && value.accepted?.operationID)
        operationID = value.accepted.operationID;
      if (
        !Number.isSafeInteger(value.sequence) ||
        value.sequence !== lastSequence + 1
      ) {
        handlers.onError(
          `Session operation event sequence ${value.sequence} follows ${lastSequence}`,
        );
        socket.close();
        return;
      }
      lastSequence = value.sequence;
      handlers.onEvent(value);
    });
    socket.addEventListener("error", () =>
      handlers.onError("Session operation WebSocket connection failed"),
    );
    socket.addEventListener("close", () => {
      if (heartbeat !== undefined) window.clearInterval(heartbeat);
      if (!closed) {
        closed = true;
        handlers.onClose();
      }
    });
    return {
      cancel: () => {
        if (socket.readyState === WebSocket.OPEN && operationID)
          socket.send(JSON.stringify({ type: "cancel", operationID }));
      },
      close: () => socket.close(),
    };
  }

  async followLogs(
    namespace: string,
    name: string,
    signal: AbortSignal,
    onEntry: (entry: LogEntry) => void,
  ): Promise<void> {
    const response = await this.request(
      `/api/namespaces/${encodeURIComponent(namespace)}/runs/${encodeURIComponent(name)}/logs?tail=100&follow=true`,
      { signal },
    );
    if (!response.body)
      throw new Error("Log streaming is not supported by this browser");
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let pending = "";
    for (;;) {
      const { done, value } = await reader.read();
      pending += decoder.decode(value || new Uint8Array(), { stream: !done });
      const lines = pending.split("\n");
      pending = lines.pop() || "";
      for (const line of lines) if (line) onEntry(JSON.parse(line) as LogEntry);
      if (done) break;
    }
  }
  async runtimes(namespace: string): Promise<RuntimeSummary[]> {
    return (
      (await (
        await this.request(
          `/api/namespaces/${encodeURIComponent(namespace)}/runtimes`,
        )
      ).json()) as { items: RuntimeSummary[] }
    ).items;
  }
  async runtime(namespace: string, name: string): Promise<RuntimeDetail> {
    return (await (
      await this.request(
        `/api/namespaces/${encodeURIComponent(namespace)}/runtimes/${encodeURIComponent(name)}`,
      )
    ).json()) as RuntimeDetail;
  }
  async workflowRuns(namespace: string): Promise<WorkflowRunSummary[]> {
    return (
      (await (
        await this.request(
          `/api/namespaces/${encodeURIComponent(namespace)}/workflowruns`,
        )
      ).json()) as { items: WorkflowRunSummary[] }
    ).items;
  }
  async workflowRun(
    namespace: string,
    name: string,
  ): Promise<WorkflowRunDetail> {
    return (await (
      await this.request(
        `/api/namespaces/${encodeURIComponent(namespace)}/workflowruns/${encodeURIComponent(name)}`,
      )
    ).json()) as WorkflowRunDetail;
  }
}
