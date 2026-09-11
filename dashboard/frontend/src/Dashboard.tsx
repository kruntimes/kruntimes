import { createContext, useContext, useEffect, useRef, useState } from "react";
import { DashboardAPI, type DashboardSession } from "./api";
import type { LogEntry, WorkflowRunDetail } from "./types";
import { ui } from "./ui";
import {
  styles,
  initialStyle,
  initialTheme,
  selectStyle,
  selectTheme,
  type UIStyle,
  type Theme,
} from "./appearance";
import { StatusIcon } from "./workflow-ui";
import { WorkflowDAG } from "./workflow-dag";
import { WorkflowJobDetail } from "./workflow-job-detail";
import { SidebarIcon } from "./sidebar-icons";

const api = new DashboardAPI();
const loadRunLogs = (namespace: string, runName: string) =>
  api.logs(namespace, runName);
const age = (value?: string) => {
  if (!value) return "—";
  const seconds = Math.max(
    0,
    Math.round((Date.now() - new Date(value).getTime()) / 1000),
  );
  for (const [unit, size] of [
    ["y", 31536000],
    ["mo", 2592000],
    ["w", 604800],
    ["d", 86400],
    ["h", 3600],
    ["m", 60],
  ] as const)
    if (seconds >= size) return `${Math.floor(seconds / size)}${unit}`;
  return `${seconds}s`;
};
const pathParts = () =>
  location.pathname.split("/").filter(Boolean).map(decodeURIComponent);
const runURL = (namespace: string, name: string) =>
  `/namespaces/${encodeURIComponent(namespace)}/runs/${encodeURIComponent(name)}`;
const workflowURL = (namespace: string, name: string) =>
  `/namespaces/${encodeURIComponent(namespace)}/workflowruns/${encodeURIComponent(name)}`;
const workflowJobURL = (namespace: string, workflow: string, job: string) =>
  `${workflowURL(namespace, workflow)}/jobs/${encodeURIComponent(job)}`;
const formattedDate = (value?: string) =>
  value
    ? new Intl.DateTimeFormat(undefined, {
        dateStyle: "medium",
        timeStyle: "short",
      }).format(new Date(value))
    : "—";
const workflowStatusLabel = (phase: string) => {
  switch (phase) {
    case "Succeeded":
      return "Successfully completed";
    case "Failed":
      return "Failed";
    case "Running":
      return "In progress";
    case "Cancelled":
      return "Cancelled";
    default:
      return "Queued";
  }
};
const NoticeContext = createContext<(message: string) => void>(() => undefined);
const anonymousSession: DashboardSession = { authenticated: false };

const accountLabel = (accountName: string) => {
  const serviceAccount = accountName.match(
    /^system:serviceaccount:([^:]+):([^:]+)$/,
  );
  return serviceAccount
    ? `${serviceAccount[1]}/${serviceAccount[2]}`
    : accountName;
};

export function Dashboard() {
  const [path, setPath] = useState(pathParts());
  const [theme, setTheme] = useState<Theme>(initialTheme);
  const [style, setStyle] = useState<UIStyle>(initialStyle);
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [error, setError] = useState("");
  const [token, setToken] = useState("");
  const [session, setSession] = useState<DashboardSession>(anonymousSession);
  const [loginOpen, setLoginOpen] = useState(false);
  const [loginError, setLoginError] = useState("");
  const [loggingIn, setLoggingIn] = useState(false);
  const [notice, setNotice] = useState("");
  useEffect(() => {
    const update = () => setPath(pathParts());
    addEventListener("popstate", update);
    return () => removeEventListener("popstate", update);
  }, []);
  useEffect(() => {
    api
      .session()
      .then(setSession)
      .catch(() => setSession(anonymousSession));
  }, []);
  useEffect(() => {
    api
      .namespaces()
      .then((items) => {
        setNamespaces(items);
        setError("");
      })
      .catch((cause) => setError((cause as Error).message));
  }, [session.authenticated]);
  const connect = async () => {
    setLoggingIn(true);
    setLoginError("");
    try {
      const nextSession = await api.connect(token.trim());
      setToken("");
      setSession(nextSession);
      setLoginOpen(false);
      setError("");
    } catch (cause) {
      setLoginError((cause as Error).message);
    } finally {
      setLoggingIn(false);
    }
  };
  const namespace =
    path[0] === "namespaces" ? path[1] : namespaces[0] || "default";
  const workflowDetail =
    path[0] === "namespaces" && path[2] === "workflowruns" && Boolean(path[3]);
  return (
    <NoticeContext.Provider value={setNotice}>
      <div
        className={
          workflowDetail
            ? "min-h-screen"
            : "grid min-h-screen grid-cols-[210px_minmax(0,1fr)] max-md:grid-cols-1"
        }
      >
        {!workflowDetail && (
          <Sidebar namespace={namespace} namespaces={namespaces} />
        )}
        <div className="min-w-0">
          <Header
            session={session}
            onLogin={() => {
              setLoginError("");
              setLoginOpen(true);
            }}
            onDisconnect={async () => {
              await api.disconnect();
              setSession(anonymousSession);
            }}
          />
          {path[0] === "settings" ? (
            <Settings
              style={style}
              onStyle={(value) => {
                selectStyle(value);
                setStyle(value);
              }}
              theme={theme}
              onTheme={(value) => {
                selectTheme(value);
                setTheme(value);
              }}
            />
          ) : path[0] === "about" ? (
            <About />
          ) : path[0] !== "namespaces" ? (
            <Home namespaces={namespaces} />
          ) : (
            <Page namespace={namespace} path={path.slice(2)} />
          )}
          {error && (
            <p className="px-8 text-[var(--danger)]" role="alert">
              {error}
            </p>
          )}
        </div>
      </div>
      {notice && (
        <NoticeDialog message={notice} onDismiss={() => setNotice("")} />
      )}
      {loginOpen && (
        <LoginDialog
          token={token}
          error={loginError}
          submitting={loggingIn}
          onToken={setToken}
          onSubmit={connect}
          onDismiss={() => {
            setLoginOpen(false);
            setLoginError("");
          }}
        />
      )}
    </NoticeContext.Provider>
  );
}

function Header({
  session,
  onLogin,
  onDisconnect,
}: {
  session: DashboardSession;
  onLogin: () => void;
  onDisconnect: () => void;
}) {
  return (
    <header className="flex flex-wrap min-h-24 items-center justify-between gap-4 bg-[var(--surface)] px-8 py-3 max-md:px-4">
      <a className="text-lg font-bold text-[var(--text)] no-underline" href="/">
        kruntimes{" "}
        <span className="font-medium text-[var(--link)]">Dashboard</span>
      </a>
      {session.authenticated ? (
        <div className="flex min-w-0 items-center gap-2">
          <span
            className="max-w-56 truncate text-sm text-[var(--muted)]"
            title={session.accountName}
          >
            {session.accountName
              ? accountLabel(session.accountName)
              : "Authenticated"}
          </span>
          <button className={ui.button} onClick={onDisconnect}>
            Log out
          </button>
        </div>
      ) : (
        <button className={ui.primaryButton} onClick={onLogin}>
          Log in
        </button>
      )}
    </header>
  );
}
function Sidebar({
  namespace,
  namespaces,
}: {
  namespace: string;
  namespaces: string[];
}) {
  const root = `/namespaces/${encodeURIComponent(namespace)}`;
  const navClass = (path: string) =>
    `${ui.navItem} ${location.pathname.startsWith(path) ? ui.selectedNavItem : ""}`;
  const resourceNavigation = [
    { label: "Runs", path: `${root}/runs`, icon: "runs" },
    { label: "Runtimes", path: `${root}/runtimes`, icon: "runtimes" },
    {
      label: "Workflow Runs",
      path: `${root}/workflowruns`,
      icon: "workflowruns",
    },
  ] as const;
  const dashboardNavigation = [
    { label: "Settings", path: "/settings", icon: "settings" },
    { label: "About", path: "/about", icon: "about" },
  ] as const;
  return (
    <aside className="sidebar grid content-start gap-4 bg-[var(--surface)] p-5 max-md:grid-cols-2">
      <section className="sidebar-group max-md:col-span-2">
        <h2 className="sidebar-group-title">Kruntimes resources</h2>
        <label>
          Namespace
          <select
            value={namespace}
            onChange={(event) => {
              location.assign(
                `/namespaces/${encodeURIComponent(event.target.value)}/runs`,
              );
            }}
          >
            {namespaces.map((item) => (
              <option key={item} value={item}>
                {item}
              </option>
            ))}
          </select>
        </label>
        <nav className="sidebar-nav" aria-label="Kruntimes resources">
          {resourceNavigation.map((item) => (
            <a className={navClass(item.path)} href={item.path} key={item.path}>
              <SidebarIcon name={item.icon} />
              <span>{item.label}</span>
            </a>
          ))}
        </nav>
      </section>
      <section className="sidebar-group max-md:col-span-2">
        <h2 className="sidebar-group-title">Dashboard</h2>
        <nav className="sidebar-nav" aria-label="Dashboard">
          {dashboardNavigation.map((item) => (
            <a className={navClass(item.path)} href={item.path} key={item.path}>
              <SidebarIcon name={item.icon} />
              <span>{item.label}</span>
            </a>
          ))}
        </nav>
      </section>
    </aside>
  );
}
function Home({ namespaces }: { namespaces: string[] }) {
  return (
    <main className="mx-auto max-w-[90rem] p-8 max-md:p-4">
      <section className={`${ui.panel} p-5`}>
        <h1>Cluster overview</h1>
        <p>
          Select a namespace to browse Runs, Runtime pools, and WorkflowRuns.
        </p>
        <div className="flex flex-wrap gap-2">
          {namespaces.map((name) => (
            <a
              className={ui.button}
              href={`/namespaces/${encodeURIComponent(name)}/runs`}
              key={name}
            >
              {name}
            </a>
          ))}
        </div>
      </section>
    </main>
  );
}
function About() {
  return (
    <main className="mx-auto max-w-[90rem] p-8 max-md:p-4">
      <section className={`${ui.panel} p-5`}>
        <h1>About kruntimes</h1>
        <p>
          kruntimes runs workloads on Kubernetes with warm Runtime Pods and a
          Run-based control plane, reducing startup latency while preserving
          Kubernetes-native scheduling and authorization.
        </p>
        <p>
          <a href="https://kruntimes.io/docs/" target="_blank" rel="noreferrer">
            Read the public kruntimes documentation ↗
          </a>
        </p>
      </section>
    </main>
  );
}
function Settings({
  style,
  onStyle,
  theme,
  onTheme,
}: {
  style: UIStyle;
  onStyle: (style: UIStyle) => void;
  theme: Theme;
  onTheme: (theme: Theme) => void;
}) {
  return (
    <main className="mx-auto max-w-[90rem] p-8 max-md:p-4">
      <section className={`${ui.panel} max-w-2xl p-5`}>
        <h1>Settings</h1>
        <p>
          Appearance preferences are applied immediately and saved in this
          browser.
        </p>
        <div className="mt-6 grid gap-5">
          <label>
            Style
            <select
              value={style}
              onChange={(event) => onStyle(event.target.value as UIStyle)}
            >
              {styles.map((item) => (
                <option key={item.id} value={item.id}>
                  {item.label}
                </option>
              ))}
            </select>
          </label>
          <label>
            Theme
            <select
              value={theme}
              onChange={(event) => onTheme(event.target.value as Theme)}
            >
              <option value="system">System</option>
              <option value="light">Light</option>
              <option value="dark">Dark</option>
            </select>
            <small>System follows your operating system preference.</small>
          </label>
        </div>
      </section>
    </main>
  );
}
function Page({ namespace, path }: { namespace: string; path: string[] }) {
  if (path[0] === "runs" && path[1])
    return <RunPage namespace={namespace} name={path[1]} />;
  if (path[0] === "runtimes" && path[1])
    return <RuntimePage namespace={namespace} name={path[1]} />;
  if (path[0] === "workflowruns" && path[1] && path[2] === "jobs" && path[3])
    return (
      <WorkflowJobPage
        namespace={namespace}
        workflow={path[1]}
        jobName={path[3]}
      />
    );
  if (path[0] === "workflowruns" && path[1])
    return <WorkflowPage namespace={namespace} name={path[1]} />;
  if (path[0] === "runtimes") return <RuntimeListPage namespace={namespace} />;
  if (path[0] === "workflowruns")
    return <WorkflowListPage namespace={namespace} />;
  return <RunListPage namespace={namespace} />;
}
function useLoaded<T>(load: () => Promise<T>, keys: unknown[]) {
  const [value, setValue] = useState<T>();
  const [error, setError] = useState("");
  useEffect(() => {
    let alive = true;
    load()
      .then((next) => alive && (setValue(next), setError("")))
      .catch((cause) => alive && setError((cause as Error).message));
    return () => {
      alive = false;
    };
  }, keys);
  return { value, error };
}
function RunListPage({ namespace }: { namespace: string }) {
  const { value: runs = [], error } = useLoaded(
    () => api.runs(namespace),
    [namespace],
  );
  return (
    <main>
      <Title title={`Runs · ${namespace}`} />
      <Error message={error} />
      <Table headers={["Name", "Mode", "Phase", "Runtime", "Pod", "Age"]}>
        {runs.map((run) => (
          <tr key={run.uid}>
            <td>
              <a href={runURL(namespace, run.name)}>{run.name}</a>
            </td>
            <td>{run.mode}</td>
            <td>
              <Phase value={run.phase} />
            </td>
            <td>
              <a
                href={`/namespaces/${encodeURIComponent(namespace)}/runtimes/${encodeURIComponent(run.runtime)}`}
              >
                {run.runtime}
              </a>
            </td>
            <td>{run.assignedPod || "—"}</td>
            <td>{age(run.creationTimestamp)}</td>
          </tr>
        ))}
      </Table>
    </main>
  );
}
function RunPage({ namespace, name }: { namespace: string; name: string }) {
  const { value: run, error } = useLoaded(
    () => api.run(namespace, name),
    [namespace, name],
  );
  const [logs, setLogs] = useState<LogEntry[]>();
  const [logAbort, setLogAbort] = useState<AbortController>();
  const logAbortRef = useRef<AbortController | undefined>(undefined);
  const showNotice = useContext(NoticeContext);
  const stopLogs = () => {
    logAbortRef.current?.abort();
    logAbortRef.current = undefined;
    setLogAbort(undefined);
  };
  const loadLogs = () => {
    stopLogs();
    const abort = new AbortController();
    logAbortRef.current = abort;
    api
      .logs(namespace, name)
      .then(setLogs)
      .then(() => {
        setLogAbort(abort);
        return api.followLogs(namespace, name, abort.signal, (entry) =>
          setLogs((current = []) => [...current, entry]),
        );
      })
      .catch((cause) => {
        if ((cause as Error).name !== "AbortError")
          showNotice((cause as Error).message);
      })
      .finally(() => {
        if (logAbortRef.current === abort) {
          logAbortRef.current = undefined;
          setLogAbort(undefined);
        }
      });
  };
  useEffect(
    () => () => {
      logAbortRef.current?.abort();
    },
    [namespace, name],
  );
  if (!run)
    return (
      <main>
        <Title
          title={`Run · ${name}`}
          back={`/namespaces/${encodeURIComponent(namespace)}/runs`}
        />
        <Error message={error} />
        <section className={`${ui.panel} mx-8 p-5 max-md:mx-4`}>
          <p>
            Run detail requires Kubernetes permission to read this Run. Logs
            only require permission to read its assigned Pod log stream.
          </p>
          <div className="flex items-center justify-between">
            <h2>Logs</h2>
            <button
              className={ui.button}
              onClick={logAbort ? stopLogs : loadLogs}
            >
              {logAbort ? "Stop logs" : "Load logs"}
            </button>
          </div>
          <Logs entries={logs} />
        </section>
      </main>
    );
  return (
    <main>
      <Title
        title={`Run · ${run.name}`}
        back={`/namespaces/${encodeURIComponent(namespace)}/runs`}
      />
      <Error message={error} />
      <section className={`${ui.panel} mx-8 p-5 max-md:mx-4`}>
        <dl>
          <dt>Phase</dt>
          <dd>
            <Phase value={run.phase} />
          </dd>
          <dt>Runtime</dt>
          <dd>
            <a
              href={`/namespaces/${encodeURIComponent(namespace)}/runtimes/${encodeURIComponent(run.runtime)}`}
            >
              {run.runtime}
            </a>
          </dd>
          <dt>Assigned Pod</dt>
          <dd>{run.assignedPod || "—"}</dd>
          <dt>Message</dt>
          <dd>{run.message || "—"}</dd>
        </dl>
        <h2>Spec</h2>
        <JSONValue value={run.spec} />
        <h2>Status</h2>
        <JSONValue value={run.status} />
        <div className="flex items-center justify-between">
          <h2>Logs</h2>
          <button
            className={ui.button}
            onClick={logAbort ? stopLogs : loadLogs}
          >
            {logAbort ? "Stop logs" : "Load logs"}
          </button>
        </div>
        <Logs entries={logs} />
      </section>
    </main>
  );
}
function RuntimeListPage({ namespace }: { namespace: string }) {
  const { value: items = [], error } = useLoaded(
    () => api.runtimes(namespace),
    [namespace],
  );
  return (
    <main>
      <Title title={`Runtimes · ${namespace}`} />
      <Error message={error} />
      <Table headers={["Name", "Health", "Capacity", "Runs", "Ready"]}>
        {items.map((item) => (
          <tr key={item.name}>
            <td>
              <a
                href={`/namespaces/${encodeURIComponent(namespace)}/runtimes/${encodeURIComponent(item.name)}`}
              >
                {item.name}
              </a>
            </td>
            <td>
              <Phase value={item.healthy ? "Healthy" : "Unhealthy"} />
            </td>
            <td>
              {Object.entries(item.capacity || {})
                .map(([key, value]) => `${key}=${value}`)
                .join(", ") || "—"}
            </td>
            <td>{item.runCount}</td>
            <td>
              {item.readyReplicas}/{item.replicas}
            </td>
          </tr>
        ))}
      </Table>
    </main>
  );
}
function RuntimePage({ namespace, name }: { namespace: string; name: string }) {
  const { value, error } = useLoaded(
    () => api.runtime(namespace, name),
    [namespace, name],
  );
  if (!value)
    return (
      <main>
        <Title title={`Runtime · ${name}`} />
        <Error message={error} />
      </main>
    );
  return (
    <main>
      <Title
        title={`Runtime · ${name}`}
        back={`/namespaces/${encodeURIComponent(namespace)}/runtimes`}
      />
      <Error message={error} />
      <section className={`${ui.panel} mx-8 p-5 max-md:mx-4`}>
        <p>
          <Phase value={value.runtime.healthy ? "Healthy" : "Unhealthy"} />{" "}
          {value.runtime.readyReplicas}/{value.runtime.replicas} pods ready ·{" "}
          {value.runtime.runCount} Runs
        </p>
        <h2>Runtime Pods</h2>
        {(value.pods || []).map((pod) => (
          <details key={pod.name} open>
            <summary>
              {pod.name} · {pod.phase} · runtimed{" "}
              {pod.runtimedReady ? "ready" : "not ready"}
            </summary>
            <Table embedded headers={["Run", "Phase", "Runtime"]}>
              {(pod.runs || []).map((run) => (
                <tr key={run.uid}>
                  <td>
                    <a href={runURL(namespace, run.name)}>{run.name}</a>
                  </td>
                  <td>
                    <Phase value={run.phase} />
                  </td>
                  <td>{run.runtime}</td>
                </tr>
              ))}
            </Table>
          </details>
        ))}
        <h2>Spec</h2>
        <JSONValue value={value.spec} />
        <h2>Status</h2>
        <JSONValue value={value.status} />
      </section>
    </main>
  );
}
function WorkflowListPage({ namespace }: { namespace: string }) {
  const { value: items = [], error } = useLoaded(
    () => api.workflowRuns(namespace),
    [namespace],
  );
  return (
    <main>
      <Title title={`Workflow Runs · ${namespace}`} />
      <Error message={error} />
      <Table headers={["Name", "Phase", "Jobs", "Age"]}>
        {items.map((item) => (
          <tr key={item.uid}>
            <td>
              <a
                href={`/namespaces/${encodeURIComponent(namespace)}/workflowruns/${encodeURIComponent(item.name)}`}
              >
                {item.name}
              </a>
            </td>
            <td>
              <Phase value={item.phase} />
            </td>
            <td>{item.jobCount}</td>
            <td>{age(item.creationTimestamp)}</td>
          </tr>
        ))}
      </Table>
    </main>
  );
}
function WorkflowPage({
  namespace,
  name,
}: {
  namespace: string;
  name: string;
}) {
  const { value, error } = useLoaded(
    () => api.workflowRun(namespace, name),
    [namespace, name],
  );
  if (!value)
    return (
      <main>
        <Title title={`Workflow Run · ${name}`} />
        <Error message={error} />
      </main>
    );
  return (
    <WorkflowFrame namespace={namespace} workflow={name} detail={value}>
      <Error message={error} />
      <WorkflowOverview namespace={namespace} workflow={name} detail={value} />
    </WorkflowFrame>
  );
}
function WorkflowFrame({
  namespace,
  workflow,
  detail,
  selectedJob,
  children,
}: {
  namespace: string;
  workflow: string;
  detail: WorkflowRunDetail;
  selectedJob?: string;
  children: React.ReactNode;
}) {
  const names = Object.keys(detail.spec.jobs).sort();
  return (
    <main className="grid min-h-screen grid-cols-[300px_minmax(0,1fr)] p-0 max-lg:grid-cols-1">
      <aside className="grid content-start gap-3 bg-[var(--surface)] px-6 py-7">
        <a
          className="text-sm text-[var(--muted)] no-underline transition hover:text-[var(--link)]"
          href={`/namespaces/${encodeURIComponent(namespace)}/workflowruns`}
        >
          ← Workflows
        </a>
        <h1 className="mb-0 mt-4 text-xl">{workflow}</h1>
        <small className="mt-0">{namespace}</small>
        <a
          className={`${ui.navItem} mt-8 text-sm ${selectedJob ? "" : ui.selectedNavItem}`}
          href={workflowURL(namespace, workflow)}
        >
          <span aria-hidden="true">⌂</span> Summary
        </a>
        <strong className="mt-2 px-3 py-2 text-xs uppercase tracking-wide text-[var(--muted)]">
          Jobs
        </strong>
        <nav aria-label="Workflow jobs" className="grid gap-1">
          {names.map((jobName) => (
            <a
              className={`${ui.navItem} text-sm ${
                jobName === selectedJob ? ui.selectedNavItem : ""
              }`}
              href={workflowJobURL(namespace, workflow, jobName)}
              key={jobName}
            >
              <StatusIcon
                value={detail.status.jobs?.[jobName]?.phase || "Pending"}
              />
              <span>{jobName}</span>
            </a>
          ))}
        </nav>
      </aside>
      <section className="min-w-0 px-10 py-9 max-md:px-4 max-md:py-5">
        {children}
      </section>
    </main>
  );
}
function WorkflowOverview({
  namespace,
  workflow,
  detail,
}: {
  namespace: string;
  workflow: string;
  detail: WorkflowRunDetail;
}) {
  const [query, setQuery] = useState("");
  const [scale, setScale] = useState(1);
  const [tab, setTab] = useState<"pipeline" | "logs" | "artifacts">("pipeline");
  const matchingJobs = new Set(
    Object.keys(detail.spec.jobs).filter((job) =>
      job.toLocaleLowerCase().includes(query.toLocaleLowerCase()),
    ),
  );
  return (
    <>
      <header className="flex flex-wrap min-h-[9.25rem] items-start justify-between gap-8 max-md:flex-col max-md:gap-5">
        <div>
          <p className="m-0 flex items-center gap-3 text-xl text-[var(--text)]">
            <StatusIcon value={detail.phase} />
            <strong>{workflowStatusLabel(detail.phase)}</strong>
          </p>
          {detail.status.message && <p>{detail.status.message}</p>}
        </div>
        <dl className="ml-auto mt-15 flex flex-wrap max-w-full text-sm max-md:ml-0 max-md:mt-0">
          <div className="grid min-w-28 gap-1 border-l border-[var(--line)] px-5">
            <dt>Started</dt>
            <dd>{formattedDate(detail.creationTimestamp)}</dd>
          </div>
          <div className="grid min-w-28 gap-1 border-l border-[var(--line)] px-5">
            <dt>Age</dt>
            <dd>{age(detail.creationTimestamp)}</dd>
          </div>
          <div className="grid min-w-28 gap-1 border-l border-[var(--line)] px-5">
            <dt>Status</dt>
            <dd>
              <StatusIcon value={detail.phase} /> {detail.phase || "Pending"}
            </dd>
          </div>
        </dl>
        <button
          aria-label="More workflow actions"
          className={ui.iconButton}
          title="More actions"
        >
          •••
        </button>
      </header>
      <div className="flex flex-wrap min-h-16 items-center justify-between gap-5 max-md:flex-col max-md:items-stretch">
        <nav className="flex gap-2" aria-label="Workflow views">
          {(["pipeline", "logs", "artifacts"] as const).map((item) => (
            <button
              className={`${ui.button} ${tab === item ? ui.selectedNavItem : ""}`}
              aria-pressed={tab === item}
              key={item}
              onClick={() => setTab(item)}
            >
              {item[0].toUpperCase() + item.slice(1)}
            </button>
          ))}
        </nav>
        {tab === "pipeline" && (
          <div className="flex flex-wrap items-center gap-2 max-md:pb-3">
            <label className={ui.searchField}>
              <span aria-hidden="true">⌕</span>
              <input
                aria-label="Search jobs"
                className={ui.textInput}
                onChange={(event) => setQuery(event.target.value)}
                placeholder="Search jobs…"
                value={query}
              />
            </label>
            <button
              aria-label="Zoom out"
              className={ui.iconButton}
              onClick={() => setScale((value) => Math.max(0.65, value - 0.1))}
              title="Zoom out"
            >
              −
            </button>
            <button
              aria-label="Zoom in"
              className={ui.iconButton}
              onClick={() => setScale((value) => Math.min(1.35, value + 0.1))}
              title="Zoom in"
            >
              +
            </button>
            <button
              className={ui.button}
              onClick={() => setScale(1)}
              title="Fit pipeline"
            >
              Fit
            </button>
          </div>
        )}
      </div>
      {tab === "pipeline" ? (
        <WorkflowDAG
          detail={detail}
          jobURL={(jobName) => workflowJobURL(namespace, workflow, jobName)}
          matchingJobs={matchingJobs}
          scale={scale}
        />
      ) : (
        <section className={`${ui.panel} mt-6 min-h-[22.5rem] p-12`}>
          <h2>{tab[0].toUpperCase() + tab.slice(1)}</h2>
          <p>
            {tab === "logs"
              ? "Workflow-wide log aggregation is not available yet. Open a Job to inspect its step logs."
              : "This WorkflowRun did not report any artifacts."}
          </p>
        </section>
      )}
    </>
  );
}
function WorkflowJobPage({
  namespace,
  workflow,
  jobName,
}: {
  namespace: string;
  workflow: string;
  jobName: string;
}) {
  const { value, error } = useLoaded(
    () => api.workflowRun(namespace, workflow),
    [namespace, workflow],
  );
  if (!value)
    return (
      <main>
        <Title title={`Job · ${jobName}`} />
        <Error message={error} />
      </main>
    );
  const job = value.status.jobs?.[jobName];
  if (!value.spec.jobs[jobName])
    return (
      <main>
        <Title
          title={`Job · ${jobName}`}
          back={workflowURL(namespace, workflow)}
        />
        <section className={`${ui.panel} mx-8 p-5 max-md:mx-4`}>
          <p>Job not found.</p>
        </section>
      </main>
    );
  return (
    <WorkflowFrame
      detail={value}
      namespace={namespace}
      selectedJob={jobName}
      workflow={workflow}
    >
      <Error message={error} />
      <WorkflowJobDetail
        jobName={jobName}
        namespace={namespace}
        phase={job?.phase || "Pending"}
        result={job?.outputs?.result}
        loadLogs={loadRunLogs}
        runURL={runURL}
        steps={job?.steps || []}
      />
    </WorkflowFrame>
  );
}
function Title({ title, back }: { title: string; back?: string }) {
  return (
    <div className="px-8 pt-8 max-md:px-4">
      <div>
        {back && (
          <a className="text-sm" href={back}>
            ← Back
          </a>
        )}
        <h1 className="mt-1 text-2xl">{title}</h1>
      </div>
    </div>
  );
}
function Table({
  embedded = false,
  headers,
  children,
}: {
  embedded?: boolean;
  headers: string[];
  children: React.ReactNode;
}) {
  return (
    <section
      className={`${ui.panel} ${embedded ? "ui-embedded" : ""} mx-8 my-4 overflow-x-auto max-md:mx-4`}
    >
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr>
            {headers.map((header) => (
              <th
                className="border-b border-[var(--line)] p-3 text-left text-xs uppercase tracking-wide text-[var(--muted)]"
                key={header}
              >
                {header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </section>
  );
}
function Phase({ value }: { value: string }) {
  return (
    <span
      className={`inline-flex border border-[var(--line)] px-2 py-0.5 text-xs ${
        value || "Unknown"
      } phase`}
    >
      {value || "Unknown"}
    </span>
  );
}
function JSONValue({ value }: { value: unknown }) {
  return <pre>{JSON.stringify(value, null, 2)}</pre>;
}
function Logs({ entries }: { entries?: LogEntry[] }) {
  return (
    <pre>
      {entries === undefined
        ? "Logs have not been loaded."
        : entries
            .map((entry) => `[${entry.stream}] ${entry.message}`)
            .join("\n") || "No matching structured logs."}
    </pre>
  );
}
function Error({ message }: { message: string }) {
  return message ? (
    <p className="px-8 text-[var(--danger)]">{message}</p>
  ) : null;
}
function NoticeDialog({
  message,
  onDismiss,
}: {
  message: string;
  onDismiss: () => void;
}) {
  return (
    <div
      className="notice-backdrop"
      role="alertdialog"
      aria-modal="true"
      aria-label="Request error"
      onClick={onDismiss}
    >
      <section className={`notice ${ui.panel}`}>
        <h2>Request unavailable</h2>
        <p>{message}</p>
        <small>Click anywhere to dismiss.</small>
      </section>
    </div>
  );
}

function LoginDialog({
  token,
  error,
  submitting,
  onToken,
  onSubmit,
  onDismiss,
}: {
  token: string;
  error: string;
  submitting: boolean;
  onToken: (token: string) => void;
  onSubmit: () => void;
  onDismiss: () => void;
}) {
  return (
    <div className="notice-backdrop" role="presentation" onClick={onDismiss}>
      <section
        className={`notice ${ui.panel}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby="login-title"
        onClick={(event) => event.stopPropagation()}
      >
        <h2 id="login-title">Log in</h2>
        <p>
          Paste a short-lived Kubernetes bearer token to access protected
          details and logs.
        </p>
        <label htmlFor="dashboard-token">Kubernetes bearer token</label>
        <textarea
          id="dashboard-token"
          value={token}
          onChange={(event) => onToken(event.target.value)}
          placeholder="Paste token"
          autoFocus
        />
        {error && (
          <p className="text-[var(--danger)]" role="alert">
            {error}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <button
            className={ui.button}
            onClick={onDismiss}
            disabled={submitting}
          >
            Cancel
          </button>
          <button
            className={ui.primaryButton}
            onClick={onSubmit}
            disabled={submitting || !token.trim()}
          >
            {submitting ? "Logging in…" : "Log in"}
          </button>
        </div>
      </section>
    </div>
  );
}
