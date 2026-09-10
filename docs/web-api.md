# Web UI + `/api/*` 接口契约

> 从 AGENTS.md 拆出。**改 web/API/stats 前必读**。

`web.enabled`（默认 true）时 daemon 同一 mux 挂 `/ui/`（embed 静态资源）、`/api/`（JSON）和 `/metrics`（Prometheus exposition）。鉴权是**可选的 S2 层**（`web.auth`，见下节）：默认（loopback 部署）两个面都无鉴权，`internal/config` 的 `requireLoopbackListen` 拒绝非回环 `listen`（`0.0.0.0`、空 host `:PORT`、`[::]`、内网 IP、域名），只放行 `127.x`/`[::1]`/`localhost`；**配齐两个 auth 文件后非回环 listen 被允许**（`requireAuthForNonLoopback`：validate 先跑 loopback 检查，失败再看 S2 gate），面向 LAN/团队部署。

## `web.auth`（S2 可选鉴权，`internal/webauth`）

两个面各自独立开关，文件不配置 = 该面无鉴权（历史 loopback-trust 行为）：

```yaml
web:
  auth:
    admin_token_file: ~/.model-proxy/admin_token.txt   # admin 数据面：/api/*、/metrics、/debug/*；/ui/ 是无秘密的登录 bootstrap
    api_keys_file: ~/.model-proxy/api_keys.txt          # 转发面：/v1/*、/v1/messages、/v1/models；/health 保持开放供 liveness 探针
```

- **文件格式**：一行一个 token；空行与 `#` 注释忽略（`webauth.loadTokens`）。
- **校验细节**：token 从 `Authorization: Bearer <token>` 或 `x-api-key` 头提取（`BearerFromRequest`，OpenAI/Anthropic 客户端惯例都接受）；对整个 token 集做 constant-time compare（逐条 `subtle.ConstantTimeCompare` 后 OR），计时不泄漏命中位置。已配置但内容为空的集合拒绝一切。
- **TTL 缓存 + 有界 stale-if-error**：token 集缓存 10s（`cacheTTL`），文件编辑（轮换/吊销）无需重启/reload 即生效，hot path 不每请求读盘；任一配置文件**缺失**时整个 token 集立即变空（拒绝一切）。其他暂态读错在已有成功缓存时只允许从**首次失败**起继续服务旧集合最多 10s（失败不滑动续期），超过窗口或冷启动无缓存时拒绝；下次成功读取会换入新集合并清零失败窗口。
- **生效点与换代**：admin 数据面检查在 `internal/web` transport 的 `guardAdminAuth`（覆盖 `/api/`、`/metrics`；每个请求从 `Options.AdminAuth` 闭包只捕获一次当前 Source，认证与 LAN Origin 策略不混代）和 `internal/app/proxy_http.go` 的 `Handler` 入口（web-disabled 部署的 admin 端点只认 admin token）；转发面检查同在 `Handler` 入口（`apiKeys` Source）。两个 Source 由 `applyAuthSources` 随 config generation 整体换入（原子指针，只换路径引用；token 内容的 10s TTL 缓存独立于 reload）。拒绝响应 401：admin 面带 `WWW-Authenticate: Bearer realm="model-proxy-admin"`。
- **浏览器会话**：启用 admin auth 时 `/ui/*` 仍可读，因为 embed 的 HTML/JS/CSS 不包含 runtime 数据或凭据；首次 API 401 后 UI 把用户输入的 bearer `POST /api/auth/session`，服务端返回 session cookie（`HttpOnly`、`SameSite=Strict`、`Path=/api`、TLS 时 `Secure`，不设持久过期），随后普通 fetch 与 `EventSource('/api/events')` 自动携带。cookie 只在 `/api` 下发送，不能进入 `/v1/*` 转发或上游；其值承载 URL-safe 编码的 bearer，每次请求仍由当前 Source 复核，所以 token 文件轮换/吊销在 TTL 后同样失效。JavaScript 收到响应后立即清空输入，不写 local/session storage、URL 或日志；`DELETE /api/auth/session` 清除 cookie。`/metrics` 不在 cookie Path 内，继续显式带 bearer。直接 HTTP 的 LAN 会话不能抵抗同网段窃听，跨主机部署应置于受信网络、VPN、SSH tunnel 或 TLS reverse proxy 后。
- **非回环门槛**：`requireAuthForNonLoopback`（`internal/config`）只在两个文件都配置时放行非回环 `listen`；loopback 保留无鉴权默认，两文件均可选。开启 `api_keys_file` 后，takeover 写入客户端配置的 `PROXY_MANAGED` 占位 key 必须换成文件里的真实 key。

**浏览器侧防线**（`internal/web` `guardBrowserOrigin`，挂在 `serveUI`/`serveAPI` 入口；proxy-owned admin 端点经 `GuardAdminBrowserOrigin` 复用）：
loopback 挡不住“借用户浏览器之手”的请求，所以凡携带浏览器身份头（`Origin` 或 `Sec-Fetch-Site`）的请求额外要求：(1) `Sec-Fetch-Site: cross-site` 一律拒绝；(2) `Origin` 与请求 `Host` 一致，封住 evil.com 发起的 CORS simple request；(3) 未启用 admin auth 时 `Host` 只能是 loopback；启用后额外允许 LAN **IP literal**（如 `192.168.1.20:15721`）或与 config `listen` 完全相同的 hostname:port，但仍拒绝其他 DNS host，避免 rebound 域名在同源视角下绕过。GET `/api/config` 原文可能含 static provider key，读与写同等防护。无浏览器头的 CLI/curl（`daemonctl`、脚本）仍以 bearer 为权威，不受浏览器 Host 规则影响。

**前端布局契约**：Status→Logs 每条日志是「行号 gutter + 正文」两列网格；行号与 gutter 右边框留 2px，gutter 背景只覆盖行号列，鼠标悬停标出整条逻辑行，单击选中该行（改变行号前景色，不干预原生选择）。Config→Raw YAML **硬最小高度 480px**，按编辑器 viewport top + 卡片下方 chrome 重新计算，可见空间大于 480px 铺满、不足仍 480px 并允许滚动，绝不靠固定 `100vh - 常量` 推测。Raw YAML 编辑时 500ms 防抖调 `/api/config/validate` 内联报错（校验失败不阻塞到保存才暴露，有错禁用 Save；点击错误跳转对应行）。编辑器还维护一份**需重启键名单**（`listen`、`log_level`、`log_file`、`web.enabled`、`request_log.*`、`stats.db_path`、`stats.retention`、`budgets`、`scheduling.quota_poll_interval`——依据 pitfalls #29/#31、`initStats` "startup-only"、`proxy_lifecycle.go` watcher 启动期创建与 request_log/logger 启动期构建的代码事实）：当前文本相对磁盘基线改动到这些键时显示 ⓘ "需重启 daemon 生效" 提示；名单是前端常量（app.js `RESTART_KEYS`），行扫描只做提示、校验权威仍在服务端。Config→Settings 表单（app.js `SETTINGS_GROUPS`）把 `log_level`/`log_file`、`scheduling.*`、`request_log.*`、`stats.*`、`cache.*` 从 YAML 编辑器提到表单：控件一律用 `.field`/`.field.check` 样式组，缺省键的代码默认值只作 placeholder，**只回写与加载值不同的字段**（`settingsDiff`，纯函数），清空字段发送 `null` 删除键回到默认；字段名旁的 info 图标按钮点击弹出该键的含义与代码默认值（`help`/`def` 与控件同源，契约测试锁定每个字段都有两者；图标为内联 SVG，`currentColor` 随主题在 muted/accent 间切换），各块用 `.settings-group` 分隔线 + padding 分组；`request_log`/`stats` 组与 `log_level`/`log_file`/`scheduling.quota_poll_interval` 保存后显示需重启提示（与上段同一份代码事实）。

| 方法 | 路径 | 请求 | 响应 | 备注 |
|---|---|---|---|---|
| POST/DELETE | `/api/auth/session` | POST：`Authorization: Bearer <admin token>`；DELETE：— | 204 + Set-Cookie | 浏览器 admin 会话建立/清除；POST 必须显式 bearer 且同源，cookie 为 HttpOnly/SameSite=Strict/`Path=/api`，响应 `Cache-Control: no-store`；admin auth 未启用时 POST 400 |
| GET | `/api/status` | — | `{uptime,version,listen,health{...},model_locks{...},quota{...},schedule{...},counters{...},cache{...},warnings}` | handler 只消费 admin dashboard 投影的脱离式快照；`internal/app/web_adapter.go` 的 DashboardState 端口按 `Proxy.mu → internal/runtime.Manager` 捕获同一 config generation 的 listen/warnings/cache 与 health/model-lock/quota/pin/sticky/spread，`schedule` 通过该 snapshot 的只读 `PreviewOrder` 计算，不再次读取 Manager，故同一响应的 health/quota/pin/sticky/order 不会混代或跨 mutation。内部 map 不外泄。`quota` 是 `QuotaSnapshot` 原样序列化（无 json tag → **PascalCase**）；`quota[name].ExhaustionEta` 是 tracker 按 Δused/Δt 速率算出的 ultimate 窗口耗尽预测（零值=无预测：首快照/速率≤0/断档，语义见 `docs/architecture/runtime-state.md`），配额卡在 ultimate 窗口行尾展示，调度不使用。`cache` = `{enabled,hits,misses,entries,models:[{model,hits,misses,entries}]}`（响应缓存观测；`hits`/`misses` 是自启动/上次 reset 以来的累计计数，`entries` 是当前存活条数 gauge，可能小于 misses——失败的请求 miss 不落条、TTL 懒过期与容量淘汰会移除条目；`models` 按 called（exposed）model 名排序，缺省省略）。`health[name]` 含 `circuit_state`/`available`/`frozen?`/`circuit_until?`/`rate_limited_until?`/`rate_limit_kind?`（429 分类 transient/quota/daily，仅限频中输出；`frozen:true` 仅 operator 手动冻结时输出，`available` 同时为 false）。`model_locks[provider]` = `[{model,until}]`（仅生效中的模型锁，与 health 同一 Manager dashboard 快照，过期不输出；`doctor --live` 用它解释 route 全灭）。`schedule.models[route]` = `{first,ordered[{provider,pool_parent?,priority,tier,surplus,quality_penalty,available,peak}],sticky?,sticky_dwell_remaining_sec?,pools?,pin?,pin_expires?}`；route 有生效 pin 时 `ordered` 是**未 pin 的默认调度链**（`PreviewOrder` 以 `IgnorePins` 重算，unpin 后恢复的顺序），`first` 仍是 pin 生效时的实际首选（可能等于 pin 的池化账号而非 pin 名本身），`pin`/`pin_expires` 标注覆盖关系 |
| GET | `/metrics` | — | Prometheus text exposition（`text/plain; version=0.0.4`） | per-provider 计数器快照（`model_proxy_requests_total`/`failures_total`/`failovers_total`/`rate_limited_429_total` + `latency_milliseconds_sum`/`ttft_milliseconds_sum`），派生自与 `/api/status` 相同的 detached Dashboard 快照（无新锁面）；虚拟计数键（guard、attempts、fusion、routing）以普通 provider 出现，语义同 `/api/stats`；零流量 provider 不出序列。过 `guardAdminAuth` + browser-origin guard；scraper 显式带 bearer，浏览器 session cookie 因 `Path=/api` 不会发送；仅 GET，其余方法 405 |
| GET | `/api/logs?tail=N` | — | `{lines:[…]}` | 读 log 文件末尾 N 行（默认 200，上限 1000）；无 log 路径 → 404 |
| GET | `/api/models` | — | `{providers:{<providerName>:{fingerprint:"<16hex>",probed_at:"<RFC3339>",models:{<modelId>:{chat:"yes\|no\|unknown",anthropic:"…",responses:"…"}}}}}` | 启动期协议探测（startup protocol probe）的每 provider × model 三协议能力矩阵（`internal/runtime/wirecap` ModelStore 的脱离式快照，经 admin 端口投影，不经 `Proxy.mu`）。`probed_at` 是该 provider 最近一次探测/纠正时间；`fingerprint` 是协议相关配置（base urls / provider_id / headers）的指纹——配置变化即整 provider 失效重探，匹配则复用判定（无 TTL）；fingerprint 不含 models 列表，从 config 删除的 model 由下一个探测 pass 从 store 剔除，不会滞留在本响应里。verdict 三值：`yes`/`no`/`unknown`（unknown = 未得出结论，下轮重探，UI 必须与 `no` 区分渲染）；`anthropic` 在未配置 `anthropic_base_url` 时恒为 `no`（定义上不支持，非观测结论）。无数据 provider 不出序列；store 为空 → `{"providers":{}}`。响应 `/responses` 路由级 404 会把该 (provider,model) 的 `responses` 纠正为 `no` 并刷新 `probed_at` |
| GET | `/api/config` | — | `{yaml, summary, provider_models, provider_meta, routes, settings}` | 原文件 verbatim round-trip。`routes` 是**生效路由表**（provider models 推导 + alias 聚合 + priority 继承，显式 `routes:` 覆盖同名条目）；`provider_meta` 为每 provider 的 `{priority, alias}`；`summary.route_count` 按生效表计。`settings` 是 Config 标签页表单（而非 Raw YAML）编辑的标量块投影：`{log_level, log_file, scheduling{circuit_threshold,circuit_cooldown,rate_limit_backoff,quota_cooldown,model_lockout,retry_wait,upstream_timeout,sticky_dwell,quota_poll_interval,quota_switch_margin,quality_error_weight,quality_ttft_weight}, request_log{enabled,dir,max_file_size,max_body_bytes,retention}, stats{db_path,retention}, cache{enabled,ttl,max_entries,max_body_bytes}}`。值 = **文件原始值**（空/零 = 键缺失，前端据此把代码默认值渲染为 placeholder）；例外是 `log_level`/`log_file`（loader 填的生效默认值）。可选整数（`circuit_threshold`、`quota_switch_margin`、`quality_error_weight`、`quality_ttft_weight`）以 **JSON 指针**序列化——缺失为 `null`，显式 `0`（如禁用质量惩罚）保留为 `0`。表单只回写与加载值不同的字段，未触碰的键绝不落盘 |
| POST | `/api/config` | `{yaml}` | `{status:"reloaded"}` / 400 | `saveAndReload`：validate → backup `<configDir>/.model-proxy/back/<base>.<ts>.bak` → atomicWrite → reload。校验失败不落盘；reload 失败从当次备份回滚 |
| POST | `/api/config/validate` | `{yaml}` | `{ok, errors:[{line, message}]}` | 只校验不落盘不 reload：复用与 `saveAndReload` 完全相同的 parse+validate 管线（`internal/config.ValidateYAML`）。语法/类型错误行号直接来自 yaml.v3（type error 每字段一条）；`Config.validate` 语义错误按消息尽力定位到 yaml.Node 键路径（`provider "x"`→`providers.x`、`route "x" target N`→该 target 行、`scheduling.x` 等 dotted 字段→子键），定位不到 `line:0`。`errors` 恒为数组（非 null）。UI Raw YAML 编辑器 500ms 防抖调用，有错时禁用保存；点击错误跳转到对应行 |
| POST | `/api/config/edit` | `{kind,name,data}` | `{status:"reloaded"}` / 400 | 结构化编辑 `kind∈{general,scheduling,request_log,stats,cache,provider,route}`，改 `yaml.Node`（保留注释/键序）→ `saveAndReload`。`general` 写 `listen`/`log_level`/`log_file`；`scheduling`/`request_log`/`stats`/`cache` 写各自块的标量键（见 GET `/api/config` 的 `settings` 列表）；`provider`/`route` 用 `name` 定位。`data` 里某键为 **`null` 即删除该键**（表单“清空=回到代码默认”的语义）；JSON 数字按整数/最短小数落成 YAML 标量（`1073741824` 不写成 `1.073741824e+09`）。`request_log.*`/`stats.db_path`/`stats.retention` 写入后 reload 成功但运行期不生效，需重启 daemon（UI 负责提示） |
| GET | `/api/presets` | — | `{presets:[{name,provider_id,base_url,usage_url?,billing?,models[]}]}` | 内置预设目录（`internal/presets`，派生自内置注释模板，过滤未注册 provider，排除内网 aqp）；UI Add Provider 向导的数据源 |
| POST | `/api/presets/<name>` | — | `{status:"added",warnings:[模型名…],reload_warning?:"…"}` / 400 | 合并模板 provider 块到 config.yaml（幂等：块已存在则跳过合并）→ 与 `/api/config/edit` 同管线 validate+saveAndReload；`warnings` 只含多 provider 聚合模型名（该模型同时被其他已配置 provider 服务且无显式 route —— 提示用 provider priority 控制顺序），UI 需展示；reload 失败时块已落盘但 daemon 仍是旧 generation，错误串放 `reload_warning`（与歧义模型名分字段，UI 不得当作模型名渲染）；凭据随后走 `/api/accounts/<provider>` 或 `/api/login/<provider>/start`；未知 preset 400 |
| GET | `/api/accounts` | — | `{providers:[{name,provider_id,billing,accounts:[{id,label,added_at,[email]}]}]}` | **响应无任何 key 字段**（无法泄漏）；id 不掩码（UI 要用它删）。codex 的 `added_at` 恒为空：codex OAuth 文件不落创建时间戳（只有语义不同的 `last_refresh`），不伪造数据 |
| POST | `/api/accounts/<provider>` | `{api_key, access_key?, secret_key?, label?, replace?}` | `{id,status:"added",warning?}` | 仅 apikey 类；aqp/codex 返 400 指向 async login。volcengine 走 `addVolcengineAccount`（探 usage_url 验 Ark API Key + 可选 AK/SK 经签名 GetAFPUsage），其余（含 kimi-code）探 usage_url。落盘后 best-effort reload；reload 失败（config.yaml 不可读/非法，非本次操作所致）时账号已存盘，响应带 `warning`，runtime 保持旧集直到 config 修复并 reload |
| DELETE | `/api/accounts/<provider>/<id>` | — | `{status:"removed",warning?}` | apikey 类经 `accounts.Store.RemoveAccount`；aqp `provider.ClearAqpAccount`；codex `provider.ClearCodexAccount`。三类都走各自 store/provider owner，不能由 Web 直接删文件；file/keychain 跨模式来源与失败重试语义由底层统一处理。落盘后 best-effort reload；失败同上，响应带 `warning` |
| POST | `/api/accounts/<provider>/<id>/test` | — | `{status:"ok"\|"failed",http_status,reason,latency_ms,provider,account_id,model}` | 账号粒度测活（`probe.Callable` 真实最小请求，复用 provider 的 ProbeRequest/ExtraHeaders）；admin 经 `ProbeRuntime` 端口一次 runtime 快照同代捕获 config/impl，模型取该 provider 首个路由目标否则 models[0]；只读不 reload，请求取消会取消上游 probe。UI 账号卡片 Test 按钮 |
| GET | `/api/tokens?window=\|from=\|to=` | — | `{usage:[{provider,model,input,output,cache_creation,cache_read,total,requests}],agents:[{agent,requests,input,output,cache_creation,cache_read,total,models:[{provider,model,requests,input,output,cache_creation,cache_read,total}]}],since,window}` | SSE 扫描器累计的观测用量；`agents` 是同窗口的 agent 维度展开：每 agent 一行汇总（按 total 降序）+ `models` per-(provider,model) 分解（同序），供 UI Agents 卡片。`total` = input+output+cache_creation+cache_read（桶是 anthropic 归一口径，input 不含 cached，故 total 为四桶直和），agent 汇总与 model 行同一定义。**虚拟 key 排除**：`guard`/`attempts`/`routing`/`fusion` 是计数器虚拟命名空间（非上游 provider，见 `/api/analytics` 节），`admin.Service.Tokens` 投影时剔除，不出现在 `usage` 中。**时间范围**：无参数 = 内存累计（随 reset 一同清零）；`window=1h\|24h\|7d\|all`（遗留预设契约，保留兼容）或 `from`/`to`（unix 秒或 RFC3339，与 `/api/stats` 同一解析器，单边可省略=该侧无界）改为聚合 SQLite 分钟桶。边界语义：from 向下取整到分钟边界（边界分钟整段计入），to 落入的分钟整段计入（bucket start ≤ to）；窗口视图只覆盖已落盘的完成分钟，当前分钟 live 计数只在 all 视图出现。fail-closed：未知 window、无法解析的 from/to、from>to、window 与 from/to 混用均 400。响应回显生效的 `window`（from/to 模式下为默认 `all`）与分钟截断后的 `from`/`to`。UI Status 页 tokens 区时间维度选择器（触发按钮 + popover：左侧预设列带 ✓——last 1h/today/yesterday/last 7d/last 30d/this month/last month/custom/all time，右侧双月日历选自定义闭区间整天；本地时区对齐；Esc/外部点击关闭并丢弃未完成选段，popover 打开时 5s tick 跳过重渲染）驱动 from/to |
| POST | `/api/tokens/reset` | — | `{status:"reset"}`；durable reset 失败时 500 | 先清 SQLite，再清内存 + flusher 基线（注意：也清空响应缓存）；持久化失败时保留 live counters/cache，避免重启后历史复活 |
| GET | `/api/stats?from=&to=&provider=&model=&bucket=` | — | `{from,to,bucket,buckets:[...]}` | 存储 1 分钟桶；`bucket` 仅展示聚合（SQL GROUP BY）。buckets 含 `avg_latency_ms`/`avg_ttft_ms` |
| GET | `/api/agents?from=&to=&agent=&provider=&model=&bucket=` | — | `{from,to,bucket,buckets:[...]}` | per-(agent,provider,model) 桶（`agent_buckets`：requests/input/output/cache_creation/cache_read/latency_ms_sum/failures） |
| GET | `/api/requests?model=&provider=&session=&status=&errors=&from=&to=&limit=&shadow=` | — | `{enabled,records:[summary…],facets:{providers,models,provider_models}}` | request_log 查询（未启用 → `{enabled:false,records:[],facets:{providers:[],models:[],provider_models:{}}}`）。summary 含 `agent`、`shadow` 布尔，以及从响应体解析出的 `input`/`output`/`cache_read`/`cache_creation`（`UsageOnly` 投影，因此 summary 不返回 request/response body 和 response_headers）；`shadow=only\|exclude` 过滤影子记录；`session=` 精确匹配 `session_id`（先经 `rawPrefilter` 字节包含预筛，避免整库 JSON 解码）。用 `bufio.Reader.ReadBytes` 流式扫描全部日志文件，limit 默认 100 上限 1000。`facets` 是**数据驱动**的过滤选项：本次扫描窗口内实际出现的 distinct provider/model，以及 provider→models 映射（供 UI 联动）；在 `filter.matches` **之前**采集，因此不会被请求自带的 model/provider 过滤收窄（UI 下拉不会把自己锁死） |
| GET | `/api/requests/<id>` | — | 完整 record（含 request/response body、`agent`） | replay 的数据源；影子记录 id 为 `shadow-<原id>` |
| GET | `/api/sessions?limit=` | — | `{enabled,sessions:[{session_id,first_ts,last_ts,requests,shadow_requests,errors,providers,models,usage:{Input,Output,CacheRead,CacheCreation},cost_usd}…]}` | 按会话聚合的最新请求日志（limit 默认 50 上限 200，最近活跃在前）；`usage` 的键是 **PascalCase**（`Usage` 结构体无 json tag）；成本同 analytics 定价路径；request_log 关闭 → `{enabled:false}`。只聚合 `session_id` 非空的记录（即客户端确实发了配置 allowlist 里的会话头） |
| GET | `/api/security?kind=&from=&to=&limit=` | — | `{enabled,records:[{ts,kind,request_id?,agent?,protocol?,exposed?,names?,action?,detail?}…],skipped}` | 安全审计日志查询（`internal/observe/seclog`，guard 命中的持久化记录）。`kind` 仅 `secret\|path\|drift`（其他值 400）；`from`/`to` 解析惯例同 `/api/stats`（unix 秒或 RFC3339，inclusive，内部转成审计日志的 unix 毫秒）；`limit` 默认 100 上限 1000，records 按 ts 新到旧；`skipped` 是扫描时跳过的不可读行数。审计目录由 `guard.audit_path`（默认 `~/.model-proxy/log/security/security.log`）的目录派生；`guard.audit: false` 或目录不存在 → `{enabled:false,records:[],skipped:0}`（惯例同 request_log 关闭）。**records 只含模式/路径类别名与动作，绝不含命中内容**（seclog 红线），DTO 不新增任何内容字段 |
| GET | `/api/shadow-report?from=&to=` | — | `{from,to,entries:[{route,primary_provider,shadow_provider,samples,status_match_rate,primary_latency_ms,shadow_latency_ms,latency_diff_ms,primary_size_avg,shadow_size_avg}]}` | 影子评测聚合（按 `shadow-<父id>` 配对，仅成对样本计入） |
| GET | `/api/fusion?workflow=` | — | `{workflows:{<名>:{runs,runs_today,quorum_met,degraded{原因:次数},panel_input/output,judge_input/output,synth_input/output,amplification}},runs:[{run_id,ts,route,workflow,agent,proto,quorum,drafts_used,degraded,legs[{provider,model,kind,status,latency_ms,input,output,err,cut}],judge_used,synth_committed,synth_status,synth_latency_ms,synth_input,synth_output}]}` | 编排观测（`internal/fusion.Registry` 纯内存，200 条 run 环形新到旧；汇总数据从 eventHub 回读，**不依赖 request_log**）。degraded 原因：`insufficient_proposers`/`tools_unsupported`/`body_build_failed`/`budget_exceeded`/`multi_turn`；`amplification`=(候选+judge+汇总)/汇总 token。时间序列走 `("fusion",<workflow>)` 分钟桶（requests=编排次数、failovers=降级次数） |
| GET | `/api/events` | — | SSE 流 | 实时请求监视：先重放 200 条 recent ring 再推 start/end 事件（含 request_id/session_id/agent/provider/status/latency/tokens），15s keepalive。**只覆盖 LLM 协议路径**（`/v1/messages`、`/v1/chat/completions`、`/v1/responses`）；未知路径（浏览器 `/.well-known/...` 探测、favicon、迷路 GET）在 handler 层直接 502，**不产生 live 事件、不写请求日志**（unrouted model 仍是非空 proto，照旧产生终局 end）。`session_id` 来自 `request_log.session_headers` 允许列表（有序，取第一个非空；默认 `x-claude-code-session-id`/`x-session-affinity`/`x-session-id`/`x-opencode-session`；不含 `x-client-request-id`——多数客户端是每请求 id），与请求日志 `session_id` 同源。**仅用于观测**：不改路由粘性（跨池账号仍按 `x-claude-code-session-id` 粘）。Live 页顶部有 **session 选择器**：选中后隐藏实时表，改用 `/api/requests?session=`（逐请求）+ `/api/sessions`（token/成本聚合）渲染该 session 的请求分析（请求数 / input+output / 缓存读写 / 平均延迟 / 错误 / 成本 / model、provider、agent），实时行与持久化行按 request_id 字段级合并（实时状态/status/latency/provider 优先；持久化 `agent` 与 token 回填缺失值），行可点击展开完整 body。选择 **All (live)** 时是纯实时 ring（最新 100 条事件，不从请求日志拉取持久化记录补齐；历史数据看 session 视图或 Requests 页）。`web.enabled`（默认开）时由 web 层 `/api/` 子树服务并过 admin auth + browser-origin guard；浏览器 `EventSource` 不能自设 Authorization，依靠上述 `/api` session cookie。`web.enabled: false` 时回落主 mux 的 proxy handler 分支并过 admin bearer + `GuardBrowserOrigin`。guard.secrets 命中时另有 `type:"guard"` 事件（detail 仅含模式类型名与动作，绝不含命中内容）；`budgets:` 月度预算越线时另有 `type:"budget"` 事件（provider 字段=scope，detail 为 JSON `{scope, month, threshold_usd, actual_usd}`，每 (scope, 月份, 阈值) 每进程只发一次）。已提交（committed）请求向上游回写响应字节的过程中会额外推送 `type:"progress"` 事件：`received_bytes` 为截至目前已接收的响应字节数，`text` 为响应头 32 KiB 前缀（UTF-8 原文；SSE 流则包含 `data:` 行），按 250 ms / 16 KiB 节流、请求结束时停止；无人订阅 Live 页面时该 tap 关闭，无开销 |
| GET/POST/DELETE | `/api/pin` | POST `{route,provider,ttl_seconds?}` | GET `{pins:[{route,provider,expires_at}]}` / POST `{route,provider,expires_at,status:"pinned"}` / DELETE `{route,removed:bool}` | 运行期 pin（见 `docs/architecture/runtime-state.md`）；`ttl_seconds` >0 时换算为 TTL（省略/0 = 不过期），`expires_at` 为 RFC3339（无过期时为空串）；DELETE 用 query 参数 `?route=<名>` 清除，`removed` 表示是否确有 pin 被移除 |
| GET | `/api/analytics?from=&to=&provider=&model=&granularity=day\|month` | — | `{granularity,from,to,series:[{provider,model,points:[{bucket,requests,input,output,cache_creation,cache_read,cost,priced}]}],totals:{input,output,cost},price_coverage:{priced:[],unpriced:[]}}` | 日历日/月聚合 + **服务端现算等价 payg 成本**（见下「Analytics 等价成本」） |
| POST | `/api/quota/refresh` | 空 body 或 `{"provider":key}` | `{status:"refreshed"[,provider]}` / 400 / 404 | 同步刷新配额缓存（可立即重查 `/api/status`）：空 → `pollAll`，指定 → `pollOne`（key 即 `name` 或 `name#accountID`），未知 key 404；malformed JSON body → 400（与 `/api/health/reset` 一致，不触发全量 poll） |
| POST | `/api/health/reset` | 空 body 或 `{"provider":key}` | `{cleared:[names],model_locks_cleared:n}` | 清冻结运行态（熔断开路冷却、429 限频冷却、模型锁定，含 operator 手动冻结标志），目标立即重试；空=全部，池化父名清全部虚拟；**不清** sticky/pin/剥参 blocklist。`unfreeze` CLI 与 UI Providers 卡 unfreeze 按钮 |
| POST | `/api/health/freeze` | `{"provider":key}`（**必填**，空/缺失 → 400 `provider is required`） | `{frozen:[names]}` / 400 | 手动冻结：provider 被调度排除直到 unfreeze——显式 `frozen` 标志，无到期，成功/失败/限频记录不清除，随 health 段 `frozen:true` 落盘并按同一 config 指纹门控跨重启恢复。**无 freeze-all**（与 `/api/health/reset` 的空=全部刻意不对称：全冻结=自我断供，只有逃生口 unfreeze 保留 no-arg）。匹配在**已知 provider key 全集**（config 名 + 池化虚拟 `name#id`）上做：池化父名冻结全部虚拟，从未失败过的 provider 也可冻结（惰性建条目），unknown 名匹配为空（200 + 空数组，非错误）；**不动** sticky/pin/quota/模型锁/剥参 blocklist。畸形 JSON → 400；落盘成功才 200，否则 500（内存态已生效、持久化滞后）。`freeze` CLI 与 UI Providers 卡 freeze 按钮 |
| POST | `/api/models/refresh` | `{"provider":name}` | `{provider,kept:[],added:[],removed:[],policy_dropped:[],probe_dropped:[{model,reason}],warning?,config_updated:bool}` / 400 / 404 | `models refresh <provider>` CLI 的 daemon 孪生（UI Status→Models 每 provider 的 Refresh 按钮）：拉 upstream 列表（impl 不可用时回退 config ∪ 路由 target 集）→ 策略过滤 → 逐候选三协议矩阵探测（任一腿 yes 即保留）→ 覆盖写 `providers.<name>.models`（保注释 yaml.Node 往返 + config 锁 + 校验写，无净变化跳过）→ 进程内热 reload（失败则从 .bak 恢复并报错）→ 健康探测时用新鲜矩阵**整体替换**该 provider 的 model_caps 缓存（unknown/全失败**不替换**，fail-closed）。安全网同 CLI：fetch/probe 全失败或 impl 缺失 → 未校验写合并集 + `warning`，绝不清空 `models:`。列表字段为空时是 `[]` 不是 null。同步接口，探测期间请求阻塞 |
| POST/GET | `/api/login/<provider>/start`、`/api/login/<session>/poll` | — | `{session_id,...}` / `{state, detail, result, warning?}` | 异步登录（aqp SSO URL / codex device flow）；poll 状态 pending/done/error。`internal/web` 的 task owner 管理轮询，关闭时取消 HTTP/等待；凭据 commit 前响应取消，进入 commit 后完成 save+reload 再退出。done 时若 reload 失败，`warning` 非空（凭据已落盘，runtime 旧） |

**写操作统一热重载**：所有 mutation 落盘后触发进程内 `proxy.reload` —— 同一 worker 进程原地换 cfg/providers，不重启。账号增删虽不改 config.yaml，但 reload→`buildProviders`→`loadPool` 重读池文件，新账号随即展开成虚拟。reload 通过 `runtime.Manager.ReplaceGeneration` 清空 health/sticky/model-lock/paramBlock/spread/quota（operator pin 保留）、重建响应缓存，并 kick `quota.pollAll`。注意：进程内 reload（UI 与 worker 同进程）≠ `serve reload`（给独立进程发 SIGHUP）。

**reload 失败不是静默成功**：`reload` 仅在 `config.yaml` 自身不可读/非法时失败（mutation 写的是池文件，不是 config.yaml，所以正常操作不会触发）。失败时凭据已落盘、不可撤销，故仍返回 2xx，但响应带 `warning`（错误原文）并记一行 `[accounts] reload after mutation failed` 日志；runtime 保持旧集直到 config 修复并下次 reload（普通请求不会重读池文件）。前端在 add/remove 模态框和登录 done 状态展示该 warning。config 编辑走 `saveAndReload`，先校验、失败从备份回滚并返回 400（不同于账号增删的 best-effort）。

`serve status`（CLI）是 `/api/status`+`/api/tokens`（带 `--logs` 再加 `/api/logs`）的终端消费者，内容与 Status 标签页一致（Providers 表含 LAT/TTFT 列）。

## SSE token 扫描器（`tokens.go`，forward 2xx 提交处接入）

`usageScanner` 是 `io.ReadCloser`，仅当 `isSSE(resp.Header)` 包在 `resp.Body` 外，字节**原样透传**（不修改/缓冲/阻塞）；失败静默。bounded 64KB 行缓冲（防 OOM）。commit-on-EOF/close（含客户端断开，`forward` 在 `flushCopy` 后显式 `body.Close()`）。解析只看 `data:` + 首字符 `{` 的行：anthropic shape（`message_start`→input/cache、`message_delta`→output）或 openai shape（`prompt_tokens`/`completion_tokens`，best-effort——`usage` 仅当客户端发 `stream_options.include_usage` 才有；协议转换路径由转换器注入）。`tokenCounter` 纯内存；持久化由 SQLite stats 接管。其内嵌 `mu` 是独立叶子锁，不与 `Proxy.mu` 或 `runtime.Manager` 嵌套。

## 调用统计持久化（`internal/observe/stats` + `internal/app/observe_adapters.go`）

`metricsStore`（per-(provider,model) 原子计数器）+ `tokenCounter` 在 hot path 纯内存，**hot path 不碰 SQLite**。应用层 `statsFlusher` 按墙钟分钟边界 tick，快照 metrics/tokens/agents 并与上次 baseline diff；每一路 delta 先进入带原始 minute 的 pending batch，再按时间顺序写 Store，故某一路瞬时失败不会把累计量挪到后一个时间桶。每路最多保留 360 个 exact-minute batch；更长故障会把最老两桶合并并归到最早 minute 边界（累计量不丢，只降低最老区间的时间分辨率），恢复时每个 tick 每路最多 drain 30 桶，避免长期持有 flusher 锁。`internal/observe/stats.Store` 独占 SQLite schema、additive migration、分钟桶 upsert、查询、retention 和 legacy JSON import。非零 delta 写入 `~/.model-proxy/stats.db`（`minute_buckets` 表，`ON CONFLICT DO UPDATE` 累加，`last_request_at` 用 `MAX`）。`modernc.org/sqlite` 纯 Go（`CGO_ENABLED=0`）；stats 连接的 SQLite busy timeout 为 250ms，避免外部写锁让 flusher 卡满原先的 5 秒；`config.stats.{db_path, retention}` 默认 30d。空闲 tick 仍会重试 pending batch 并执行 retention prune；SIGINT/SIGTERM 会取消正在执行的周期 Store 调用，在 lifecycle loop 停止后 final flush，并在 2 秒 best-effort retry window 内重试 pending 后关闭 Store；永久失败记录剩余批次数。context 可取消普通 Begin/Exec，但 modernc busy handler 和底层文件系统调用不提供绝对 hard deadline，因此契约不承诺 `Proxy.Close` 必在 2 秒内返回。查询 `GET /api/stats`（`bucket` 聚合，存储恒 1 分钟）+ `model-proxy stats` CLI。锁纪律：metrics/token/agent/flusher 都是独立叶子锁，不与其它嵌套；Store 不反向持有 runtime owner。

**延迟与失败口径**：`minute_buckets` 带 `latency_ms_sum`/`ttft_ms_sum`（additive ALTER 迁移，老库自动加列）。stats 延迟用**上游响应头到达**时间（`upstreamMs`，不含客户端慢读；live 事件的 latency 仍是客户端口径）。`evRequests` **只在 commit +1**（失败尝试只计 failovers/failures，不稀释 avg）。`statsFlusher.flush()` 全周期持 `f.mu`，`reset()` 同锁并同时处理三个 runtime counter、SQLite history 与 baseline（否则分钟边界撞 reset 会把全量历史写回刚清空的库）；响应 cache 由外层 `Proxy.resetStats` 清理，不进入 Store。

**agent 维度**（`agent.go`）：`detectAgent` 从 `x-claude-code-session-id`/`claude-cli` UA → `claude-code`、UA 含 `codex` → `codex`、`opencode`、`pi` 产品 token（`pi/` 或 `pi (` 前缀，覆盖 pi-ai 的无版本 UA）→ `pi`（无 UA=`unknown`）。未识别 UA 不再统一记 `other`：优先取 UA 产品 token（首个 `/`、空格、`(`、`;` 前的前缀，小写、仅留 `[a-z0-9._-]`、戰4字节，如 `curl/8.0`→`curl`）；无可用 token 时直接用原始（小写）UA 作为标签（空白折叠为 `-`、去控制字符、截64字节）；仅空白 UA 记 `other`。并行管线 `agentCounter`（key=(agent,provider,model)，独立叶子锁）→ `agent_buckets` 表（requests/input/output/`cache_creation`/`cache_read`/`latency_ms_sum`/`failures`，分钟粒度，同样 additive 迁移）。token 归因经 usageScanner 的 agentSink（非 SSE 无 token 只计 requests）；全失败 502 记 `incRequests`+`incFailure`（到首个尝试目标）。重启时 `agent_buckets` 累计值回 seed 到内存计数器和 flusher 基线（与 provider/model 的 minute_buckets 完全同构），因此 Agents 卡片跨重启连续、首个 flush 不会重复计数。查询 `GET /api/agents`（窗口化范围查询）+ `stats --by-agent`（`--agent/--provider/--model` 过滤，agent 汇总行 + per-(provider,model) 分解行）；UI Status 页 Agents 卡片改用 `GET /api/tokens` 的 `agents` 字段（与 Token usage 同窗口的内存累计，渲染在 Token Usage 区块下方；每 agent 汇总行下嵌 per-(provider,model) 分解行，五个 token 维度齐全）。

## Analytics 等价成本（`internal/pricing` + `internal/web` handler，默认开启）

`GET /api/analytics?from=&to=&provider=&model=&granularity=day|month` 在 SQLite stats 之上做**日历日/月聚合**（存储恒 1 分钟）：`stats.Store.QueryAnalytics` 用 SQL `date(minute,'unixepoch','localtime','start of day'/'start of month')` GROUP BY；bucket = 本地时区自然日/月初的 unix instant，由 Store 经 `time.ParseInLocation(...,time.Local)` 转——不用 `strftime('%s',…)`（会把本地日期误读为 UTC 当天 0 点，偏移一个时区）。每个 point 现算**等价 payg 成本**：price × tokens，**不落盘、不伪造**；未知价 → `cost:null, priced:false`。响应：`{granularity, from, to, series:[{provider, model, points:[…]}], totals:{input, output, cost}, price_coverage:{priced:[], unpriced:[]}}`。**虚拟 key 排除**：`counters.VirtualProviders`（`guard`/`attempts`/`routing`/`fusion`）与真实 provider 共用 (provider, model) 键空间（见 `internal/observe/counters/metrics.go`），只属于 `/api/stats` 的时序；`admin.Service.Analytics` 在投影时剔除，它们不会出现在 `series`、`totals` 或 `price_coverage` 里（同理 `/api/tokens`）。

**价格优先级**（`pricing.Resolve`）：config `prices:` 在组合根 `detachedPricing` 边界复制并转换为 USD/M override，查询时 ÷1e6 转 USD/token；命中后覆盖 `pricing.Catalog`（OpenRouter 目录，bare-name 精确匹配，无 endpoint/后缀模糊匹配）。OpenRouter 目录在 parse 期按 vendor rank 去重（canonical vendor 胜出，如 `deepseek/deepseek-v4-pro` 击败 `openrouter/deepseek-v4-pro`；tilde 别名 `~openai/gpt-5.6-luna` 剥成 bare 名 `gpt-5.6-luna`）。`pricing.ComputeCost`：`input×Prompt + output×Completion + cacheRead×CacheRead + cacheCreation×CacheWrite`。

**新配置**（`internal/config`）：
- 顶层 `pricing:{enabled, ttl, source_url}` — 默认 `enabled:true` / TTL `24h` / `source_url` 默认 `https://openrouter.ai/api/v1/models`（`pricing.DefaultEndpoint`）。`enabled:false` → `pricingSnapshot` 返回 nil 且不抓取目录；显式 `prices:` override 仍可定价，未命中 override 的模型显示 n/a。
- 顶层 `prices:` map — per-model override，单位 **USD per MILLION tokens**（人类单位）；字段 `input`/`output`/`cache_read`/`cache_write`（后两者默认 0）。命中即盖过目录。

**缓存**（`internal/pricing`，镜像 models.dev 模式）：`~/.model-proxy/pricing_cache.json`，TTL 默认 24h（`pricing.DefaultTTL`，可被 `pricing.ttl` 覆盖）。`pricing.EnsureFresh`：fresh → 用；stale → conditional GET（带 `If-None-Match`）；304 → 只刷 `fetched_at` + 持久化；200 → 重建 + 持久化。持久化使用目标目录中的唯一临时文件再 atomic rename，多个进程不会争用固定 `.tmp`，读取者只观察完整 JSON。抓取失败：有旧 → 用旧 + stderr 告警；无 → 空 catalog（未知价显示 n/a，不阻塞 UI）。

**环境变量 `MP_PRICING_URL`**：pricing 端点解析优先级 **config `pricing.source_url` > `MP_PRICING_URL` env > OpenRouter 默认**（`pricing.DefaultEndpoint`）。`PricingConfig.ResolvedSourceURL()`（`internal/config`）在 config 未设时回落到环境变量，**镜像 `MP_MODELSDEV_URL`** 的 test/mirror override 语义，生产路径 `Proxy.pricingSnapshot`（`proxy.go`）经此生效。单测 `internal/config` 的 `TestPricingConfigSourceURL_Precedence` 钉死 config > env > default 三级优先级；目录、缓存与计算测试归属 `internal/pricing/pricing_test.go`。

**Web UI**：`/ui/` Analytics 标签页（`internal/web/assets/`）消费
`/api/analytics`，渲染 token + 等价成本**趋势图**（uPlot；x 轴为 unix 秒，uPlot 的 time scale 单位；每 series 显式 `stroke`——uPlot 1.6.x 不自动分配颜色，缺 stroke 只画坐标轴不画线）+ **成本汇总表**（provider/model/等价成本/占比）。per-(provider,model) 的 token/请求总量在 Status 页 Token Usage，两页不重复；未定价模型（如 `doubao-*`）显示 `n/a` + UI 提示。

**CLI**：`stats --granularity day|month` 或 `--cost` 任一 → `renderAnalytics` 改打 `/api/analytics`（`formatAnalyticsTable`：每 (provider,model) 一行 = 窗口内 SUM，`--cost` 才出 cost 列，未定价 `n/a`；`--json` 原样）。两者都省略 → 走 `/api/stats`，输出与原 `stats` **字节一致**（CLI 契约不变，append-only）。

**锁与依赖纪律**：`Proxy.pricingMu` 独立叶子锁；`pricingSnapshot`/`priceOverrides` 经 `cfgSnapshot()` RLock 读 cfg，不持 `p.mu` 调入。`internal/pricing` 不依赖 main 包的 YAML 配置、Proxy 或 Web；组合根的 `detachedPricing`（`internal/app/proxy_snapshot.go`）是 `PriceConfig → pricing.Override` 的复制/单位边界，admin 端口与 budget watcher 共用。无 stats store → `series:[]`（nil-safe）。

## Web 运行时边界

HTTP/UI transport 统一归 `internal/web`。其 `Server` 不持有 `*Proxy`，只消费
consumer-owned `ReadAPI` / `CommandAPI`：只读 handler 通过 `ReadAPI` 查询
request log、安全审计日志（seclog 投影）、tokens、stats、Fusion、pins、pricing 及 detached
dashboard/config/provider 快照；写操作和主动网络探测通过 `CommandAPI` 执行
reset、quota refresh、health reset + persist、pin、reload 与 account probe。应用层的
`internal/admin`（`admin.Service`）是两个端口的唯一应用适配，负责把组合根经
`internal/app/web_adapter.go` 注入的窄端口映射到 transport DTO/命令；`internal/app/web_adapter.go` 只负责
composition 与 mux 挂载。任何 transport handler 都不得绕过端口直接访问 Proxy。

账号测活必须在 admin capability 内只调用一次 runtime 快照端口（`ProbeRuntime`），从同一 generation
取得 config 和 provider implementation；凭据文件检查及上游网络 I/O 在快照完成、
锁已释放后执行。admin 读侧不提供按名字单独读取 runtime provider 的入口，
避免 reload 期间把旧 config 与新 impl 混用。

Web 生命周期也归 `internal/web`：task owner 是后台工作的唯一 admission gate，
session store 保存 detached、transport-visible login 更新。task owner 管理
login-session GC 与 AQP/Codex 轮询，关闭时在同一 mutex 内停止 admission、取消
root context 并等待任务。GC 只删除超过 TTL 的 done/error 会话，不能删除仍
pending 的会话；否则 UI 会在后台任务仍可能落盘时提前得到 404。嵌入式资源位于
`internal/web/assets`，由该包的 asset owner 提供，无独立前端构建步骤。

## 会话成本汇总（`GET /api/sessions`）

`internal/observe/requestlog/sessions.go` 的 `SessionSummaries` 按最新 `scanLimit`（2000）条
记录聚合出每个 `session_id` 的时间跨度、请求数（含 shadow 单列）、错误数、providers/models、
token 总量（`ExtractUsage` 兼容 anthropic/chat/responses 三种响应体形状；流式响应按记录的
SSE 文本逐帧提取、按字段取最大值合并——usage 帧以累计计数重复出现；cache read/creation
单列，openai 形状的 `prompt_tokens`/`input_tokens` 内含 `cached_tokens`，提取时从 input 中
扣除以免与 cache 桶双计）与等价 USD 成本；查询走 `Filter.UsageOnly` 投影——用量在逐行读取时
解析、body 在 top-K 堆保留前剥离，2000 条扫描不会把全量 body（默认每侧至多 5MiB）钉在内存；
底层查询带文件级提前终止（见 `docs/architecture/fusion-shadow-cache.md`）：top-K 堆满后，
最新记录仍老于堆底的整个轮转文件直接跳过，端点成本不再随 retention 窗口内全部文件线性增长。
成本走与 `/api/analytics`、budget watcher 完全相同的 `pricing.Resolve`（config `prices:`
覆盖优先）+ `ComputeCost` 路径，未定价模型贡献 0。
web 层 `handleSessions` 暴露 `GET /api/sessions?limit=50`（上限 200，按最近活跃排序），
request_log 关闭时返回 `{enabled:false}`。`Filter.Session` 支持按 session id 精确过滤
`QueryRecords`。shadow 请求计入所在会话（真实上游开销）并单列计数。

## 请求访问日志（`internal/observe/requestlog`，JSONL 文件，默认关闭）

记录每个 commit 的 upstream 调用的 request+response body（各自受
`max_body_bytes` 限制）和元数据，逐行 JSON 写轮转文件（目录默认
`~/.model-proxy/log/requests`），供离线分析
（`jq`/`grep`）与查询 API。**默认 `enabled: false`**（零开销：不 wrap、
不开文件、不起 goroutine）；改 `enabled` 需**重启**（reload 不重建 logger）。

热路径：`internal/app/observe_adapters.go` 先把请求、route target 与 response header
快照映射为纯值 Input；`p.reqLog != nil` 时 `resp.Body`（**协议转换后**的字节）
包 `internal/transport/bodycapture.Reader`（有界 tee，`max_body_bytes` 封顶，
超限停捕获但字节仍透传）→ 非阻塞 enqueue 到 buffered chan（cap 2048，满则计数
降频 log，**绝不阻塞 forward**）。`request_id` 恒生成（启动 nonce + 原子计数器，
live 事件同用）。单 goroutine drain 写文件；写失败计数并降频 log；目录初始化失败
后 logger fail closed。持久化（队列/轮转/retention/权限）由共享 sink
`internal/observe/logfile` 承载，与 seclog 同一模式：活动文件按天命名
`requests-YYYYMMDD.log`，同日重启追加同一文件（不产生每启动一个文件），首次写入才懒建文件；
超 `max_file_size`（默认 1G）时归档为 `requests-YYYYMMDD--HHMMSS-<seq>.log`，自然日变更直接切到新天名（空文件不残留）。retention（默认 30d）按
mtime 删归档文件，**活跃文件永不删**（启动/每小时/关停三次 sweep 回收上次遗留
的活跃文件）。SIGINT/SIGTERM 排空 chan + 写完 + sweep + 关文件。新建或既有日志
目录/文件都收紧为 `0o700`/`0o600`（含用户 prompt）。只记 commit 响应
（2xx + 非 failover 4xx）；failover 中间尝试与 all-failed 502 不记。影子记录带
`shadow:true` + `request_id: shadow-<原id>`（见
`docs/architecture/fusion-shadow-cache.md`）。记录的 `request_body` 保存原始
客户端请求体（replay 保真用；改写/转换前的）。响应 header 只保存
`content-type`、`x-request-id`、`retry-after`。配置入口为
`request_log.{enabled, dir, max_file_size, max_body_bytes, retention}`。

**查询 API/UI**：`GET /api/requests`（过滤
model/provider/status/errors/from/to/limit/`shadow=only|exclude`，
`QuerySummaries` 在 top-K 之前清除 request body、response body 和 response
headers）+ `GET /api/requests/<id>`（`QueryRecords`，完整 body）+ Web UI
Requests 标签页（过滤行 + 影子徽标 + 点击记录在其**正下方**插入详情行展开完整 body，再点收起；**可同时展开多行**，点击一行不会关闭其他行；过滤行顺序为 session → provider → model（session 下拉选项来自 `/api/sessions`，选中后表格上方显示该会话汇总——请求数/输入输出/缓存读写/平均延迟/错误/等价成本/provider/model——表格按 `session=` 过滤、limit 提到 500，且可与 provider/model/shadow/errors 叠加；汇总口径与 Status→Live 的 session 面板一致，来源 `/api/sessions` 聚合）；provider → model 两者**联动**且选项**来自日志数据而非 config.yaml**：每次 `/api/requests` 响应带 `facets`（扫描窗口内实际出现的 distinct provider/model + provider→models 映射），provider 下拉用 `facets.providers`、model 下拉用 `linkedModels(provider, facets.provider_models)` 收窄，不再适用的已选 model 被清空；两个下拉都是**自绘可搜索**（原生 `<datalist>` 弹层由浏览器定位、无法对齐也无法主题化；后端仍是大小写不敏感 substring 匹配，自由输入仍有效）。每个过滤控件（provider/model 下拉、shadow 选择、errors only 复选框、Refresh）都在 change/选中时立即生效，无需额外点 Refresh。详情里 request JSON body 格式化（≤8MB）并在 ≤256KB 时语法高亮（大 body 只格式化不加 span，避免 DOM 爆炸；超大 body 按 64KB **整行**分块，首屏只入 DOM 一块，滚到容器底部自动追加下一块（无按钮）；追加时若该块全部是折叠行（渲染高度近 0）会继续追加，直到容器重新可滚动或块用尽——否则滚动条已经到底，用户永远触发不了剩下的块；body 容器高度为 `min(70vh, 760px)` 并强制显示细滚动条（共享的 340px 上限 + macOS overlay 滚动条让多 MB body 看起来像被截断），且**超长单行**（>4000 字符，典型是巨型 JSON 字符串值/粘贴的文件；普通 1–3KB thinking/text 段不再折叠）折叠成可展开块（preview + 字符数，展开后换行内联到外层容器，不再有嵌套 300px 滚动陷阱），不再需要横向拖一兆级的一行；response body 一律**按行展示**（原始行 + 行号 gutter，不展开格式化——流式响应按事件卡片/缩进渲染会过高），超限退化为 raw `<pre>`；无日志记录的请求（unrouted model 的 502/400 终局不 commit，因此没有 request-log 记录）点击展开显示**中性提示**而非红色错误，且只请求一次 `/api/requests/<id>` 不再循环）。
