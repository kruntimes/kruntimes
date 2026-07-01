---
title: "发布流程"
---

kruntimes 使用带有前导 `v` 的 SemVer tags，例如 `v0.1.0`。

项目目前是 `v0.x experimental`。CRDs 是 `v1alpha1`，因此次版本号发布可能仍包含
breaking API 或行为变更。发布说明必须明确指出这些变更。

## 版本管理

- 补丁版本修复 bug、安全问题和文档错误，不含故意的 API 或行为变更。
- 次版本添加功能、API 字段、runtime 行为或安装变更。在 `v0.x` 期间，当发布说明描述了
  影响和迁移路径时，可能包含 breaking changes。
- 主版本保留给稳定的 API 兼容性承诺。

发布应用版本时保持以下版本一致：

- Git tag：`vX.Y.Z`
- `charts/kruntimes/Chart.yaml`：`appVersion`
- `charts/kruntimes-runtimes/Chart.yaml`：`appVersion`
- 引用了具体发布版本的 README 示例

Chart `version` 是 Helm package version，不需要等于 `appVersion`。当 chart
templates、values、dependencies 或 chart metadata 变化时 bump chart `version`。
当默认安装的 kruntimes application 或 runtime image 版本变化时 bump `appVersion`。

不要复用或移动已发布的 release tags。如果发布产物在发布后有问题，请发布一个新的补丁
版本，并将有问题的 GitHub release 标记为被取代。

## Changelog

每个面向用户的变更都应在 `Unreleased` 下有 `CHANGELOG.md` 条目。适用时使用以下标题：

- `Added`
- `Changed`
- `Deprecated`
- `Removed`
- `Fixed`
- `Security`

打 tag 之前：

1. 将 `Unreleased` 条目移到 `## X.Y.Z - YYYY-MM-DD` 下。
2. 在顶部保留一个空的 `## Unreleased` section。
3. 在常规列表之前标注 breaking changes 和必需的迁移步骤。
4. 不影响用户、运维人员、runtime 作者或贡献者的纯内部重构不要放入 changelog。
5. 当发布更改了 Kubernetes、Helm、Go、Python 或发布的 `krt` artifact 支持时，
   更新 `docs/compatibility.md`。
6. 当发布更改了安装、升级、卸载、故障排查、备份或恢复行为时，更新 `docs/operations.md`。
7. 当发布更改了 Runtime Server 协议、Runtime CRD 模板约定或执行语义时，
   更新 `docs/custom-runtime.md`。

## 发布说明

GitHub release notes 应从 changelog 编写，并包含：

- 发布类型和支持级别，
- 升级说明和 breaking changes，
- 镜像 tags 和验证说明，
- Helm OCI chart 引用，
- CLI 二进制、校验和和 provenance 验证说明，
- 该版本的已知限制，
- 指向 changelog 和安装文档的链接。

使用以下大纲：

```markdown
## Summary

## Breaking Changes

## Upgrade Notes

## Images

## Verification

## Known Limitations

## Changelog
```

对于 `v0.x` 发布，包含一句明确说明 `v1alpha1` API 是实验性的，可能在后续次版本中更改。

## 预检

打 tag 之前运行以下检查：

```bash
make test
make test-integration
make test-helm
make test-race
make govulncheck
```

在公开发布 tags 以及任何更改 Runtime、scheduler、controller、Helm、CRD、artifact
或 Workflow 行为的发布前运行 `make e2e`。

确认预检后这些生成文件是干净的：

```bash
git status --short
```

## 打 Tag

检查通过后：

1. 提交 changelog 和版本更新。
2. 创建并推送注释 tag：

   ```bash
   git tag -a v0.1.0 -m "kruntimes v0.1.0"
   git push upstream v0.1.0
   ```

3. 确认 `Release Images` workflow 发布了带有 SBOM 和 provenance attestation 的签名镜像。
4. 确认 `Release Charts` workflow 发布了 Helm OCI charts。
5. 确认 `Release CLI` workflow 将 `krt` 归档、校验和和 provenance attestation 上传到
   GitHub release。
6. 从 changelog 起草 GitHub release，仅在发布产物可用后才发布。

## 产物验证

### 容器镜像

发布的容器镜像预期在以下位置：

- `ghcr.io/<owner>/scheduler:<version>`
- `ghcr.io/<owner>/controller:<version>`
- `ghcr.io/<owner>/runtimed:<version>`
- `ghcr.io/<owner>/bash-runtime:<version>`
- `ghcr.io/<owner>/python-runtime:<version>`

使用 `cosign` 验证签名：

```bash
cosign verify ghcr.io/<owner>/controller:0.1.0 \
  --certificate-identity-regexp 'https://github.com/.*/.github/workflows/release-images.yml@refs/tags/v0.1.0' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

同时确认 GitHub Packages 中显示的镜像 digest 与 `Release Images` workflow 输出中的
digest 匹配。

### Helm OCI Charts

发布的 Helm charts 预期在以下位置：

- `oci://ghcr.io/<owner>/charts/kruntimes`
- `oci://ghcr.io/<owner>/charts/kruntimes-runtimes`

按版本安装已发布的 charts：

```bash
helm upgrade --install kruntimes oci://ghcr.io/<owner>/charts/kruntimes \
  --version 0.1.0 \
  --namespace kruntimes-system \
  --create-namespace

helm upgrade --install kruntimes-runtimes oci://ghcr.io/<owner>/charts/kruntimes-runtimes \
  --version 0.1.0 \
  --namespace default \
  --create-namespace
```

### krt CLI

发布的 `krt` release assets 预期在 GitHub release 中：

- `krt_vX.Y.Z_linux_amd64.tar.gz`
- `krt_vX.Y.Z_linux_arm64.tar.gz`
- `krt_vX.Y.Z_darwin_amd64.tar.gz`
- `krt_vX.Y.Z_darwin_arm64.tar.gz`
- `krt_vX.Y.Z_windows_amd64.tar.gz`
- `krt_vX.Y.Z_checksums.txt`

下载所需的归档和校验和文件后验证校验和：

```bash
sha256sum --check --ignore-missing krt_v0.1.0_checksums.txt
```

验证 GitHub artifact provenance：

```bash
gh attestation verify krt_v0.1.0_linux_amd64.tar.gz \
  --repo <owner>/kruntimes
```

attestation subject digest 必须与下载的归档 digest 匹配。

## 失败的发布

如果打 tag 成功但产物发布失败：

1. 在正常 PR 中修复 workflow 或源码问题。
2. 验证通过后发布新的补丁版本。
3. 不要重新 tag 原版本。
4. 如果有任何产物对用户可见，添加一条 release note 说明哪个版本取代了失败的发布。
