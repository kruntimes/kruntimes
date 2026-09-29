import type { LogEntry } from "./types";

export function StatusIcon({ value }: { value: string }) {
  const symbol =
    value === "Succeeded" || value === "Ready" || value === "Healthy"
      ? "✓"
      : value === "Failed" || value === "Timeout" || value === "Unhealthy"
        ? "×"
        : value === "Running"
          ? "◌"
          : value === "Cancelled"
            ? "■"
            : value === "Skipped"
              ? "–"
              : "◷";
  return (
    <span
      aria-label={value || "Pending"}
      className={`status-icon ${value || "Pending"}`}
      role="img"
    >
      {symbol}
    </span>
  );
}

export function LogViewer({
  entries,
  query,
}: {
  entries?: LogEntry[];
  query: string;
}) {
  if (entries === undefined) return <pre>Logs have not been loaded.</pre>;
  const lines = entries.flatMap((entry) =>
    `[${entry.stream}] ${entry.message}`.split("\n"),
  );
  const matching = query.trim().toLocaleLowerCase();
  const visible = lines
    .map((line, index) => ({ line, number: index + 1 }))
    .filter(
      ({ line }) => !matching || line.toLocaleLowerCase().includes(matching),
    );
  return (
    <div className="log-viewer" tabIndex={0}>
      {visible.length ? (
        visible.map(({ line, number }) => (
          <div
            className={
              /error|failed|fatal/i.test(line) ? "log-line error" : "log-line"
            }
            key={`${number}-${line}`}
          >
            <span aria-hidden="true" className="log-line-number">
              {number}
            </span>
            <code>{line}</code>
          </div>
        ))
      ) : (
        <div className="log-line">No matching structured logs.</div>
      )}
    </div>
  );
}
