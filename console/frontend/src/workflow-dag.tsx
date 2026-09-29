import {
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type PointerEvent as ReactPointerEvent,
} from "react";
import type { WorkflowRunDetail } from "./types";
import { StatusIcon } from "./workflow-ui";
import { ui } from "./ui";

type GraphEdge = {
  fromX: number;
  fromY: number;
  toX: number;
  toY: number;
  key: string;
};

function workflowLayers(detail: WorkflowRunDetail) {
  const jobs = detail.spec.jobs;
  const names = Object.keys(jobs).sort();
  const levels = new Map<string, number>();
  const remaining = new Set(names);
  while (remaining.size) {
    const ready = [...remaining].filter((name) =>
      (jobs[name].needs || []).every((need) => !remaining.has(need)),
    );
    if (!ready.length) {
      for (const name of [...remaining].sort()) levels.set(name, 0);
      break;
    }
    for (const name of ready.sort()) {
      const needs = jobs[name].needs || [];
      levels.set(
        name,
        needs.length
          ? Math.max(...needs.map((need) => levels.get(need) ?? 0)) + 1
          : 0,
      );
      remaining.delete(name);
    }
  }
  const width = Math.max(0, ...levels.values()) + 1;
  return Array.from({ length: width }, (_, level) =>
    names.filter((name) => levels.get(name) === level),
  );
}

export function WorkflowDAG({
  detail,
  matchingJobs,
  scale,
  jobURL,
}: {
  detail: WorkflowRunDetail;
  matchingJobs: Set<string>;
  scale: number;
  jobURL: (jobName: string) => string;
}) {
  const canvas = useRef<HTMLDivElement>(null);
  const scroll = useRef<HTMLDivElement>(null);
  const nodes = useRef(new Map<string, HTMLAnchorElement>());
  const [edges, setEdges] = useState<GraphEdge[]>([]);
  const [dragging, setDragging] = useState(false);
  const drag = useRef<
    | {
        pointerId: number;
        clientX: number;
        clientY: number;
        scrollLeft: number;
        scrollTop: number;
        moved: boolean;
      }
    | undefined
  >(undefined);
  const layers = useMemo(() => workflowLayers(detail), [detail]);
  useLayoutEffect(() => {
    const update = () => {
      const parent = canvas.current;
      if (!parent) return;
      const bounds = parent.getBoundingClientRect();
      const next: GraphEdge[] = [];
      for (const [job, spec] of Object.entries(detail.spec.jobs))
        for (const need of spec.needs || []) {
          const from = nodes.current.get(need)?.getBoundingClientRect();
          const to = nodes.current.get(job)?.getBoundingClientRect();
          if (from && to)
            next.push({
              // DOM rectangles include the canvas transform; SVG coordinates
              // must remain in its unscaled local coordinate system.
              fromX: (from.right - bounds.left) / scale,
              fromY: (from.top + from.height / 2 - bounds.top) / scale,
              toX: (to.left - bounds.left) / scale,
              toY: (to.top + to.height / 2 - bounds.top) / scale,
              key: JSON.stringify([need, job]),
            });
        }
      setEdges(next);
    };
    update();
    const observer = new ResizeObserver(update);
    if (canvas.current) observer.observe(canvas.current);
    for (const node of nodes.current.values()) observer.observe(node);
    return () => observer.disconnect();
  }, [detail, scale]);
  const endDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (drag.current?.pointerId !== event.pointerId) return;
    if (event.currentTarget.hasPointerCapture(event.pointerId)) {
      event.currentTarget.releasePointerCapture(event.pointerId);
    }
    drag.current = undefined;
    setDragging(false);
  };
  const startDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (
      event.button !== 0 ||
      (event.target as Element).closest(".dag-node") ||
      !scroll.current
    ) {
      return;
    }
    drag.current = {
      pointerId: event.pointerId,
      clientX: event.clientX,
      clientY: event.clientY,
      scrollLeft: scroll.current.scrollLeft,
      scrollTop: scroll.current.scrollTop,
      moved: false,
    };
    event.currentTarget.setPointerCapture(event.pointerId);
    setDragging(true);
  };
  const moveDrag = (event: ReactPointerEvent<HTMLDivElement>) => {
    const active = drag.current;
    if (!active || active.pointerId !== event.pointerId || !scroll.current) {
      return;
    }
    const offsetX = event.clientX - active.clientX;
    const offsetY = event.clientY - active.clientY;
    if (!active.moved && Math.hypot(offsetX, offsetY) < 4) return;
    active.moved = true;
    event.preventDefault();
    scroll.current.scrollLeft = active.scrollLeft - offsetX;
    scroll.current.scrollTop = active.scrollTop - offsetY;
  };
  return (
    <div className={`${ui.subtlePanel} mt-6 min-h-[33.5rem] overflow-hidden`}>
      <div
        className="dag-scroll"
        data-dag-dragging={dragging || undefined}
        onPointerCancel={endDrag}
        onPointerDown={startDrag}
        onPointerMove={moveDrag}
        onPointerUp={endDrag}
        ref={scroll}
      >
        <div
          className="dag"
          ref={canvas}
          style={{ "--dag-scale": scale } as React.CSSProperties}
        >
          <svg className="dag-edges" aria-hidden="true">
            {edges.map((edge) => {
              const middle = (edge.fromX + edge.toX) / 2;
              return (
                <path
                  key={edge.key}
                  d={`M ${edge.fromX} ${edge.fromY} H ${middle} V ${edge.toY} H ${edge.toX}`}
                />
              );
            })}
          </svg>
          <div className="dag-stages">
            {layers.map((layer, level) => (
              <section
                className={`dag-stage ${layer.length === 1 ? "single" : "group"}`}
                key={level}
              >
                {layer.length > 1 && (
                  <header className="dag-stage-header">
                    <strong>Parallel jobs</strong>
                    <span>{layer.length} jobs</span>
                  </header>
                )}
                {layer.map((jobName) => {
                  const job = detail.status.jobs?.[jobName];
                  const result = job?.outputs?.result;
                  return (
                    <a
                      className={`dag-node ${
                        matchingJobs.has(jobName) ? "match" : "dimmed"
                      }`}
                      href={jobURL(jobName)}
                      key={jobName}
                      ref={(element) => {
                        if (element) nodes.current.set(jobName, element);
                        else nodes.current.delete(jobName);
                      }}
                    >
                      <span className="dag-node-heading">
                        <span>
                          <StatusIcon value={job?.phase || "Pending"} />
                          <strong>{jobName}</strong>
                        </span>
                        <small>{job?.phase || "Pending"}</small>
                      </span>
                      {result && <small>result: {result}</small>}
                    </a>
                  );
                })}
              </section>
            ))}
          </div>
        </div>
      </div>
    </div>
  );
}
