---
title: "kruntimes PRD"
---

## Background: Challenges in Building a Kubernetes-Based Serverless Platform

Building a serverless platform on Kubernetes is challenging because Kubernetes
scheduling and resource management are not a perfect match for serverless
workloads. Serverless workloads are usually short-lived, need fast startup, and
have dynamic resource requirements. These characteristics can conflict with the
default Kubernetes execution path.

For example, the Kubernetes scheduler can take seconds to schedule a Pod, while
serverless workloads may need to start and finish in milliseconds. Kubernetes
resource management can also create imbalance when many small, bursty workloads
arrive at once.

- **Performance and overhead: Pod cold starts**

  This is the core pain point. Serverless systems need millisecond-level
  response, but a native Pod startup path can be much slower. The biggest costs
  are image pulling, especially for multi-GB images, scheduling, IP allocation,
  volume mounting, and container initialization. Sandboxes such as Firecracker
  can start microVMs quickly, but combining them with lazy container image
  loading and standard Kubernetes interfaces often requires non-standard
  infrastructure changes that many platform teams do not want to own.

- **Scheduling efficiency**

  The Kubernetes scheduler is designed for long-running services and uses a
  best-effort model. Using it to schedule a large number of short-lived,
  high-concurrency serverless tasks can lead to insufficient scheduling
  throughput and slow decisions. Batch schedulers such as Volcano are suitable
  for big data jobs, but they are less optimized for fine-grained function-like
  tasks.

- **Elasticity speed**

  Native HPA reacts to metrics and can lag behind sudden traffic spikes.
  Event-driven scaling layers such as KEDA can make faster decisions, but the
  underlying Pods still start slowly. The result is a gap between fast scaling
  decisions and slow execution readiness. Systems usually need cold-start
  optimization or reserved warm pools to compensate.

- **Organization and Conway's Law**

  This is often the most practical constraint. Serverless teams are frequently
  separate from Kubernetes infrastructure teams and cannot easily push custom
  scheduler, image loading, or node-runtime changes. Organizational boundaries
  make some technically possible solutions impractical, so the platform needs a
  path that stays above Kubernetes internals.

## Proposal: Runtime Pools with Two-Layer Scheduling on Kubernetes

- **Bypass organizational blockers with application-layer control**

  The Kubernetes team provides basic Pod management. kruntimes treats this as a
  new infrastructure substrate and moves optimization logic into an
  application-layer control plane owned by the serverless or platform team. The
  scheduler, CRDs, and daemon components run in the team's own namespace and do
  not require deep Kubernetes infrastructure customization.

- **Replace creation with reuse to address cold starts**

  The core idea is to use pre-created warm Pod pools instead of creating a new
  Pod on demand. When a task arrives, the scheduler assigns it to a ready daemon
  inside a warm Pod. User-visible startup can drop from a Pod startup path to a
  much shorter dispatch path. This avoids the slowest parts of Pod cold starts:
  Kubernetes scheduling and image pulling.

- **Use two-layer scheduling to separate concerns**

  The first layer is Kubernetes coarse scheduling, responsible for keeping warm
  Pod pools alive. The second layer is kruntimes application scheduling, which
  reuses compute capacity inside warm Pods and handles high-concurrency
  short-lived tasks. This gives the application layer room to implement
  queueing, priority, resource allocation, and policies that match CI/CD and
  serverless workloads.

- **Use the Runtime abstraction for environment consistency**

  Different execution environments, such as Go, Node.js, or BuildKit, can be
  wrapped as different Runtimes. Each Runtime is an independent Deployment pool.
  This isolates environment dependencies, avoids dependency conflicts, keeps
  execution environments consistent through prebuilt images, and makes the
  system extensible by adding a new pool and labels for a new language or tool.

- **Use declarative CRDs to reduce distributed-system complexity**

  kruntimes avoids complex peer-to-peer communication and introduces the Run
  CRD as the central contract. It records which environment to use, what to do,
  who should do it, and what happened.

  - The scheduler only watches Runs and writes scheduling information.
  - The daemon only watches Runs assigned to itself, executes them, and updates
    status.
  - Failover is simpler because if a daemon disappears, its Runs can be marked
    pending again after timeout and rescheduled. State is persisted in etcd.

Overall, this design builds an application-layer serverless framework on top of
an imperfect but widely available Kubernetes substrate. It is optimized for
short-lived tasks, keeps the system controllable by the platform team, and
leaves room to evolve toward environment-as-function style architectures.

## User Experience

kruntimes is a two-layer scheduling system on Kubernetes. It keeps warm Runtime
Pods ready to execute code, reducing cold-start latency. The goal is to provide
a simple but powerful serverless infrastructure that administrators can deploy
without deep Kubernetes cluster modifications, while users can run code through
a simple interface.

Core user experience:

1. User interface

   - Users interact through the Kubernetes API and can create and manage Run CRD
     objects with kubectl.
   - SDKs such as Python and Go can submit code to kruntimes.

2. Runtime environments

   - Built-in runtimes can support languages and environments such as Bash,
     Python, Go, Node.js, and WASM. For these Runtimes, Run scheduling and
     execution are managed by the kruntimes two-layer scheduling system.
   - Custom Runtime Servers can extend kruntimes to additional languages and
     execution systems. Some environments may delegate scheduling to the
     Runtime Server itself, which is useful for Slurm, Ray, Spark, and similar
     systems. kruntimes can provide Runtime Server implementations for such
     systems, and users can implement their own.

3. Resource management

   - Default Runtime resources include CPU, memory, and storage. Users can add
     additional resource types in the Runtime CRD spec, such as GPU, FPGA,
     network bandwidth, or storage IOPS.
   - Users can specify resource requirements in the Run CRD spec so their code
     runs in an environment that satisfies those requirements.
   - The kruntimes scheduler places Runs onto suitable Runtime Pods based on
     requested and available resources.
   - Per-Run resource isolation is managed by the Runtime Server's local
     scheduler, for example by using cgroups inside the Runtime.

4. Application layer

   - Future application-layer features can include scheduled jobs such as
     CronRun and workflow orchestration such as Pipeline. These features build
     on the core user interface and Runtime environment model.
