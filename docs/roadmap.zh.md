---
title: "项目状态与路线图"
---

kruntimes 作为 `v0.x experimental` 项目活跃开发中。API 是 `v1alpha1`，可能在稳定发布
之前发生变更。

## 当前状态

已完成的基础功能包括：

- Run 和 Runtime CRDs。
- 预热 Runtime Pod 调度。
- Bash 和 Python 内置 runtimes。
- 有界输出和外部 artifact 引用。
- 通过长期运行的 maintainers 进行 Runtime artifact 清理。
- 重试、超时、取消、stale-pod 恢复和终止条件。
- Helm charts、发布工作流、SBOM、签名、CLI releases 和 benchmark harness。
- 安全、运维、发布、兼容性和自定义 Runtime 文档。

## 近期路线图

### v0.x 实验期

- 保持公开文档与实现同步。
- 加强调度、artifact 清理和 workflow 行为的 E2E 覆盖。
- 改进 CLI 易用性和示例。
- 扩展自定义 Runtime 示例。
- 持续推进供应链和安全加固。

### 迈向 v1.0

- 稳定 CRD API。
- 定义兼容性和迁移保证。
- 记录弃用策略。
- 明确生产环境的多租户隔离策略。
- 发布稳定的安装和升级指南。

## 开源就绪

详细的开源就绪清单见 [Open Source Readiness Plan](open-source-readiness.md)。

## 发布历史

见 [CHANGELOG.md](https://github.com/kruntimes/kruntimes/blob/main/CHANGELOG.md)
和 [Release Process](release.md)。
