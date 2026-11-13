# ADR-0001　IDP Sandbox 的设计（Agent 执行沙箱）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-06；决策人选择新建独立仓库 `idp-xyz/idp-sandbox`，方案由实现者推荐） |
| 范围 | `idp-sandbox`：让 Agent 在隔离环境里运行自己写的代码，并保存结果文件 |
| 依据 | idp-host ADR-0001 D1（HOST 不内建工具，工具都经 MCP 网关）与 D31（不使用厂商自带的 code interpreter，因为它绕过逐个工具调用的门控）；idp-mcp ADR-0001（上游由 Release 钉住、授权与审计在网关）；MCP 2026-07-28 |

## 1. 背景与边界

此前平台里没有"Agent 执行代码"的能力：HOST 的 harness 只能经网关调用已登记的工具。让模型写代码并运行，是一类常见的需求（数据处理、计算、格式转换），但也是风险最大的一类：模型的输出就是不可信代码，可能被提示词注入操纵。

这个组件只做一件事：给不可信代码一个**没有网络、资源有限、用完即弃**的运行环境。它不做：长期运行的服务、持久数据、包安装、GPU、交互式终端。

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **做成 MCP 上游服务，放在 idp-mcp 网关后面，不内建进 HOST** | HOST 的 D1 / D31 已定：工具都经网关。这样沙箱免费得到网关的授权（哪个角色能用哪些工具）、按钉住的 `inputSchema` 做参数校验、调用审计、漂移封锁和四眼发布；HOST、Workspace 一行都不用改 |
| D2 | **每次运行一个全新的容器**：无网络、只读根、用户 65534、丢弃全部 capabilities、`no-new-privileges`、seccomp、私有 IPC、`init`；内存（含交换）、CPU、进程数、`nofile`、`fsize`、墙钟时间、输出大小都有上限 | 没有状态可以泄漏到下一次运行；每一项对应一类已知的滥用（外联与渗出、提权、fork 炸弹、内存与磁盘耗尽、死循环、输出洪水） |
| D3 | **隔离后端可替换**：默认 Docker（runc，加固）；`IDP_SANDBOX_RUNTIME=runsc` 或 `kata` 用 gVisor / Kata；`Backend` 是接口，Kubernetes Job 后端见 D12 | runc 与宿主共享内核，对真正不可信的代码不够；换运行时应当只是配置 |
| D4 | **工作区 = 有大小上限的 tmpfs 卷**（内核执行配额），由一个很小的"锚"容器（`sleep`，4 个进程、16 MiB）持有 | 配额不是应用层的估算，满了就是 ENOSPC；文件只在内存里，工作区一删就没有了。Docker 的 local 卷驱动在没有容器使用时会卸载 tmpfs 并丢掉内容，所以需要锚（实现时踩到的，见 §4） |
| D5 | **文件读写走容器的标准输入输出，不走归档 API** | tmpfs 卷只在挂载它的容器的挂载命名空间里可见，`docker cp` 的归档 API 写进去的东西运行时看不到（实测）。读取用 `[ -f ] && [ ! -L ]` 与 `readlink -f` 拒绝符号链接与越界 |
| D6 | **调用方只能缩短，不能放宽**：镜像、语言、资源上限都是运维配置；调用方能选的是语言（枚举）、代码、超时（不超过上限）、路径（相对、无 `..`、不进 `.idp/`） | 沙箱的价值在于调用方（可能被注入的模型）改不了约束 |
| D7 | **工作区属于调用者**（D11）；id 仍是 128 位随机数；一次只允许一个运行；有总数、每个调用者与并发数的上限；空闲 30 分钟与存活 4 小时后回收；启动时按标签清扫上一个进程遗留的容器与卷 | M1 时网关不转发身份，id 只能是承载令牌；M2 起有了身份，id 是第二道防线 |
| D11 | **调用者身份来自网关（M2，idp-mcp ADR-0001 D40–D43）**：沙箱在 idp-mcp 里以 `identity: forward` 注册；网关把经过认证的 tenant、subject、roles 放进每个请求的 `_meta["io.idp.telos/caller"]`，并总是先删除客户端自带的同名键。沙箱把工作区绑到 `tenant/subject`：别的调用者（包括另一租户的同名用户）对它的任何操作得到的都是"不存在"，与一个从未存在的 id 不可区分；每个调用者最多 8 个工作区；请求不带身份就拒绝（`IDP_SANDBOX_REQUIRE_CALLER`，默认开） | 多租户、多用户共用一个沙箱实例不再依赖 id 的保密；按调用者限额才有意义；审计里有"谁"（subject 的哈希前缀） |
| D8 | **自己没有认证，只接受网关**：默认只监听回环；监听非回环地址必须设置 `IDP_SANDBOX_PRIVATE_NETWORK=true`（运维声明只有网关能到达） | 与 idp-mcp 的"上游在私网"一致；宁可启动失败，也不要被一个 `0.0.0.0` 的默认值暴露一个能执行代码的端口 |
| D9 | **审计只记元数据**：工具名、结果、耗时、工作区 id 的哈希前缀、代码的 SHA-256 前缀；不记代码与文件内容。内部错误（容器引擎的报错、路径）不返回给模型，只写日志 | 代码里可能有敏感数据；引擎错误会泄露部署细节 |
| D10 | **依赖：只用标准库**（Engine API 走 unix 套接字上的 net/http，附着标准输入用 HTTP 升级的原始连接）；MCP 的准入规则从 idp-mcp 复制，不共享代码 | 与 HOST 的 D13 相同 |
| D12 | **Kubernetes 后端（M3）**：同一个 `Backend` 接口，只用 Kubernetes REST API（net/http）。工作区是 PVC；每次运行是一个 Job（`backoffLimit 0`、`activeDeadlineSeconds`、Pod 的加固与 D2 一一对应：非 root、只读根、丢弃 capabilities、seccomp、不挂令牌、requests = limits）；没有 exec，所以代码与文件放进 ConfigMap 挂载，结果从 Pod 日志读，stderr 靠包装命令与随机标记分开；无网络由运行命名空间里的 NetworkPolicy 保证，服务与运行分在两个命名空间，服务账号在运行命名空间只有最小的 Role，另有 ResourceQuota 与 LimitRange | 套接字等于宿主 root（§5）；Job 后端让沙箱不再需要容器引擎，并能用 RuntimeClass 换 gVisor / Kata。没有 exec / attach，是为了 RBAC 里不授予它们：服务账号被攻破时也不能进入别人的运行 |
| D13 | **经白名单的出口代理（M3）**：运行默认没有网络；运维配置了出口代理（`idp-sandbox-egress`，`IDP_SANDBOX_EGRESS_PROXY`）时，`sandbox_run` 多一个 `network` 参数，取 `none`（默认）或 `egress`。`egress` 的运行得到自己的、`Internal` 的网络（没有网关、没有路由出去），网络上只接着代理（用完即断开并删除），并设置 `HTTP_PROXY` / `HTTPS_PROXY`。代理按**主机名**白名单（精确名或 `*.后缀`，空表示全部拒绝）和端口白名单放行 CONNECT 与明文 HTTP 转发，并**自己解析、自己检查地址**：回环、私网、链路本地（含 169.254.169.254）、CGNAT、多播、未指定地址一律拒绝，所以白名单里的名字指向内网、或 DNS 重绑定，都进不去；有并发数、隧道时长和字节数上限，每次放行与拒绝都记日志 | 不能联网就不能装包、调 API，这是真实需求；放开必须是运维的决定、有边界、可审计。每次运行一个网络，是因为共享网络里并发运行（可能属于不同调用者）能互相连接。`network` 参数让工具的 schema 变化，网关钉住 schema，所以启用出口要重新发现并评审服务器（这是想要的）；没配置代理时 schema 里 `network` 只有 `none` |
| D14 | **office 运行档位（平台 ADR-0002 阶段 0，2026-11-13）**：运维用 `deploy/office/build.sh` 构建镜像 `idp-sandbox-office:<版本>`，即 python 运行镜像加上 [OfficeCLI](https://github.com/iOfficeAI/OfficeCLI)（Apache-2.0）的 linux-x64 单文件二进制。版本与 SHA-256 钉在脚本里（现为 1.0.159，2026-10-27 发布，与 release 的 SHA256SUMS 和 GitHub API 摘要都对过）；构建用 `--network none`，构建后在同样的限制下冒烟一次。设了 `IDP_SANDBOX_OFFICE_IMAGE` 才出现 `language: office`：在这个镜像里以 POSIX shell 运行脚本，`officecli` 在 PATH 上，隔离与限额和其他运行完全相同，调用者仍不能选镜像（D6）。`sandbox_run` 的 `language` 参数带上每种语言的说明，模型能据此选择。镜像的环境变量与原因：`OFFICECLI_SKIP_UPDATE=1`、`OFFICECLI_NO_AUTO_INSTALL=1`（运行没有网络）；`OFFICECLI_NO_AUTO_RESIDENT=1`（默认 `create` 会在后台起常驻进程缓存文档，运行一结束它就随容器消失，未写回的修改会丢，还占进程配额）与 `OFFICECLI_RESIDENT_FLUSH=each`（Agent 仍会显式 `officecli open`，它自带的参考资料推荐这样做，真实模型实测就这么做了：每次修改立即写回，常驻进程随运行消失也不丢东西）；`DOTNET_EnableDiagnostics=0`（不在 `/tmp` 建调试管道与诊断套接字）；`DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1`（镜像没有 ICU，不设则 .NET 启动即退出；只影响按地区的格式与排序，不影响中文等文字本身）；`DOTNET_EnableWriteXorExecute=0`（.NET 的 W^X 双重映射用 `ftruncate` 给一个内存文件定大小，而运行的文件大小上限 `fsize` = 工作区配额同样限制它：8 MiB 时实测 `PAL_SEHException` 中止，64 MiB 时通过；关掉它，档位就不依赖工作区配额的大小。代价是 JIT 代码页不再写执行分离，进程本身仍在无网络、只读根、无能力、seccomp 的隔离里） | Agent 要读、改、生成 Word / Excel / PowerPoint 文件；OfficeCLI 不需要 Office，单文件、离线可用。它的 MCP 模式是 stdio 服务器（`officecli help` 所述），不适合直接登记为网关上游（平台 ADR-0002 第 1 节），所以以命令行的方式放进沙箱，沿用沙箱的全部隔离 |

## 3. 威胁与对应

| 威胁 | 对应 | 验证 |
|---|---|---|
| 代码联网外传数据、扫内网、访问容器引擎 | 无网络，只有回环（D2） | `TestNoNetwork`：外网、内网、本机端口、DNS 都不通，网卡只有 lo |
| 提权、写系统文件、读宿主凭据 | 非 root、无 capabilities、只读根、无套接字、环境不带凭据 | `TestRunsUnprivileged…`：uid 65534、CapEff 全 0、NoNewPrivs、seccomp 过滤、写 /etc 失败 |
| fork 炸弹 | 进程数上限 | `TestForkBombIsContained`，之后宿主和下一次运行正常 |
| 吃光内存 | 内存与交换上限，OOM 时标记 | `TestMemoryBombIsKilled` |
| 死循环 / 长时间占用 | 墙钟超时并杀死容器；调用方只能缩短 | `TestInfiniteLoopTimesOut` |
| 输出洪水 | 超过上限即终止并标记截断 | `TestOutputFloodIsCappedAndStopped` |
| 写满磁盘 | tmpfs 大小上限 | `TestDiskFillStopsAtTheQuota` |
| 读别人的工作区 | 每个工作区独立的卷；绑定到调用者（D11）；id 不可猜 | `TestWorkspacesAreIsolatedFromEachOther`、`TestAWorkspaceBelongsToItsCaller`；端到端 `other-caller-cannot-read` / `-run`、`forged-caller-…` |
| 经符号链接读镜像里的文件或越出工作区 | 读取拒绝符号链接并检查 `readlink -f` | `TestSymlinkInWorkspaceIsNotFollowed` |
| 提示词注入诱使调用方放宽约束（换镜像、改资源） | 不在输入里；网关按钉住的 schema 拒绝多余字段 | `tools_test`；端到端 `gateway-rejects-extra-field` |
| 越权调用 | 网关按角色授权（guest 只看得到只读工具） | 端到端 `guest-cannot-run`、`operator-has-no-delete` |

## 4. 实现中发现的

- **归档 API 写不进 tmpfs 卷**：先用 `docker cp` 写文件，运行时文件不在。卷在没有容器挂载时被卸载，即使有运行中的容器，`cp` 也写到了卷驱动之外。改为经标准输入写入（D5），并让一个锚容器持有卷（D4）。
- **经网关的上游需要进注册服务的主机白名单**（`IDP_ALLOWED_UPSTREAM_HOSTS`、`IDP_PRIVATE_UPSTREAM_HOSTS`）：沙箱是私网主机，开发拓扑的覆盖文件里已加。
- 日志驱动不能设为 `none`（读不到输出）；用 `json-file` 并限制为 1 MiB。

## 5. 限制（没有做的，以及为什么）

- **runc 共享宿主内核**：加固挡不住内核漏洞。生产应使用 gVisor 或 Kata；本机没有 `runsc`，所以 `IDP_SANDBOX_RUNTIME` 的路径只有代码、没有实测。
- **服务需要容器引擎套接字，那等于宿主的 root**。开发拓扑挂载 `/var/run/docker.sock`；生产应在专用节点、虚拟机或 rootless 引擎上运行，或加套接字代理限制 API；或用 Kubernetes 后端（D12），那里服务账号的权限就是一个 Role。
- **身份这一跳没有认证**：网关转发的身份没有签名，沙箱靠"只有网关能到达它的私网"相信网关（idp-mcp D43）。网络策略因此是边界；给身份签名需要网关与上游之间的密钥分发，是另一个决策。
- **出口只有 Docker 后端有**（D13）：Kubernetes 后端的运行仍然完全没有网络；等价的做法（白名单代理 Pod 加只放行它的 NetworkPolicy）未做。出口代理按主机名放行，不按调用者区分：白名单对所有调用者一样。代理能看到并记录目标主机，看不到 HTTPS 的内容；放行的主机若允许上传，就能被用来外传工作区里的数据，所以白名单应当只列只读来源。
- **加固运行时的实测情况（2026-11-06）**：
  - **gVisor（runsc 20260126.0，`kvm` 平台）**：Kubernetes 后端在 kind 上 22 项集群检查全过，整平台 PATH D 也过——但那次的内存炸弹是靠超时过的：kind 用 systemd cgroup 驱动，runsc 没开 `systemd-cgroup` 时把 Pod 的 cgroup 路径当成目录名，沙箱跑在 Pod 的 cgroup 之外，运行的内存与 CPU 上限都没生效（256 MiB 的 Pod 分配 2 GiB 不被杀）；开了之后沙箱回到 Pod 的 cgroup，但整个沙箱在 pause 容器的 cgroup 里，kubelet 只给容器关 swap，节点有 swap 时炸弹会先换进 swap（5 次里 2 次拖了 10–17 s 才被杀），所以 `tests/kind/gvisor.sh` 同时给所有 Pod 关了 swap，之后内存炸弹 4.5 s 被 OOM 杀掉，`check.py` 也改成超时不算通过；Docker 后端 20 个隔离测试全在 runsc 下，全过（`tests/integration/gvisor.sh`，docker-in-docker，容器用宿主 cgroup 命名空间时 runsc 能应用 cgroup：内存炸弹被限额杀掉，fork 炸弹耗尽进程数上限、整个沙箱被杀；`CGROUPS=0` 退回 `--ignore-cgroups` 时上限不生效、靠超时挡住）。默认的 `systrap` 平台在这台 WSL2 上起不来（最新 20261102.0：`creating process: out of memory`；20260126.0：`failed to create a syscall thread`），`kvm` 与 `ptrace` 可用，根因没有查；这个环境里所有进程（含节点的 PID 1）都带一个 seccomp 过滤器，可能相关。gVisor 的 `--pids-limit` 数的是沙箱自己的宿主线程，16 起不来，64 可以。与 runc 的差别：`/proc/self/status` 没有 `NoNewPrivs` 与 seccomp 行，写只读根得到 EACCES，`--network none` 的沙箱连回环网卡都没有（更严），Docker 内置 DNS 在其网络栈里不生效（出口代理的地址因此按 IP 给）。
  - **Kata（4.3.1，Go 运行时 + Cloud Hypervisor）**：Kubernetes 后端 22 项全过（`tests/kind/kata.sh`），客户机内核 6.18.56；内存型 `/tmp` 在客户机里没有大小限制，写满撞 Pod 内存上限被 OOM 杀，而不是 ENOSPC。QEMU 在这台机器（WSL2 嵌套 KVM）上客户机一启动就崩；Rust 运行时需要节点里没有的 D-Bus。
  - **性能（Python 3.12，同机）**：docker 上 runc 对 runsc：CPU 密集相同；4000 个小文件读写 0.07 s 对 0.15 s；10 万次 `stat` 0.06 s 对 0.55 s；300 MB 内存触碰 0.12 s 对 0.49 s；容器启动 340 ms 对 640 ms。Kubernetes 上 runc / gVisor / Kata：Pod 从创建到完成约 3.7 s / 5–8 s / 8 s；4000 个小文件 0.18 / 0.17 / 2.5 s（Kata 走 virtiofs）；`stat` 0.06 / 0.56 / 0.05 s；300 MB 内存触碰 0.12 / 0.5 / 0.55 s。
  - **没有验证的**：PVC 容量是否被存储驱动执行（kind 的 local-path 不执行；云块存储的卷大小本身就是上限）、镜像摘要锁定与私有仓库拉取、生产 CNI 的行为、真实（非嵌套）云节点上的 gVisor / Kata。**部署的集群必须有执行 NetworkPolicy 的 CNI**（kind 默认的 kindnet 在 CI 里没有拦住运行 Pod 的出站，验证用 Calico），否则"无网络"不成立。`Pids` 无法按 Pod 设置（交给 kubelet 的 `podPidsLimit` 或 gVisor）；stderr 在超时被杀时丢失；每次文件操作启动一个 Pod（秒级）。
- **单实例、内存里的工作区**：重启即清空；没有跨副本的会话。
- **没有按调用者的配额与计费**：只有全局的并发与工作区总数上限。
- 只有 Python 与 Shell 两种语言；镜像里没有额外的库（要数据分析库就得由运维构建带库的镜像并配置）。

## 6. 验证

- Kubernetes 后端（D12）：`internal/kube` 的单元测试（假 API 服务器：成功与 stdout / stderr 分离、非零退出、DeadlineExceeded、本地超时、OOMKilled、输出截断、工作区不存在、取消与出错时的清理、Job 安全字段逐项断言、ConfigMap 写文件的形状、符号链接的退出码映射、清扫只动本 owner、TLS 与 CA）；`tests/deploy` 检查清单（命名空间、Role 恰好是后端用到的权限、运行命名空间全拒网络、配额、加固）；**真实集群**：CI 的 `kubernetes` 任务在 kind（Calico）上应用清单并经端口转发跑 `tests/kind/check.py`（21 项：工具、运行 Pod 的安全字段由集群自己报告、外网 / API 服务器 / 沙箱服务连不上、无服务账号令牌、内存炸弹被杀、死循环超时、输出上限、`/tmp` 配额、别的调用者得到"不存在"、无身份被拒、删除后命名空间里什么都不剩）。
- 单元（`make test`，`-race`）：MCP 准入规则（版本、批处理、遗留方法、头部不一致、`_meta`）、工具的参数严格性、错误不泄露内部细节、每个工具有网关能钉住的 schema。
- 真实容器（`make test-integration`，20 个用例（含出口的 4 个：只到白名单、不能绕过代理、并发运行互相看不见、未配置时被拒；含内核断言：gVisor 下代码看到 4.4.0））：上表的每一项，加运行间文件保留、一次一个运行与容量上限、删除与回收、非法请求。
- 经网关（`bash deploy/e2e.sh`）：真实的 idp-mcp 注册服务与网关、PostgreSQL；注册、发现（6 个工具）、评审、授权（operator 与 guest 两个角色）、API Key、发布、部署；再经网关创建工作区、写文件、运行 Python 读取并计算、读回结果；恶意代码被挡住；网关拒绝非法参数；越权被拒；调用记录进了注册服务且不含代码原文。
