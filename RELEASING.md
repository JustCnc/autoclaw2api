# 发布说明（Agent Runbook）

本文件给后续 agent / 维护者使用。目标：把 `autoclaw2api` 的新版本发布到 GitHub Release，
让 CPA 插件商店能安装/更新，并保持 `cpa_plugins` registry 一致。

## 0. 先看结论

- 版本唯一来源是 `schema.go` 里的 `pluginVersion`，Git tag 必须是 `v<pluginVersion>`。
- 插件商店读取**最新 GitHub Release**；只推 tag 不发 Release，商店无法安装/更新。
- Release 必须是正式版：不要 draft，不要勾选 prerelease。
- Release 资产名、zip 结构、`checksums.txt` 由 workflow 自动生成，不要手工改名。
- registry 的 `version` 只是展示兜底；正常发版不需要改 registry，但建议同步更新。

## 1. 关键文件与地址

| 项 | 值 |
| --- | --- |
| 插件项目 | `C:/Users/test/Desktop/cpa/autoClaw2api` |
| registry 项目 | `C:/Users/test/Desktop/cpa/cpa_plugins` |
| 插件仓库 | `https://github.com/JustCnc/autoclaw2api` |
| registry 仓库 | `https://github.com/JustCnc/cpa_plugins` |
| registry raw URL | `https://raw.githubusercontent.com/JustCnc/cpa_plugins/main/registry.json` |
| Release workflow | `.github/workflows/release.yml` |
| 插件 ID / provider key | `autoclaw2api` |
| 支持平台 | `linux/amd64`、`linux/arm64`、`windows/amd64` |

本机路径（其他环境按实际调整）：

| 用途 | 路径 |
| --- | --- |
| CPA 核心目录 | `C:/Users/test/Downloads/EasyCLIProxyAPI-v0.3.53-Windows-amd64/cpa-core` |
| CPA 配置 | `cpa-core/config.yaml` |
| 插件数据 | `cpa-core/plugin-data/autoclaw2api` |
| 本地 Go | `/c/Users/test/Downloads/raccoon-build/go/bin` |
| 本地 mingw | `/c/Users/test/Downloads/raccoon-build/mingw/ucrt64/bin` |
| 管理 API | `http://127.0.0.1:8317` |
| 管理密钥 | 读 `cpa-core/config.yaml` 的 `management.secret-key`，**不要写进仓库** |

## 2. 发版前检查

先把版本号改好，再提交、打 tag。顺序反了会导致 tag 指向旧代码。

- [ ] 改 `schema.go`：

  ```go
  pluginVersion = "0.2.0"
  ```

- [ ] 如果作者/仓库信息变了，同步 `plugin.go` 的 `Author`、`GitHubRepository`。
- [ ] 如果行为/配置变了，更新 `README.md`、`config.example.yaml`、`config.go` 的配置描述。
- [ ] 如果上游客户端升级了，同步 `config.go` 里的 `X-Version`、`app_key`、系统提示词前缀默认值。
- [ ] 确认 `cpa_plugins/registry.json` 的 `id` 仍是 `autoclaw2api`，`repository` 仍是
      `https://github.com/JustCnc/autoclaw2api`。

本地检查：

```bash
cd C:/Users/test/Desktop/cpa/autoClaw2api
export PATH="/c/Users/test/Downloads/raccoon-build/go/bin:$PATH"

gofmt -l *.go          # 应该没有输出
CGO_ENABLED=0 go test ./...
git status --short
git diff --check
```

可选：本地编译 Windows DLL 做冒烟测试（产物在 `dist/`，已被 `.gitignore` 忽略）：

```bash
export PATH="/c/Users/test/Downloads/raccoon-build/mingw/ucrt64/bin:$PATH"
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -trimpath -ldflags='-s -w' \
  -o dist/plugins/windows/amd64/autoclaw2api.dll .
```

**不要提交** `dist/`、`*.exe`、`*.dll`、`*.so`、`accounts.json`、token、管理密钥、
从本机导出的 `autoclaw2api-default.json`。本仓库的 `.gitignore` 已经覆盖这些，
但提交前仍要看一眼 `git status`。

## 3. 提交并打 tag

```bash
cd C:/Users/test/Desktop/cpa/autoClaw2api

VERSION=0.2.0
TAG="v$VERSION"

git add schema.go plugin.go README.md config.example.yaml RELEASING.md \
  .github/workflows/release.yml
git commit -m "release: $TAG"
git push origin main

git tag -a "$TAG" -m "$TAG"
git push origin "$TAG"
```

规则：

- tag 必须指向版本号已经更新过的那个 commit。
- tag 和 `pluginVersion` 必须完全一致；workflow 会在不一致时直接失败。
- 已发布的 tag 不要移动、不要复用；需要修复就发新 patch 版本。
- 预览版不要走这个流程：workflow 只接受点分数字 tag，且商店只认正式 Release。

## 4. 发布 GitHub Release

### 方式 A：agent 有 `GH_TOKEN` / `gh auth status` 正常

```bash
gh auth status

gh release create "$TAG" \
  --repo JustCnc/autoclaw2api \
  --verify-tag \
  --title "$TAG" \
  --generate-notes
```

不要加 `--draft` 或 `--prerelease`。

### 方式 B：人工网页

1. 打开 https://github.com/JustCnc/autoclaw2api/releases
2. **Draft a new release**
3. 选择刚推的 tag（例如 `v0.2.0`）
4. 标题、正文可留空；workflow 会在正文为空时自动写入更新内容
5. **不要**勾选 prerelease，直接 **Publish release**

### 方式 C：重跑已有 tag 的构建

tag 已存在、Release 已有但资产缺失/构建失败时：

```bash
gh workflow run release.yml \
  --repo JustCnc/autoclaw2api \
  -f tag="$TAG"
```

## 5. 等待并核对 Release 资产

```bash
gh run list --repo JustCnc/autoclaw2api --workflow release.yml --limit 5
gh run watch <run-id> --repo JustCnc/autoclaw2api

gh release view "$TAG" --repo JustCnc/autoclaw2api \
  --json tagName,isDraft,isPrerelease,assets
```

一次成功发布应包含（`X.Y.Z` 为不带 `v` 的版本）：

```text
autoclaw2api_X.Y.Z_linux_amd64.zip
autoclaw2api_X.Y.Z_linux_arm64.zip
autoclaw2api_X.Y.Z_windows_amd64.zip
checksums.txt
autoclaw2api-X.Y.Z-linux-amd64.so
autoclaw2api-X.Y.Z-linux-arm64.so
autoclaw2api-X.Y.Z-windows-amd64.dll
autoclaw2api-X.Y.Z-linux.tar.gz
SHA256SUMS
RELEASE_NOTES.md
```

下载并校验：

```bash
rm -rf /tmp/autoclaw-release
gh release download "$TAG" \
  --repo JustCnc/autoclaw2api \
  --dir /tmp/autoclaw-release \
  --clobber

cd /tmp/autoclaw-release
sha256sum -c checksums.txt

for z in autoclaw2api_*.zip; do
  echo "== $z"
  unzip -l "$z"
  count=$(unzip -Z1 "$z" | grep -Ec '\.(so|dll)$' || true)
  test "$count" -eq 1
done
```

检查点：

- `checksums.txt` 只包含三个 zip，且 hash 全部通过。
- 每个 zip 的**根目录**只有 `autoclaw2api.so` / `autoclaw2api.dll`，不能有子目录或多个库。
- Release 不是 draft、不是 prerelease，并且是最新 Release。
- tag 是 `vX.Y.Z`，资产里的版本是 `X.Y.Z`（没有 `v`）。

## 6. 更新 registry（建议）

正常发版时商店以最新 Release 为准，`registry.json` 不改也能更新；同步一下兜底版本即可：

```bash
cd C:/Users/test/Desktop/cpa/cpa_plugins
node -e "const r=require('./registry.json'); console.log(r.plugins.map(p=>p.id+':'+p.version).join('\n'))"
# 编辑 registry.json，把 autoclaw2api.version 改成 X.Y.Z（不带 v）
git add registry.json
git commit -m "registry: autoclaw2api X.Y.Z"
git push
```

`autoclaw2api` 的条目：

```json
{
  "id": "autoclaw2api",
  "name": "AutoClaw2API",
  "description": "AutoClaw desktop (GLM / Z.ai) provider for CLIProxyAPI: OpenAI-compatible chat-completions and models, account pool, desktop-grade request fingerprint and built-in phone login page.",
  "author": "JustCnc",
  "version": "0.1.0",
  "repository": "https://github.com/JustCnc/autoclaw2api",
  "homepage": "https://github.com/JustCnc/autoclaw2api",
  "tags": ["Provider", "AutoClaw", "GLM"]
}
```

规则：

- `id` 必须保持 `autoclaw2api`，不能随意改，否则商店认成另一个插件。
- `version` 不带前导 `v`；它只是最新 Release 查询失败时的展示兜底。
- `repository` 必须是 `https://github.com/JustCnc/autoclaw2api`，不能带结尾斜杠。
- 详细规则见 `cpa_plugins/README.md`。不要在本仓库提交二进制。

验证 raw URL：

```bash
curl -sS https://raw.githubusercontent.com/JustCnc/cpa_plugins/main/registry.json | head
```

## 7. 本机 CPA 验证

配置里应有：

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/JustCnc/cpa_plugins/main/registry.json"
```

重启 EasyCLIProxyAPI（会中断当前请求；它自己会拉起 `cpa-core/cli-proxy-api.exe`）后查询：

```bash
KEY=$(grep -E '^[[:space:]]*secret-key:' \
  "C:/Users/test/Downloads/EasyCLIProxyAPI-v0.3.53-Windows-amd64/cpa-core/config.yaml" \
  | head -1 | sed -E 's/.*secret-key:[[:space:]]*"?([^"]+)"?.*/\1/')

curl -s --noproxy '*' \
  -H "Authorization: Bearer $KEY" \
  http://127.0.0.1:8317/v0/management/plugin-store \
  | node -e "let s='';process.stdin.on('data',d=>s+=d).on('end',()=>{const r=JSON.parse(s);const p=(r.plugins||[]).find(x=>x.id==='autoclaw2api');console.log(JSON.stringify({sources:r.sources,plugin:p},null,2))})"
```

预期：

- `sources` 包含官方源和 `JustCnc/cpa_plugins`。
- `autoclaw2api` 的 `installed` / `installed_version` / `update_available` 符合当前版本。

从商店安装/更新后，确认注册状态：

```bash
curl -s --noproxy '*' \
  -H "Authorization: Bearer $KEY" \
  http://127.0.0.1:8317/v0/management/plugins
```

应看到 `registered: true`、`effective_enabled: true`，版本为新版本。

最后做一次最小请求（用现有 CPA client key，不要写进仓库）。模型名带 `autoclaw/` 前缀：

```bash
curl -sS --noproxy '*' \
  -H "Authorization: Bearer $CPA_CLIENT_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"autoclaw/zai_glm-5.3-flash","messages":[{"role":"user","content":"Say OK"}],"max_tokens":16}' \
  http://127.0.0.1:8317/v1/chat/completions
```

限流行为可以顺手回归一下（`rate_limit_rpm: 30`、`rate_limit_burst: 20`）：
并发 40 应该得到约 20 个 200 + 20 个本地 429，紧接着的单发仍然 200。

## 8. 失败处理与回滚

- **workflow 报 tag 与 pluginVersion 不一致**：不要改 workflow 绕过；把 `schema.go` 改成
  与 tag 一致的版本，提交后发新 tag。
- **Release 资产缺失 / checksums 对不上**：先用 `workflow_dispatch` 重跑同一个 tag；
  仍失败则发新 patch 版本。不要手工上传一个名字不对的 zip。
- **新版本有问题**：发新的 patch 版本修复；不要静默删除用户已经下载的 Release。
- **registry 写错**：修正 `registry.json` 后 push；raw URL 会更新，商店刷新即可。
- **GitHub API 限流**：workflow 会回退到 `git log` 生成 notes；商店可能暂时显示
  registry 的兜底 `version`，稍后重试即可。
- 不要修改资产命名、zip 结构、插件 ID 或 workflow 的 runner 基线（`ubuntu-22.04`），
  否则商店会拒绝安装或旧系统无法加载。

## 9. Agent 边界

可以做：

- 修改版本、代码、文档、测试；本地跑 `go test`；提交、push、打 tag。
- 用从配置里读取的管理密钥查询本地 CPA API。
- 有 `GH_TOKEN` 时创建/查看 Release、查看 workflow 日志、下载并校验资产。
- 更新 `cpa_plugins/registry.json` 并 push。

必须先问人：

- 没有 token 时发布 Release。
- 删除/移动已发布的 tag 或 Release。
- 修改插件 ID、资产命名、zip 结构、registry schema。
- 添加 LICENSE 或改变许可证。
- 强制 push `main`。

绝对不要：

- 把 token、账号 JSON、access/refresh token、管理密钥写进仓库或提交信息。
- 提交 `dist/`、`*.exe`、`*.dll`、`*.so`、`.pi/`、`plugin-data/`。
- 用 `git add -A` 前不看 `git status`。
- 在 Release 里放多个动态库或嵌套目录。

## 10. 最短检查清单

```text
[ ] schema.go pluginVersion = X.Y.Z
[ ] gofmt / go test / git diff --check 通过
[ ] commit + push main
[ ] tag vX.Y.Z 指向版本 commit，并 push
[ ] 发布正式 Release（非 draft / 非 prerelease）
[ ] workflow 绿灯
[ ] 三个 zip + checksums.txt + notes 存在
[ ] sha256sum -c checksums.txt 通过
[ ] 每个 zip 根目录只有一个动态库
[ ] registry version 已同步（建议）
[ ] 本地插件商店能识别新版本 / 能安装或更新
[ ] 最小 chat 请求成功
```
