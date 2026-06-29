---
title: "kruntimes"
---

kruntimes 是一个 Kubernetes-native execution engine，用于在预热的
Runtime Pod 池上运行 serverless functions、CI pipelines、包含 AI workload
的 batch workloads，以及 AI agent tasks/sandboxes。它避免为每次执行创建新
Pod，并在应用层完成细粒度调度。

项目目前为 `v0.x experimental`，使用 `v1alpha1` API。内置 Runtimes 适用于
受信任 namespace 中的受信任 workload。

## 从这里开始

- [项目概览](docs/overview/)
- [快速开始](docs/quickstart/)
- [安装](docs/installation/)
- [进阶用法](docs/usage/)
- [架构](docs/architecture/)
- [设计 PRD](docs/prd/kruntimes-prd/)
- [API 参考](docs/api/)
- [配置](docs/configuration/)
- [故障排查](docs/troubleshooting/)
- [常见问题](docs/faq/)
- [开发指南](docs/development/)
- [测试指南](docs/testing/)
- [运维指南](docs/operations/)
- [安全与威胁模型](docs/security/)
- [自定义 Runtime 开发](docs/custom-runtime/)
- [兼容性矩阵](docs/compatibility/)
- [发布流程](docs/release/)
- [性能基准测试](docs/benchmarks/)
- [路线图](docs/roadmap/)
- [社区与治理](docs/community/)
