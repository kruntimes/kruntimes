# Security, Authorization, and Threat Model

kruntimes uses Kubernetes namespaces and RBAC as its current administrative
boundary. The built-in Bash and Python runtimes execute trusted code inside
shared Runtime Pods; they are not sandboxes for mutually untrusted tenants.

## Threat Model Summary

The primary security boundary is the Kubernetes namespace. A subject that can
create a Run in a namespace can execute code inside any matching Runtime Pod in
that namespace. A subject that can create or update a Runtime can influence the
generated Runtime Deployment, including executable images, commands, resources,
artifact storage, and workload identity.

kruntimes currently protects the Kubernetes API objects it owns, keeps Run
state transitions explicit, limits direct ingress to runtimed endpoints, and
documents trusted-workload expectations. It does not provide per-Run sandboxing
inside the built-in runtimes.

### Assets

| Asset | Security goal |
| --- | --- |
| Run and Workflow specs | Prevent unauthorized code execution, source disclosure, and mutation. |
| Run status, outputs, artifact refs, and logs | Prevent unauthorized disclosure and misleading status updates. |
| Runtime specs and generated Deployments | Restrict who can choose executable images, commands, volumes, credentials, and service accounts. |
| Runtime Pod workspace | Prevent accidental cross-Run data reuse; do not claim hostile-code isolation. |
| Artifact stores and credentials | Keep stored objects scoped to the owning Run and prevent credential exposure to unauthorized users. |
| Control-plane service accounts | Reserve scheduler, controller, and runtimed status mutation rights for kruntimes components. |

### Actors

| Actor | Assumption |
| --- | --- |
| Cluster administrator | Trusted to install CRDs, Helm charts, controller RBAC, and namespace policy. |
| Runtime administrator | Trusted namespace operator; can configure executable Runtime pools and attached credentials. |
| Run submitter | Trusted to execute code within the namespace trust boundary, but not trusted with control-plane status writes. |
| Run observer | May inspect Run status, outputs, artifact metadata, and optionally logs/artifacts when granted port-forward access. |
| Built-in Runtime implementation | Trusted code running inside a shared Runtime Pod; not a hostile-code sandbox. |
| Custom Runtime implementation | Responsible for any stronger isolation boundary it advertises. |

### Trust Boundaries

- **Namespace boundary**: scheduler placement is namespace-local. Namespaces
  should not mix mutually untrusted Run submitters with shared built-in Runtime
  pools.
- **Runtime Pod boundary**: all Runs assigned to one built-in Runtime Pod share
  the pod network namespace, runtime process namespace, mounted volumes,
  workspace volume, and Kubernetes service account.
- **Control-plane boundary**: only kruntimes controllers should update
  `runs/status`, `runtimes/status`, and generated Runtime Pod readiness
  conditions.
- **Artifact boundary**: artifacts are referenced from Run status but stored
  outside etcd. Store configuration and credentials are part of the Runtime
  administrator trust boundary.

### Threats, Mitigations, and Current Gaps

| Threat | Current mitigation | Current gap or required operator control |
| --- | --- | --- |
| Unauthorized user executes code by creating Runs | Kubernetes RBAC controls `runs` creation. | RBAC cannot restrict `spec.runtime`, command, env, or source per user; use namespace separation or admission policy. |
| Runtime administrator deploys malicious runtime or sidecar image | Treat `runtimes` write permission as workload-admin access. | No image policy is enforced by kruntimes; use admission controls and signed image policy. |
| Run code reads another Run's files in the same Runtime Pod | runtimed uses per-Run workspace directories and cleanup. | Built-in runtimes do not sandbox hostile code from shared pod filesystems or runtime processes. |
| Run code uses Runtime Pod network or service account | Namespace-local scheduling and documented role separation. | Built-in runtimes do not provide per-Run network or identity isolation; use custom runtimes or separate namespaces/service accounts. |
| User reads secrets from Run specs or status | Documentation warns against putting credentials in `Run.spec.env`; RBAC controls `runs` reads. | Kubernetes stores readable Run spec/status for authorized readers; use Secrets mounted by trusted runtimes instead. |
| User reaches runtimed status or artifact endpoints directly | Default NetworkPolicy restricts pod ingress; CLI access uses Kubernetes port-forward permissions. | Operators must grant `pods/portforward` only to users allowed to read logs/artifacts. |
| Stale or compromised component mutates Run status | Status writes are reserved for control-plane service accounts. | Cluster RBAC must not grant status update verbs to users or unrelated controllers. |
| Artifact credential disclosure or object confusion | Artifact refs are compact metadata; store drivers validate refs and paths. | Runtime admins control artifact store credentials; central cleanup and recovery semantics are tracked separately. |
| Cross-namespace Run execution | Scheduler only considers Runtime Pods in the Run namespace. | Cluster-scoped controller RBAC still needs careful installation and audit. |

### Non-Goals for Built-In Runtimes

The built-in Bash and Python runtimes are not intended to provide:

- isolation between mutually untrusted Runs in the same Runtime Pod;
- per-Run Kubernetes service accounts, network policies, or cgroups;
- protection from code that intentionally inspects shared process, network, or
  filesystem state inside the Runtime Pod;
- tenant-grade secret isolation for values placed directly on Run objects.

Use a custom Runtime with a dedicated container, sandbox, or microVM per Run
when these properties are required.

## Permission Semantics

Grant kruntimes permissions according to the capability they provide, not only
the Kubernetes resource name.

| Permission | Security meaning |
| --- | --- |
| `create`, `update`, or `patch` `runtimes` | Deploy or change executable runtime and runtimed images in the namespace. A Runtime can select commands, environment, resources, artifact PVCs, and artifact credential Secrets. Treat this as namespace workload-administrator access. |
| `delete` `runtimes` | Remove a Runtime pool and interrupt or strand Runs assigned to it. |
| `create` `runs` | Execute code inside a matching Runtime Pod. The code shares that Pod's process, network, workspace, mounted volumes, and workload identity according to the Runtime implementation. |
| `update` or `patch` `runs` | Change a pending workload or request cancellation. kruntimes does not currently expose a narrower cancellation subresource. |
| `delete` `runs` | Remove execution state and initiate artifact cleanup when finalizers are present. |
| `get`, `list`, or `watch` `runs` | Read source references, arguments, environment values, execution status, outputs, and artifact metadata stored on the Run object. |
| `update` or `patch` `runs/status` | Control scheduling and execution state. Reserve this for kruntimes control-plane service accounts. |
| `create` `pods/portforward` plus `get` `pods` and `runs` | Reach a Runtime Pod's runtimed status and artifact endpoint through the Kubernetes API, as used by `krt logs` and artifact downloads. |

Kubernetes does not authorize a Run against the referenced Runtime as a
separate operation. If a subject can create a Run in a namespace, it can request
any Runtime name available to the scheduler in that namespace.

## Recommended Role Separation

Use namespaced `Role` and `RoleBinding` objects for application users:

- **Runtime administrators** may manage `runtimes`. Only trusted platform
  operators should receive this capability because `spec.image`,
  `spec.daemonImage`, artifact credentials, and mounted storage affect the
  generated Runtime Pods.
- **Run submitters** may create and read `runs`. Grant `update` or `patch` only
  when they also need cancellation; those verbs currently permit broader Run
  mutation.
- **Run observers** may receive read-only access to `runs`. Add
  `pods/portforward` only when they are allowed to read runtime logs or
  artifacts.
- **Control-plane service accounts** own status mutation. Do not grant users
  write access to `runs/status` or `runtimes/status`.

Do not grant wildcard verbs or resources to Run submitters. Kubernetes RBAC
cannot restrict a user to a particular `spec.runtime`, source URL, command, or
environment value. Enforce those policies with a validating admission policy
or admission webhook when a namespace contains Runtime pools with different
trust levels.

## Namespace Trust Boundary

The scheduler assigns a Run only to Runtime Pods in the same namespace.
Namespaces should therefore group subjects and Runtime pools that share a trust
level. Do not place mutually untrusted Run submitters in one namespace with a
shared built-in Runtime pool.

The platform Helm chart currently installs cluster-scoped controller roles.
Those roles are for kruntimes components, not examples of end-user access.
Application user access should be granted separately with namespaced RBAC.

NetworkPolicy limits direct Pod ingress, but it does not isolate Runs executing
inside the same Runtime Pod. Run code may share:

- the Runtime Pod network namespace and outbound connectivity;
- the shared workspace volume, with per-Run directories enforced by runtimed;
- the runtime process namespace and runtime server;
- volumes and Kubernetes workload identity exposed by the Runtime Pod.

For untrusted code, use a custom Runtime that creates a stronger per-Run
boundary such as a dedicated container, sandboxed runtime, or microVM, and
combine it with namespace isolation, restricted service accounts, egress
policy, and admission controls.

## Secrets and Inputs

- Do not put credentials directly in `Run.spec.env`; readers of the Run object
  can inspect its spec.
- Treat Git repositories and inline source as executable input.
- A Runtime artifact `credentialsSecretName` is exposed to the runtimed
  container. Because a Runtime administrator can also select
  `spec.daemonImage`, only trusted administrators may configure Runtime
  objects.
- Review the service account, volumes, Secrets, network access, and node
  placement of every custom Runtime before allowing users to submit Runs to it.
