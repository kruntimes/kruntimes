import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { LogEntry, WorkflowStepStatus } from "./types";
import { ui } from "./ui";
import { LogViewer, StatusIcon } from "./workflow-ui";

type WorkflowJobDetailProps = {
  namespace: string;
  jobName: string;
  phase: string;
  result?: string;
  steps: WorkflowStepStatus[];
  loadLogs: (namespace: string, runName: string) => Promise<LogEntry[]>;
  runURL: (namespace: string, runName: string) => string;
};

export function WorkflowJobDetail({
  namespace,
  jobName,
  phase,
  result,
  steps,
  loadLogs,
  runURL,
}: WorkflowJobDetailProps) {
  const [query, setQuery] = useState("");
  const [refreshKey, setRefreshKey] = useState(0);
  return (
    <>
      <header className="flex min-h-[8.75rem] items-start justify-between gap-6 border-b border-[var(--line)] pb-5 max-md:flex-col">
        <div>
          <h1 className="mb-0 text-[1.4rem]">{jobName}</h1>
          <p className="mb-0 mt-2">
            <StatusIcon value={phase} /> {phase.toLocaleLowerCase()} · workflow
            started {result ? `result: ${result}` : "no result reported"}
          </p>
        </div>
        <div className="flex items-center gap-2 max-md:flex-wrap">
          <label className={ui.searchField}>
            <span aria-hidden="true">⌕</span>
            <input
              aria-label="Search logs"
              className={ui.textInput}
              onChange={(event) => setQuery(event.target.value)}
              placeholder="Search logs…"
              value={query}
            />
          </label>
          <button
            aria-label="Refresh job details"
            className={ui.iconButton}
            onClick={() => setRefreshKey((value) => value + 1)}
            title="Refresh displayed logs"
          >
            ↻
          </button>
        </div>
      </header>
      <section className="mt-0" aria-label="Job steps">
        {steps.length ? (
          steps.map((step) => (
            <WorkflowStep
              autoExpand={phase === "Failed" && step.phase === "Failed"}
              key={`${step.name}-${refreshKey}`}
              loadLogs={loadLogs}
              logQuery={query}
              namespace={namespace}
              runURL={runURL}
              step={step}
            />
          ))
        ) : (
          <p>No steps have started.</p>
        )}
      </section>
    </>
  );
}

function WorkflowStep({
  namespace,
  step,
  autoExpand,
  logQuery,
  loadLogs,
  runURL,
}: {
  namespace: string;
  step: WorkflowStepStatus;
  autoExpand: boolean;
  logQuery: string;
  loadLogs: WorkflowJobDetailProps["loadLogs"];
  runURL: WorkflowJobDetailProps["runURL"];
}) {
  const logSources = useMemo(
    () => [
      ...(step.runName
        ? [{ name: step.name, runName: step.runName, action: false }]
        : []),
      ...(step.actionSteps
        ?.filter((action) => action.runName)
        .map((action) => ({
          name: action.name,
          runName: action.runName!,
          action: true,
        })) ?? []),
    ],
    [step.actionSteps, step.name, step.runName],
  );
  const logSourceKey = logSources.map((source) => source.runName).join(",");
  const [logsByRun, setLogsByRun] = useState<Record<string, LogEntry[]>>({});
  const [loading, setLoading] = useState(autoExpand);
  const [error, setError] = useState("");
  const details = useRef<HTMLDetailsElement>(null);
  useEffect(() => {
    if (autoExpand) details.current?.setAttribute("open", "");
  }, [autoExpand]);
  const showLogs = useCallback(() => {
    const missingSources = logSources.filter(
      (source) => logsByRun[source.runName] === undefined,
    );
    if (!missingSources.length || loading) return;
    setError("");
    setLoading(true);
    Promise.all(
      missingSources.map(async (source) => ({
        runName: source.runName,
        entries: await loadLogs(namespace, source.runName),
      })),
    )
      .then((results) =>
        setLogsByRun((current) => ({
          ...current,
          ...Object.fromEntries(
            results.map(({ runName, entries }) => [runName, entries]),
          ),
        })),
      )
      .catch((cause) => setError((cause as Error).message))
      .finally(() => setLoading(false));
  }, [loadLogs, loading, logSources, logsByRun, namespace]);
  useEffect(() => {
    if (!autoExpand || !logSourceKey) return;
    let alive = true;
    Promise.all(
      logSources.map(async (source) => ({
        runName: source.runName,
        entries: await loadLogs(namespace, source.runName),
      })),
    )
      .then(
        (results) =>
          alive &&
          setLogsByRun((current) => ({
            ...current,
            ...Object.fromEntries(
              results.map(({ runName, entries }) => [runName, entries]),
            ),
          })),
      )
      .catch((cause) => alive && setError((cause as Error).message))
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
    };
  }, [autoExpand, loadLogs, logSourceKey, logSources, namespace]);
  return (
    <details
      className="step"
      ref={details}
      onToggle={(event) => {
        if (event.currentTarget.open) showLogs();
      }}
    >
      <summary>
        <span className="step-name">
          <StatusIcon value={step.phase} />
          {step.name}
        </span>
        <small>{step.phase}</small>
      </summary>
      <div className="step-body">
        {logSources.length ? (
          <>
            {loading ? (
              <pre>Loading logs…</pre>
            ) : error ? (
              <p className="text-[var(--danger)]">{error}</p>
            ) : (
              logSources.map((source) => (
                <div key={source.runName}>
                  {source.action && <p>{source.name}</p>}
                  <p>
                    <a href={runURL(namespace, source.runName)}>
                      Open Run {source.runName}
                    </a>
                  </p>
                  <LogViewer
                    entries={logsByRun[source.runName]}
                    query={logQuery}
                  />
                </div>
              ))
            )}
          </>
        ) : (
          <p>No Run created yet.</p>
        )}
        {step.actionSteps
          ?.filter((action) => !action.runName)
          .map((action) => (
            <p key={action.name}>
              {action.name}:{" "}
              {action.runName ? (
                <a href={runURL(namespace, action.runName)}>{action.runName}</a>
              ) : (
                action.phase
              )}
            </p>
          ))}
      </div>
    </details>
  );
}
