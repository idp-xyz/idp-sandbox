# idp-sandbox 运维手册

适用：idp-sandbox（Agent 执行沙箱，MCP 上游，放在 idp-mcp 网关后面）。设计与取舍见 [`adr/ADR-0001-sandbox-design.md`](adr/ADR-0001-sandbox-design.md)。

## 1. 它是什么，怎么接

- 一个 MCP 2026-07-28 服务器，端口 8570（`/mcp`），运维端口 9570（`/healthz`、`/readyz`、`/metrics`）。
- **只能放在 idp-mcp 网关后面**：服务器自己没有认证，只有网关能到达它的私网才安全。网关负责认证、授权、参数校验和审计，并把调用者转发给它。
- 在 idp-mcp 里必须以 `identity: forward` 注册（`POST /api/v1/servers`，`"identity":"forward"`，注册后不可改）；否则每个请求都被拒绝（`the request carries no caller identity`）。
- 工具：`sandbox_create`、`sandbox_run`（`python` / `shell`）、`sandbox_write_file`、`sandbox_read_file`、`sandbox_list_files`、`sandbox_delete`。经网关时带目标名前缀（`sandbox__sandbox_run`）。
- 工具的 schema 变了（例如启用出口、改限额）→ 网关钉住的目录会漂移，要对服务器重新 `discover`、评审、重新发布 Release。

## 2. 两种后端

| | Docker（默认） | Kubernetes（`IDP_SANDBOX_BACKEND=kubernetes`） |
|---|---|---|
| 隔离 | 每次运行一个加固的容器 | 每次运行一个 Job 的 Pod |
| 工作区 | 内存型 tmpfs 卷（内核执行大小），由"锚"容器持有 | PVC（容量由存储驱动决定）|
| 需要 | Docker 套接字（等于宿主 root，只用于开发或专用节点） | 服务账号 + 运行命名空间里的 Role（Job、ConfigMap、PVC、Pod、Pod 日志）|
| 出口代理 | 支持（`network: egress`） | 未做 |
| 更强的运行时 | `IDP_SANDBOX_RUNTIME=runsc`（gVisor）或 `kata` | `IDP_SANDBOX_RUNTIME_CLASS=gvisor` 或 `kata` |
| 部署 | 单实例 | 单副本，策略 `Recreate`（启动清扫会删同一 owner 的全部对象）|

## 3. 配置（环境变量）

| 变量 | 含义 | 默认 |
|---|---|---|
| `IDP_SANDBOX_LISTEN` | `/mcp` 的地址；非回环必须同时设 `IDP_SANDBOX_PRIVATE_NETWORK=true` | `127.0.0.1:8570` |
| `IDP_SANDBOX_METRICS` | 运维端口 | `127.0.0.1:9570` |
| `IDP_SANDBOX_REQUIRE_CALLER` | 要求网关转发的调用者；`false` 只用于不经网关的开发运行 | `true` |
| `IDP_SANDBOX_MAX_PER_CALLER` | 每个调用者的工作区数 | 8 |
| `IDP_SANDBOX_MAX_WORKSPACES` / `_MAX_CONCURRENT` | 总工作区数 / 同时运行数 | 64 / 8 |
| `IDP_SANDBOX_MEMORY_MB` `_CPUS` `_PIDS` `_TIMEOUT_S` `_OUTPUT_KB` `_WORKSPACE_MB` | 每次运行的限额 | 256 / 1 / 64 / 20 / 64 / 64 |
| `IDP_SANDBOX_IDLE_MIN` | 工作区空闲多久回收 | 30（存活上限 4 小时）|
| `IDP_SANDBOX_PYTHON_IMAGE` / `_SHELL_IMAGE` | 运行镜像（运维选，调用者不能选；由运维拉取，服务不拉）| `python:3.12-slim` / `alpine:3.22` |
| `IDP_SANDBOX_RUNTIME` | Docker：OCI 运行时（`runsc`、`kata`）| 守护进程默认 |
| `IDP_SANDBOX_RUNTIME_CLASS` | Kubernetes：RuntimeClass | 无 |
| `IDP_SANDBOX_EGRESS_PROXY` / `_EGRESS_PORT` | 出口代理容器名与端口；设了才有 `network: egress` | 关 |
| `IDP_SANDBOX_NAMESPACE` / `_STORAGE_CLASS` / `_OWNER` | Kubernetes 后端的命名空间、存储类、清扫标签 | Pod 自己的命名空间 / 集群默认 / `default` |

出口代理（`idp-sandbox-egress`）：`IDP_EGRESS_HOSTS`（精确名或 `*.后缀`，空 = 全部拒绝）、`IDP_EGRESS_PORTS`（默认 443、80）。它自己解析并拒绝回环、私网、链路本地、云元数据，不要开 `IDP_EGRESS_ALLOW_PRIVATE`（仅测试）。白名单里不要放允许上传的主机。

## 4. 部署与检查

```bash
make test                       # gofmt、vet、单元测试（-race）
make test-integration           # 真实 Docker：20 个隔离测试（需要 python 和 alpine 镜像：make images）
bash deploy/e2e.sh              # 经真实的 idp-mcp 网关（需要并排的 idp-mcp 和它的夹具镜像）
bash tests/integration/gvisor.sh   # Docker 后端，全部运行在 gVisor 下（docker-in-docker，不需要 root）
```

Kubernetes：`kubectl apply -k deploy/kubernetes`（先把镜像摘要、API 服务器的 `ipBlock` 换成真实值）。集群必须有**执行 NetworkPolicy 的 CNI**，否则"无网络"不成立（kind 默认的 kindnet 不行，用 Calico）。验证：`bash tests/kind/smoke.sh`；换运行时后先 `kubectl -n idp-sandbox port-forward svc/idp-sandbox 8570:8570 &`，再 `EXPECT_GVISOR=1`（或 `EXPECT_KATA=1`）`python3 tests/kind/check.py`（它自己用 kubectl 查集群 IP）。检查中途崩了会留下工作区，30 分钟后回收，或 `kubectl -n idp-sandbox-runs delete pvc --all`。

上线前核对：`readyz` 为 200；`/metrics` 有 `idp_sandbox_calls_total`；网关里该目标状态 OK；网络策略两端都放行 8570（网关 → 沙箱）。

## 5. 日常运维

- **看日志**：JSON 行。每次调用一行 `tool`，含工具名、结果、耗时、工作区 id 的哈希前缀、调用者哈希、代码的 SHA-256 前缀；**不含代码与文件内容**。出口代理每次放行与拒绝一行（`egress`）。
- **指标**：`idp_sandbox_calls_total{tool,outcome}`、`idp_sandbox_call_seconds_total{tool}`、`idp_sandbox_workspaces`。
- **清理**：工作区空闲 30 分钟回收；进程启动时按标签清扫上一个进程遗留的容器 / 网络 / 卷（Docker）或 Job / ConfigMap / PVC（Kubernetes）。手动：`docker ps -a --filter label=idp.sandbox`，`kubectl -n idp-sandbox-runs get jobs,pods,configmaps,pvc`。
- **滚动重启 = 丢掉所有工作区**（它们本来就在内存或随进程清扫）。通知使用者。
- **换运行时**：改环境变量并重启；对每个运行时跑一遍对应的检查（上面第 4 节）。

## 6. 故障排查

| 现象 | 原因 | 处理 |
|---|---|---|
| 每个请求都是 `the request carries no caller identity` | 服务器没有以 `identity: forward` 注册，或绕过网关直连 | 重新注册（注册后不可改，要新建）；开发直连时设 `IDP_SANDBOX_REQUIRE_CALLER=false` |
| 工具列表变了 / 网关说目录漂移 | 改了 schema（出口、限额）| 对服务器重新 discover、评审、发布 Release |
| `workspace not found or expired` | id 不对、属于别的调用者、已过期或沙箱重启过 | 重新 `sandbox_create`（别的调用者的 id 与不存在的 id 回答一样，这是有意的）|
| `the sandbox is at capacity` | 并发或总数上限 | 稍后重试；调大 `_MAX_CONCURRENT` / `_MAX_WORKSPACES` |
| `you already hold the maximum number of workspaces` | 每个调用者的上限 | `sandbox_delete` 一个 |
| `network access is not enabled` | 没配出口代理 | 配代理并重新发布 Release（schema 会变）|
| 运行总是 `timed out` | 代码慢，或在 gVisor 下系统调用密集 | 调大 `_TIMEOUT_S`（上限）；评估运行时的开销 |
| 运行是 `killed: out of memory` | 内存上限 | 调大 `_MEMORY_MB`；Kata 下写满 `/tmp` 也表现为 OOM |
| `docker: HTTP 404: No such image` | 运行镜像不在 Docker 守护进程 / 节点上 | 运维拉取或加载（服务绝不拉镜像）|
| Pod 一直 `ContainerCreating`（Kubernetes）| 运行时问题 | gVisor：用 `kvm` 或 `ptrace` 平台，节点 inotify 上限调到 8192；Kata：Go 运行时 + Cloud Hypervisor，别给 RuntimeClass 加 overhead |
| gVisor 下内存炸弹只被超时停下，运行的内存 / CPU 上限不生效（Kubernetes）| 节点用 systemd cgroup 驱动而 runsc 没开 `systemd-cgroup`：沙箱跑在 Pod 的 cgroup 之外；节点有 swap 时还有一层：kubelet 只给容器关 swap，而整个沙箱在 pause 容器的 cgroup 里 | `runsc.toml` 的 `[runsc_config]` 加 `systemd-cgroup = "true"`；节点有 swap 就给 Pod 关掉（`kubelet-kubepods.slice` 的 `MemorySwapMax=0`）；kind 上 `tests/kind/gvisor.sh` 都做了 |
| 网关到沙箱超时 | 网络策略没放行 8570 | 网关与注册服务的出口策略加 8570；沙箱命名空间要带 `idp-mcp/mcp-upstream: "true"` 标签 |
| 运行 Pod 能联网（Kubernetes）| CNI 不执行 NetworkPolicy | 换 Calico / Cilium；用 `tests/kind/check.py` 的 `no-network` 项验证 |
| gVisor 下 `--pids-limit 16` 起不来 | gVisor 把自己的宿主线程算进上限 | 用 64（默认）|
| 出口请求在 gVisor 下全失败 | Docker 内置 DNS 在 gVisor 网络栈里不生效 | 已按 IP 给代理地址；确认用的是新版本 |

## 7. 安全边界（运维要知道的）

- 默认运行时（runc）与宿主共享内核：对真正不可信的代码，上 gVisor 或 Kata，并保留全部其他层（无网络、限额、非 root、无令牌、按调用者隔离）。
- 身份这一跳没有签名：只有网关能到达沙箱的网络策略就是边界，不要把 8570 暴露给别人。
- Docker 后端要 Docker 套接字：专用节点、虚拟机或 rootless 引擎，不要和别的服务共用。
- 出口白名单对所有调用者一样，按主机名；能上传的主机会成为外传通道。
- 加固运行时的实测数据和限制见 ADR 第 5 节。
