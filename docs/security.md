# Security and Authorization

kruntimes uses Kubernetes namespaces and RBAC as its current administrative
boundary. The built-in Bash and Python runtimes execute trusted code inside
shared Runtime Pods; they are not sandboxes for mutually untrusted tenants.

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
