# AutoClaw2API

把本机 AutoClaw 2.x 已登录账号的托管模型网关，接成 CLIProxyAPI（CPA）插件，
对外提供 OpenAI 兼容的 `/v1/chat/completions` 与 `/v1/models`。

本插件不是照搬 [Agent2API](https://github.com/aimod-cc/Agent2API) 的实现：上游路径、
请求头、签名、系统提示词门槛和 token 续期方式都是从本机安装的 AutoClaw 2.2.2
（`resources/app/dist/main.cjs`）里重新解包、抓包核对过的。

## 已实现

- `chat/completions`：流式 / 非流式、工具调用、`reasoning_content`、`usage`
- `models`：从 `GET /autoclaw-proxy/proxy/autoclaw-model-config` 拉取模型目录并缓存
- 多账号：配置里的主账号 + `accounts_dir/*.json` + CPA 里导入的 auth JSON
- 授权登录：控制台内嵌阿里云验证码，换取 AutoClaw 官方 OAuth 授权链接（Z.ai / Google），
  loopback 回调自动收 token；手机号验证码登录也在控制台里
- 插件页面：CPA 里的「AutoClaw2API」控制台（账号池 / 模型 / 请求 / 积分 / 日志 / 配置 / 测试对话）
- 积分余额：读 AutoClaw 资产接口 `/agent-assetmgr/api/v2/wallets`，按账号展示总积分与钱包拆分
  （奖励 / 每日 / 订阅 / 加油包 / 其他），带缓存，只读查询不消耗积分
- 登录奖励 / 每日积分：AutoClaw 的积分不是「主动领取」而是挂在客户端动作上 ——
  `POST /userapi/v1/user-info`（客户端启动那次「登录」）会触发登录奖励入账（如 +4000），
  每日任务走 `GET /autoclaw-proxy/proxy/autoclaw-task-list` +
  `POST /autoclaw-proxy/proxy/autoclaw-task-complete`。插件登录成功后会补打一次 user-info，
  后台每 `reward_interval_minutes` 分钟给当天还没领的账号跑一遍：触发登录奖励 + 领取每日任务
  （`daily_signin` 每日签到、`daily_inspiration_center` 灵感中心等，邀请类任务不代领）
- 每账号独立设备：每个账号有自己的 `device_id` 与设备名，登录、刷新、user-info、任务接口都用它，
  账号池看起来不是同一台机器；从别处导入的重复 device_id 会被自动换成新的
- 账号身份：控制台账号列表显示登录邮箱（z.ai 把邮箱放在 access_token 的 `jti` 里）、
  设备名与设备码
- 请求日志：每次转发的耗时、首字延迟、token、缓存命中、状态码、错误，环形缓冲；
  调用结束后回查 `/agent-assetmgr/api/v1/ledgers_std`，按模型 + token 数 + 时间窗匹配账单，
  把这一条实际消耗的积分回填到对应行
- token 自动续期：`/userapi/v1/refresh`，必要时回退 `/userapi/v1/agent-refresh`
- 出站伪装：
  - `X-Version` / `X-Tm` / `X-Product` / `X-Channel` / `X-Lang`
  - `X-Auth-Appid` / `X-Auth-TimeStamp` / `X-Auth-Sign = md5(appId&ts&appKey)`
  - `X-Authorization`（模型网关不用 `Authorization`）
  - `X-Trace-Id` / `X-Request-Id` / `X-Request-Model` / `X-Client-Type: pc`
  - `x_trace_id: autoclaw-desktop` / `X-Agent-Id: main` / `X-Session-Id` / `X-Query-Id`
  - 请求体字段顺序、`stream_options.include_usage`、`store:false`、`max_completion_tokens`
  - 自动在 system prompt 前注入 AutoClaw 桌面端固定前缀
    `You are AutoClaw. Answer the user directly and concisely.`
    （缺这个前缀时上游会返回 `406 Not Acceptable` 空响应）

## 目录

```text
abi.go / abi_nocgo.go   CLIProxyAPI 插件 C ABI
schema.go               ABI JSON 结构
plugin.go               生命周期与 RPC 分发
config.go               插件配置
accounts.go             账号池、auth 记录、token 刷新
catalog.go              模型目录
identity.go             请求头 / 会话 ID / HTTP 客户端
payload.go              请求体归一化与 system 前缀注入
executor.go             chat / stream 执行
ratelimit.go            按账号的本地令牌桶与上游 403 限流识别
sse.go                  SSE 解析与聚合
login.go / loginpage.go 手机号授权登录页
management.go           插件页面注册、管理接口、账号操作
web/console.html        内嵌控制台页面（go:embed）
state.go                私有数据、日志
tools/export-autoclaw-account.mjs
                        从本机 AutoClaw 登录态导出账号 JSON
build.sh                构建 DLL/SO
```

## 安装

### 方式 A：CPA 插件商店

CPA 的 `plugins.store-sources` 里加上插件源，重启后在「插件商店」搜索
`AutoClaw2API` 直接安装/更新：

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/JustCnc/cpa_plugins/main/registry.json"
```

### 方式 B：手工放二进制

把 `dist/plugins/<goos>/<goarch>/autoclaw2api.dll`（Windows）或
`autoclaw2api.so`（Linux）放到 CPA 的 `plugins/<goos>/<goarch>/` 下，然后在
`config.yaml` 中启用：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    autoclaw2api:
      enabled: true
      priority: 1
      data_dir: "/CLIProxyAPI/plugin-data/autoclaw2api"
      # 可以直接填 token，也可以留空，用 accounts_dir / 授权登录
      # access_token: ""
      # refresh_token: ""
      # device_id: ""
      accounts_dir: "/autoclaw-accounts"
      # OAuth 授权回调端口：0 = 自动挑空闲端口（候选 18432/19654/19723/53699/1876x）
      login_callback_port: 0
      # 上游单账号约 30 次/分钟、40 次突发；本地按账号做令牌桶平滑
      rate_limit_rpm: 30        # 0 = 关闭本地限速
      rate_limit_burst: 20
```

完整字段见 `config.example.yaml`。

### 方式一：从本机 AutoClaw 导出账号 JSON（推荐，最快）

AutoClaw 的登录态在
`%APPDATA%\AutoClaw-oversea-official\accounts\<account>\account-credentials.enc`，
用 Chromium 的 `Local State` 里的 DPAPI 密钥 + AES-256-GCM 加密。Windows 上运行：

```powershell
node tools/export-autoclaw-account.mjs --out .\accounts\default.json --alias default
```

然后把 `accounts` 目录挂进容器，或复制到 `accounts_dir`。插件会读取：

```json
{
  "type": "autoclaw2api",
  "alias": "default",
  "access_token": "eyJ...",
  "refresh_token": "eyJ...",
  "device_id": "....",
  "user_id": "184702"
}
```

也可以直接把这份 JSON 放进 CPA 的 auth 目录，插件会通过 `auth.parse` 接收。

### 方式二：在插件控制台里授权登录（推荐）

AutoClaw 桌面端的登录是一套 OAuth 授权链接：

```text
POST https://autoglm-api.autoglm.ai/userapi/overseasv1/zai-oauth-url
  { navigate_uri: http://localhost:<port>/auth/callback-zai, captcha proof }
  -> https://chat.z.ai/api/oauth/authorize?client_id=…&redirect_uri=…&state=…
浏览器登录后 -> http://localhost:<port>/auth/callback-zai?code=…&state=…
POST /userapi/overseasv1/zai-oauth-login { code, state, navigate_uri } -> tokens
```

插件把这整套搬进了控制台：

1. 「概览 → 新增账号 → 授权链接」，选 Z.ai 或 Google；
2. 点一下阿里云人机验证（网关对 `*-oauth-url` 强制要求 `captchaVerifyParam`，
   缺了会返回 `631002 当前版本已停止服务`，参数不对返回 `630014`）；
3. 点「生成授权链接」，插件带着验证码参数向上游换到真正的
   `https://chat.z.ai/api/oauth/authorize?...`，并在浏览器里打开；
4. 你在浏览器里完成登录，Z.ai 会把 `code` 回调到插件的
   `http://localhost:<login_callback_port>/auth/callback-zai`；
5. 插件用 code 换 token，账号自动进池，控制台轮询到成功后刷新。

**回调没自动到达时**：把浏览器地址栏里的整条链接
（`http://localhost:18432/auth/callback-zai?code=…&state=…`）粘贴到弹窗里的
「回调链接」再点「完成登录」，插件会解析 code/state、按粘贴链接里的 host 还原
`navigate_uri` 并换 token。端口被 AutoClaw 客户端占用、链接在另一台机器上打开、
或者浏览器停在回调页没转发，都走这条路。

Docker 部署时把回调端口映射出来（端口由 `login_callback_port` 指定，0 表示自动选择，
日志里会打印实际端口）：

```yaml
ports:
  - "19654:19654"
```

手机号验证码也在同一个弹窗的第二个标签页里，直接调 `/userapi/v1/agent-send-code` 和
`/userapi/v1/agent-login`，不需要额外的本地页面或端口。

手机号验证码只覆盖 AutoClaw 的手机号登录；如果账号返回“需要在企业身份中选择”，
请用方式一在已登录的 AutoClaw 客户端导出账号 JSON 再导入。

## 预编译产物

每个 Release 都带现成的二进制，直接下载对应平台即可（也支持从 CPA 插件商店安装）：

```text
autoclaw2api_<version>_linux_amd64.zip    → plugins/linux/amd64/autoclaw2api.so
autoclaw2api_<version>_linux_arm64.zip    → plugins/linux/arm64/autoclaw2api.so
autoclaw2api_<version>_windows_amd64.zip  → plugins/windows/amd64/autoclaw2api.dll
checksums.txt                             → sha256 校验
```

<https://github.com/JustCnc/autoclaw2api/releases>

## 插件页面

安装并启用插件后，CPA 的插件菜单里会出现「AutoClaw2API」，对应地址：

```text
/v0/resource/plugins/autoclaw2api/console
```

页面只有一个 HTML + 原生 JS，不需要额外静态资源，也不占端口。包含：

- **概览**：账号池可用数、模型数量、本地限速、运行时长、积分余额、上游指纹摘要、最近日志
- **账号**：列表（固定高度滚动，含登录邮箱、设备名/设备码）、刷新 token、重置冷却、禁用/启用、移除、导入、导出
- **模型**：目录列表（固定高度滚动）、上下文/最大输出、是否对外暴露、手动刷新目录
- **请求**：最近转发记录（固定高度滚动）+ 概况（总数/成功/失败/平均首字/平均耗时/token/缓存/积分），
  可自动刷新，积分由上游账单回填
- **积分**：每个账号的余额卡片（总积分 + 钱包拆分）、每日积分/登录奖励卡片（任务列表 + 今日登录奖励，
  可「领取每日积分」「触发登录奖励」）、积分流水表（时间/模型/扣费/输入输出/说明）
- **日志**：环形缓冲（固定高度滚动），可关自动刷新
- **设置**：只读生效配置，token 类字段打码

所有列表容器都限制了最大高度（表格 400px、请求 460px、流水 420px、日志 440px、配置 420px、概览日志 200px）
并独立滚动，账号或日志再多也不会把页面撑长。

页面用到的接口（前缀 `/v0/management/autoclaw2api`）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/status` | 账号池、目录、限速、积分合计、请求概况 |
| GET | `/accounts` | 账号列表 |
| GET | `/models` | 模型目录 |
| GET | `/requests?limit=100` | 请求日志 + 汇总 |
| GET | `/credits[?refresh=1]` | 每个账号的积分余额 |
| POST | `/credits/refresh` | 强制刷新全部余额 |
| GET | `/credits/ledgers?account=&limit=` | 积分流水 |
| GET | `/tasks?account=` | 每日任务列表 + 今日登录奖励 |
| POST | `/tasks/claim` | 领取每日任务（`{account, task_id}`，task_id 留空=全部） |
| POST | `/rewards/login` | 触发一次登录奖励（user-info） |
| GET | `/logs?limit=300` | 插件日志 |
| GET | `/config` | 只读生效配置 |
| POST | `/catalog/refresh` / `/accounts/refresh` / `/accounts/reset` | 目录 / token / 冷却 |
| POST | `/login/*` | 授权登录、验证码、回调 |

页面通过 `<管理密钥>` 调用 `/v0/management/autoclaw2api/*`：在右上角「管理密钥」里填
CPA 的 `management.secret-key`，只存本机 `localStorage`；也可以用
`?key=<secret-key>` 直接带上。没设置密钥时留空即可。
## 构建

```bash
# Windows DLL（在 Windows + MinGW 下）
CC_WINDOWS=x86_64-w64-mingw32-gcc ./build.sh windows/amd64

# Linux SO（在 Linux 或带交叉工具链的环境）
CC_LINUX=x86_64-linux-gnu-gcc ./build.sh linux/amd64
```

Docker 里构建 Linux `.so`（本机没有 Docker，未在容器里复测；Windows DLL 已实测）：

```bash
docker build -f build.Dockerfile -t autoclaw2api-build .
docker run --rm -v "$PWD/dist:/out" autoclaw2api-build
```

把 `dist/plugins/linux/amd64/autoclaw2api.so` 放到 CPA 容器的
`/CLIProxyAPI/plugins/linux/amd64/` 即可。

本机验证时使用的工具链：

- Go 1.27.3 windows/amd64
- MinGW GCC 16.2.0
- CPA CLIProxyAPI 8.0.44

## 本机测试结果

用独立的 CPA 8.0.44 实例（端口 8318/8319）加载本插件，使用本机已登录的
AutoClaw 账号，实际结果：

- `GET /v1/models` 返回 `autoclaw/zai_auto`、`autoclaw/zai_auto-fast`、
  `autoclaw/zaicoding_glm-5.3`、`autoclaw/zai_glm-5.3-flash`
- 非流式 `POST /v1/chat/completions` 返回 200，含 `reasoning_content`
- 流式返回标准 `chat.completion.chunk`，以 `data: [DONE]` 结束
- 自定义 system prompt 会保留在固定前缀之后
- `tools` / `tool_choice` 能触发上游 `tool_calls`
- 手工放入 auth 目录的账号 JSON 能通过 `auth.parse` 被 CPA 识别并成功调用
- 授权登录页 `/health` 与 `/login?state=...` 正常返回
- 加压脚本见 `tools/loadtest.mjs`（`node tools/loadtest.mjs burst` / `stream` / `sustain`）

## 上游限流实测（单账号）

在 CPA 8.0.44 + 本插件下用真实账号做了压测（同一出口 IP、同一账号）：

| 场景 | 结果 |
| --- | --- |
| 完全空闲后一次性并发 50 | 40 个 200，10 个 403「请求过于频繁」 |
| 并发 1/2/4/8/12/16/20/24/28 | 全部 200，延迟 1–3s，无降速 |
| 1 rps（60 次/分钟）持续 2 分钟 | 94 成功 / 25 个 403，出现 2 段 6–19 秒封禁 |
| 0.75 rps（45 次/分钟）持续 100 秒 | 72 成功 / 3 个 403 |
| 0.5 rps（30 次/分钟）持续 90 秒 | 45/45 全部成功 |
| 2 rps 持续 60 秒 | 前 26 秒 48 成功，之后连续 34 秒全部 403 |
| 12 个并发请求，每个 17.7k prompt tokens | 12/12 成功（不限 token 吞吐） |
| 流式输出 | 约 50–100 tokens/s（模型解码速度），无额外 TPS 限速 |
| 单次输出长度 | `max_completion_tokens` 是软限制，模型可能输出更多 |

结论：

- 限制的是**请求次数**（不是 token、不是 TPS）。桶容量约 40，可持续约
  30 次/分钟；越界后上游返回 403 `{"code":810002,"message":"请求过于频繁，请稍后再试"}`，
  没有 `Retry-After` 头，惩罚从几秒逐步升级到 30 秒以上，继续打会延长。
- 上限按账号计算：多账号可以线性叠加（加账号即可提升总吞吐）。
- 插件默认按账号做令牌桶平滑（`rate_limit_rpm: 30`、`rate_limit_burst: 20`），
  超出时本地直接返回 429 + `Retry-After`，不再把上游打成惩罚状态。
- 上游的 403 `810002` 会被归类成可重试的 429（账号冷却 20 秒），多账号时 CPA 会
  自动切到别的账号，而不是把 403 直接抛给客户端。
- 超限时 CPA 客户端收到的是标准 `429 rate_limit_exceeded`，按指数退避重试即可；
  OpenAI SDK / Claude Code / Codex 都会自动重试。

## 发版

版本号唯一来源是 `schema.go` 的 `pluginVersion`，tag 必须是 `v<pluginVersion>`。
完整步骤见 [`RELEASING.md`](RELEASING.md)。

## 注意

- 仅供个人学习 / 自用，请遵守 AutoClaw 的服务条款。不要用于商业转售、批量账号
  或绕过计费。
- `access_token` / `refresh_token` / `account-credentials.enc` 都是敏感凭据，不要提交
  到仓库或发给别人。
- OAuth 回调只监听 `localhost` 语义的端口，并且只接受插件自己发起的 state；不要把回调端口映射到公网。
- 插件和 AutoClaw 客户端共用同一个 `refresh_token`。插件自动续期后，AutoClaw
  客户端的本地登录态可能失效；重新打开客户端登录，或重新运行导出脚本即可。
  如果不想影响客户端，把 `refresh_margin_seconds` 设得很大，或在 token 过期前
  重新导出。
- 上游客户端升级后，如果 `X-Version`、`app_key` 或系统提示词前缀变化，需要同步
  修改 `config.go` 中的默认值。
