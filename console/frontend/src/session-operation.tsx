import { useEffect, useRef, useState } from "react";
import { type ConsoleAPI, type SessionOperationSocket } from "./api";
import type { RunDetail, SessionOperationEvent } from "./types";
import { ui } from "./ui";

type OperationState =
  "idle" | "connecting" | "streaming" | "completed" | "failed" | "cancelled";

const decodeBase64 = (value?: string) => {
  if (!value) return "";
  try {
    const bytes = Uint8Array.from(atob(value), (character) =>
      character.charCodeAt(0),
    );
    return new TextDecoder().decode(bytes);
  } catch {
    return value;
  }
};

const eventText = (event: SessionOperationEvent) => {
  switch (event.type) {
    case "accepted":
      return `accepted: ${event.accepted?.operationID || "operation admitted"}`;
    case "output":
      return `[${event.output?.stream || "output"}] ${decodeBase64(event.output?.data)}`;
    case "progress":
      return `[${event.progress?.kind || "progress"}] ${
        event.progress?.message ||
        event.progress?.toolName ||
        decodeBase64(event.progress?.data) ||
        "update received"
      }`;
    case "completed": {
      const command = event.completed?.command;
      if (!command) return "completed";
      const lines = [
        `completed: exit code ${command.exitCode}${command.timedOut ? " (timed out)" : ""}`,
      ];
      const stdout = decodeBase64(command.stdout);
      const stderr = decodeBase64(command.stderr);
      if (stdout) lines.push(`[stdout] ${stdout}`);
      if (stderr) lines.push(`[stderr] ${stderr}`);
      return lines.join("\n");
    }
    case "failed":
      return `failed: ${event.failed?.message || "Session operation failed"}`;
  }
};

export function SessionOperationPanel({
  api,
  namespace,
  run,
}: {
  api: ConsoleAPI;
  namespace: string;
  run: RunDetail;
}) {
  const [command, setCommand] = useState("");
  const [events, setEvents] = useState<SessionOperationEvent[]>([]);
  const [state, setState] = useState<OperationState>("idle");
  const [error, setError] = useState("");
  const socketRef = useRef<SessionOperationSocket | undefined>(undefined);
  const terminalRef = useRef(false);
  const cancelledRef = useRef(false);

  const closeSocket = () => {
    socketRef.current?.close();
    socketRef.current = undefined;
  };
  useEffect(() => closeSocket, [namespace, run.uid]);

  const execute = () => {
    const shell = command.trim();
    if (!shell || state === "connecting" || state === "streaming") return;
    closeSocket();
    terminalRef.current = false;
    cancelledRef.current = false;
    setEvents([]);
    setError("");
    setState("connecting");
    let socket: SessionOperationSocket | undefined;
    const currentSocket = () => socketRef.current === socket;
    socket = api.openSessionOperation(
      namespace,
      run.runtime,
      run.uid,
      { command: { shell } },
      {
        onEvent: (event) => {
          if (!currentSocket() || cancelledRef.current) return;
          setEvents((current) => [...current, event]);
          if (event.type === "completed") {
            terminalRef.current = true;
            setState("completed");
          } else if (event.type === "failed") {
            terminalRef.current = true;
            setState("failed");
          } else {
            setState("streaming");
          }
        },
        onError: (message) => {
          if (!currentSocket() || cancelledRef.current) return;
          terminalRef.current = true;
          setError(message);
          setState("failed");
        },
        onClose: () => {
          if (!currentSocket()) return;
          socketRef.current = undefined;
          if (!terminalRef.current && !cancelledRef.current) {
            terminalRef.current = true;
            setError("Session operation connection closed before completion");
            setState("failed");
          }
        },
      },
      sessionLeaseTimeout(run.spec),
    );
    socketRef.current = socket;
  };
  const cancel = () => {
    cancelledRef.current = true;
    socketRef.current?.cancel();
    setState("cancelled");
  };
  const isActive = state === "connecting" || state === "streaming";
  const ready = run.phase === "Ready";

  return (
    <section aria-labelledby="session-operation-heading">
      <h2 id="session-operation-heading">Session operation</h2>
      <p>
        Execute a shell command in this Session Run. Commands require a ready
        Session and use the Console&apos;s authenticated connection.
      </p>
      <label htmlFor="session-shell-command">Shell command</label>
      <textarea
        id="session-shell-command"
        aria-label="Session shell command"
        disabled={!ready || isActive}
        value={command}
        onChange={(event) => setCommand(event.target.value)}
        placeholder="for example: pwd && ls -la"
      />
      <div className="flex flex-wrap items-center gap-3">
        <button
          className={ui.primaryButton}
          disabled={!ready || !command.trim() || isActive}
          onClick={execute}
        >
          {state === "idle" ? "Run command" : "Run again"}
        </button>
        {state === "streaming" && (
          <button className={ui.button} onClick={cancel}>
            Cancel operation
          </button>
        )}
        <span aria-live="polite" className="text-sm text-[var(--muted)]">
          {!ready
            ? `Session must be Ready (current phase: ${run.phase || "Unknown"}).`
            : state === "idle"
              ? "No operation is running."
              : `Operation ${state}.`}
        </span>
      </div>
      {error && (
        <p aria-live="assertive" className="text-[var(--danger)]">
          {error}
        </p>
      )}
      <pre aria-label="Session operation events">
        {events.length
          ? events.map(eventText).join("\n")
          : "Operation events will appear here."}
      </pre>
    </section>
  );
}

function sessionLeaseTimeout(spec: Record<string, unknown>): number | undefined {
  const mode = spec.mode;
  if (!mode || typeof mode !== "object") return undefined;
  const session = (mode as Record<string, unknown>).session;
  if (!session || typeof session !== "object") return undefined;
  const value = (session as Record<string, unknown>).leaseTimeoutSeconds;
  return typeof value === "number" && value > 0 ? value : undefined;
}
