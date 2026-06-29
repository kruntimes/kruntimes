# Adoption Signals

This document defines the public adoption signals kruntimes should track after
launch. The goal is to measure whether the project value is understandable,
whether target users try real workloads, and whether the onboarding path works
for people outside the maintainer group.

These signals are product validation inputs, not vanity metrics. Stars,
downloads, and page views are useful context, but they do not prove that
kruntimes solves an urgent execution problem.

## Primary Signals

| Signal | Target | Why It Matters | Evidence |
| --- | --- | --- | --- |
| Value comprehension | A new user can explain the project value in two minutes. | If users cannot describe the value quickly, positioning is unclear. | Discovery notes, demo feedback, issue/Discussion phrasing, or user quotes. |
| Real workload trials | At least two design partners try real workloads. | Real workloads reveal scheduling, security, artifact, and operational gaps that examples cannot. | Named design partner notes, workload description, blocker list, and follow-up outcome. |
| Independent quick start | At least one non-maintainer completes the quick start without private help. | The project must be usable by someone who did not build it. | Issue/Discussion confirmation, screen share notes, or written onboarding report. |

## Secondary Signals

Track these to understand momentum, but do not treat them as proof of product
value by themselves:

- GitHub stars, forks, and watchers.
- Docs page views and search queries.
- Helm chart pulls and image pulls.
- CLI release downloads.
- Issues, Discussions, and PRs from non-maintainers.
- Mentions in blogs, social posts, talks, or internal platform evaluations.

## Target User Segments

Prioritize feedback from users who are likely to feel the problem:

- Platform teams operating internal developer platforms.
- CI infrastructure teams running many short steps.
- AI agent infrastructure teams running trusted tools or code sandboxes.
- Automation teams that already use Kubernetes but struggle with Pod startup
  latency or queue visibility.

Feedback from users who only need long-running services, hostile-code
isolation, or mature workflow UI is still useful, but it should not dominate the
first wedge decision.

## Tracking Template

Use one row per conversation, trial, or onboarding attempt.

| Date | User / Team | Segment | Workload | Signal | Outcome | Follow-up |
| --- | --- | --- | --- | --- | --- | --- |
| YYYY-MM-DD | TBD | Platform / CI / AI infra / Automation | Short description | Comprehension / Trial / Quick start | Pass / Partial / Fail | Link to issue, PR, notes, or next action |

Keep private names and company details out of public docs unless the user has
explicitly agreed to be referenced. Public tracking can use anonymized labels
such as `design-partner-a`.

## Validation Cadence

Review adoption signals weekly while the project is early:

1. Count new conversations, real workload trials, independent quick starts, and
   non-maintainer contributions.
2. Extract repeated blockers from notes and issues.
3. Update the roadmap if the same blocker appears in multiple target-user
   conversations.
4. Revisit the primary wedge if target users understand the project but do not
   try real workloads.

## Decision Rules

Use these rules to interpret the signals:

- If users cannot explain the value in two minutes, improve positioning,
  overview docs, and examples before adding more features.
- If users understand the value but do not try real workloads, improve the demo
  path, installation path, and target segment focus.
- If users try real workloads but hit the same operational blocker, prioritize
  that blocker over broad roadmap expansion.
- If non-maintainers cannot complete the quick start, treat onboarding as a
  release blocker for the next public milestone.
- If design partners succeed with similar workloads, use that pattern to refine
  the primary wedge.

## Public Reporting

Until there is enough evidence, report adoption status qualitatively:

- `Exploring`: target users are being interviewed.
- `Trialing`: at least one design partner is trying a real workload.
- `Validated`: at least two design partners completed real workload trials and
  at least one non-maintainer completed the quick start.

Do not mark a wedge validated only because repository metrics increased.
