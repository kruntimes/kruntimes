# Target User Validation Playbook

This playbook turns the post-public validation items in
[Open Source Readiness](open-source-readiness.md) into a repeatable operating
process. It is not evidence by itself. Mark the readiness items complete only
after real users complete the activities and the evidence is recorded.

## Goals

Use this process to answer three questions:

1. Do target users understand the kruntimes value quickly?
2. Do they have an urgent execution problem around Pod startup latency, burst
   throughput, or infrastructure ownership?
3. Is the first wedge, AI agent tools and trusted internal code-execution
   sandboxes, strong enough to guide near-term product work?

## Target Segments

Recruit 5-8 users across these segments:

| Segment | What to Look For | Strong Signal |
| --- | --- | --- |
| AI agent infrastructure | Teams running trusted tools, code execution, or sandbox-like tasks for agents. | They already keep warm workers or are blocked by per-task Pod startup. |
| Platform engineering | Internal developer platforms that expose execution APIs to other teams. | They want Kubernetes-native ownership, RBAC, status, and runtime pool control. |
| CI infrastructure | Teams running many short trusted CI steps. | Cold starts or queue drain time are visible to developers. |
| Automation platforms | Teams running short internal scripts or operational tasks. | They need fast dispatch and clear Run status without building a full worker platform. |

Avoid over-weighting feedback from users whose primary need is hostile-code
isolation, mature workflow UI, event-driven serving, or cluster-level batch
policy. Those users can still provide useful objections, but they should not
define the first wedge.

## Outreach Message

Use short, problem-focused outreach:

```text
I'm validating kruntimes, a Kubernetes-native execution engine for running
trusted short tasks on pre-warmed Runtime Pods.

I'm looking for teams that run AI agent tools, internal code execution, CI
micro-steps, or automation tasks on Kubernetes and feel Pod startup latency,
burst queue drain, or runtime ownership pain.

Would you be open to a 30-minute conversation or trying a small demo workload?
I'm mainly trying to learn whether this solves a real problem, not to sell a
finished platform.
```

Public interview feedback can be recorded with the
[Target user interview issue template](https://github.com/kruntimes/kruntimes/issues/new?template=target_user_interview.yml).
Use the template for public, non-sensitive summaries only. Keep private company
details, credentials, private source code, customer data, and security reports
out of public issues.

## Interview Script

Keep the first conversation focused on current behavior, not desired features.

1. What kinds of short-lived execution workloads do you run today?
2. How are they scheduled: one Pod per task, warm workers, a queue consumer, or
   another system?
3. Where does startup latency show up for users or pipelines?
4. During bursts, what determines queue drain time?
5. Who owns the runtime image, security policy, ServiceAccount, and resource
   limits?
6. What logs, artifacts, status, cancellation, and retry semantics do users
   need?
7. What would make a Kubernetes-native warm execution pool unacceptable?
8. After hearing the description, how would you explain kruntimes back in your
   own words?
9. Would you try it on a real non-production workload? If not, what is missing?

Record exact wording when a user explains the project value or rejects it.
Those phrases are better evidence than maintainer summaries.

## Trial Task

Ask each qualified user to try one small workload:

1. Install kruntimes on a local or development Kubernetes cluster.
2. Run the [Quick Start](quickstart.md).
3. Run at least one [end-to-end demo](demos.md), preferably the workload
   closest to their segment.
4. Inspect Run status, compact outputs, and logs.
5. Replace the demo command with one real internal command, script, or toy
   version of their workload.
6. Record setup time, blockers, missing permissions, confusing docs, and the
   point where they would or would not continue.

Do not count a trial as a design-partner success unless the user runs a real or
representative workload, not only a maintainer-led demo.

Public trial feedback can be recorded with the
[Design partner trial issue template](https://github.com/kruntimes/kruntimes/issues/new?template=design_partner_trial.yml).
Private interviews should use the same fields internally and avoid publishing
company names, credentials, private source code, or customer data without
explicit approval.

## Evidence Template

Use one row per conversation or trial. Keep private company and person names out
of public docs unless the user explicitly approves attribution.

| Date | User Label | Segment | Workload | Signal | Evidence | Follow-up |
| --- | --- | --- | --- | --- | --- | --- |
| YYYY-MM-DD | target-user-a | AI infra / Platform / CI / Automation | Short description | Comprehension / Trial interest / Trial / Quick start / Rejection | Link to interview issue, trial issue, Discussion, notes, or anonymized quote | Next action |

## Wedge Decision Rules

Evaluate the AI agent tools and trusted internal code-execution wedge with
these rules:

- **Validate** when at least two target users in the wedge run representative
  workloads and say the warm Runtime pool model addresses a current problem.
- **Keep exploring** when users understand the value but have not tried real
  workloads yet.
- **Refocus positioning** when users cannot explain the value in two minutes.
- **Shift wedge** when another segment shows stronger urgency and willingness
  to trial, while AI/tooling users mostly identify non-goals such as hostile-code
  isolation or mature workflow UI.
- **Prioritize blockers** when multiple trials hit the same missing capability,
  even if that capability was not on the original roadmap.

Repository metrics, stars, and downloads are context. They do not validate the
wedge without user conversations and workload trials.

## Weekly Review

During the first public validation period, review evidence weekly:

1. Count conversations, real workload trials, independent quick starts, and
   non-maintainer issues or PRs.
2. Identify repeated blockers and confusing documentation.
3. Decide whether the next week should focus on more interviews, demo quality,
   installation fixes, or product changes.
4. Update [Adoption Signals](adoption-signals.md) and the roadmap only when the
   evidence changes the project direction.
