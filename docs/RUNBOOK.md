# idp-sandbox 运维手册

适用：idp-sandbox（Agent 执行沙箱，MCP 上游，放在 idp-mcp 网关后面）。设计与取舍见 [`adr/ADR-0001-sandbox-design.md`](adr/ADR-0001-sandbox-design.md)。

## 1. 它是什么，怎么接

- 一个 MCP 2026-07-28 服务器，端口 8570（`/mcp`），运维端口 9570（`/healthz`、`/readyz`、`/metrics`）。
- **只能放在 idp-mcp 网关后面**：服务器自己没有认证，只有网关能到达它的私网才安全。网关负责认证、授权、参数校验和审计，并把调用者转发给它。
- 在 idp-mcp 里必须以 `identity: forward` 注册（`POST /api/v1/servers`，`"identity":"forward"`，注册后不可改）；否则每个请求都被拒绝（`the request carries no caller identity`）。
- 工具：`sandbox_create`、`sandbox_run`（`python` / `shell`）、`sandbox_write_file`、`sandbox_read_file`、`sandbox_list_files`、`sandbox_delete`。经网关时带目标名前缀（`sandbox__sandbox_run`）。
- 工具的 schema 变了（例如启用出口、改限额，或升级到 `language` 的说明有变化的版本：ADR-0001 D16、D17 在 `office` 的说明里加了 `sheetskel`、`pdftext`、`md2pdf`；D23 曾把说明里的 `sheetskel` 换成 `sheetmap`、`sheetcheck`，2026-11-20 又改回 `sheetskel`，与改回的 skill 一致：已经带着那一版说明（提交 da86df8）的部署，要对沙箱服务器再 `discover`、评审、重新发布 Release，网关才会回到 `sheetskel`；没带过那一版的不受影响）→ 网关钉住的目录会漂移，要对服务器重新 `discover`、评审、重新发布 Release。只换 office 运行镜像不改 schema，不需要这一步。
- 启用出口还会把 `sandbox_run` 的注解 `openWorldHint` 从 `false` 改成 `true`（平台 ADR-0003 D7：能连外面就如实说能连外面）。注解也在钉住的目录里：idp-mcp 的 `federation.NormalizeTools` 保留工具的每个字段（含 `annotations`），Release 的快照摘要和网关的漂移检查（`CheckDrift`，默认每分钟一次）都用同一份规范化后的目录算摘要，`TestDigestChangesWhenMeaningChanges` 就覆盖了注解变化会改摘要。所以只改注解与改 schema 一样：网关会报 `DRIFTED`，`driftAction` 为默认的 `block` 时对该服务器的所有调用被拒绝。运维要做的：先对服务器 `POST /api/v1/servers/{id}/discover` 重新发现，评审新旧快照的差别（这里应当只有 `network` 枚举与 `openWorldHint`），再发布新的 Release；在 Release 生效前这个服务器处于封锁状态，服务一滚动、Release 还没换，两边就不一致，所以启用出口要和重新发现、发布 Release 排在同一个窗口里（`driftAction: report` 的网关只报告不封锁）。反过来关闭出口也一样。

## 2. 两种后端

| | Docker（默认） | Kubernetes（`IDP_SANDBOX_BACKEND=kubernetes`） |
|---|---|---|
| 隔离 | 每次运行一个加固的容器 | 每次运行一个 Job 的 Pod |
| 工作区 | 内存型 tmpfs 卷（内核执行大小），由"锚"容器持有 | PVC（容量由存储驱动决定）|
| 需要 | Docker 套接字（等于宿主 root，只用于开发或专用节点） | 服务账号 + 运行命名空间里的 Role（Job、ConfigMap、PVC、Pod、Pod 日志）|
| 出口代理 | 支持（`network: egress`）：每次运行一个内部网络，只接着代理 | 支持（D22）：`deploy/kubernetes-egress`，运行的 Pod 多一个标签，两条 NetworkPolicy 只放它到代理；不应用这一层就没有 |
| 成员文件进出 | 支持，经附着流 | 支持（D19），经 ConfigMap 与 Pod 日志，每步是一个 Job：kind 上 25 MiB 写入约 5 秒、读回约 16 秒（gVisor 6 / 21 秒，Kata 8 / 28 秒）|
| "无网络"靠什么 | 容器没有网卡（`--network none`） | 运行命名空间的 NetworkPolicy，要 CNI 执行；服务启动时自己探测一次（D20）|
| 更强的运行时 | `IDP_SANDBOX_RUNTIME=runsc`（gVisor）或 `kata` | `IDP_SANDBOX_RUNTIME_CLASS=gvisor` 或 `kata` |
| 部署 | 单实例 | 单副本，策略 `Recreate`（启动清扫会删同一 owner 的全部对象）|

## 3. 配置（环境变量）

| 变量 | 含义 | 默认 |
|---|---|---|
| `IDP_SANDBOX_LISTEN` | `/mcp` 的地址；非回环必须同时设 `IDP_SANDBOX_PRIVATE_NETWORK=true` | `127.0.0.1:8570` |
| `IDP_SANDBOX_METRICS` | 运维端口 | `127.0.0.1:9570` |
| `IDP_SANDBOX_REQUIRE_CALLER` | 要求网关转发的调用者；`false` 只用于不经网关的开发运行 | `true` |
| `IDP_SANDBOX_CALLER_KEYS_FILE` / `_CALLER_AUDIENCE` | 网关的公钥（PEM，P-256，可以几把）与本服务在 idp-mcp 里登记的端点；设了之后只认网关签过名的调用者（ADR-0001 D18），两个要一起设 | 无（身份不验签，启动时告警）|
| `IDP_SANDBOX_MAX_PER_CALLER` | 每个调用者的工作区数 | 8 |
| `IDP_SANDBOX_MAX_WORKSPACES` / `_MAX_CONCURRENT` | 总工作区数 / 同时运行数 | 64 / 8 |
| `IDP_SANDBOX_MEMORY_MB` `_CPUS` `_PIDS` `_TIMEOUT_S` `_OUTPUT_KB` `_WORKSPACE_MB` | 每次运行的限额 | 256 / 1 / 64 / 20 / 64 / 64 |
| `IDP_SANDBOX_IDLE_MIN` | 工作区空闲多久回收 | 30（存活上限 4 小时）|
| `IDP_SANDBOX_PYTHON_IMAGE` / `_SHELL_IMAGE` | 运行镜像（运维选，调用者不能选；由运维拉取，服务不拉）| `python:3.12-slim` / `alpine:3.22` |
| `IDP_SANDBOX_OFFICE_IMAGE` | office 运行档位的镜像（ADR-0001 D14、D17）：`bash deploy/office/build.sh` 构建（OfficeCLI、读写 PDF 的五个 wheel 与两个字体文件各按 SHA-256 钉在脚本里，下载后校验，离网构建并冒烟；下载的文件留在 `IDP_SANDBOX_CACHE`，默认 `~/.cache/idp-sandbox`，目录填好之后构建不需要网络）；设了才有 `language: office` | 无（不提供 office）|
| `IDP_SANDBOX_FILES_URL` | Workspace 的内部文件端点（例 `http://workspace:8560/internal/v1/files`）；设了才有 `sandbox_import_file` / `sandbox_export_file`（ADR-0001 D15）。两个后端都支持：Docker 走附着流；Kubernetes 把文件切成 ConfigMap 写进去、分段以 base64 从 Pod 日志读回来（D19），服务还要能出站到这个端点（`deploy/kubernetes/networkpolicy.yaml` 里注释掉的那条）| 无（不搬文件）|
| `IDP_SANDBOX_NETWORK_CHECK` | Kubernetes 后端启动时的网络探测（D20）：`enforce` 发现与运行同标签的 Pod 能连到 API 服务器就拒绝启动；`warn` 只记日志；`off` 不查 | `enforce` |
| `IDP_SANDBOX_TRANSFER_MB` | 搬进搬出的单个文件上限，不得大于 `IDP_SANDBOX_WORKSPACE_MB` | 25 |
| `IDP_SANDBOX_RUNTIME` | Docker：OCI 运行时（`runsc`、`kata`）| 守护进程默认 |
| `IDP_SANDBOX_RUNTIME_CLASS` | Kubernetes：RuntimeClass | 无 |
| `IDP_SANDBOX_EGRESS_PROXY` / `_EGRESS_PORT` | 出口代理与端口；设了才有 `network: egress`。Docker 后端是代理的容器名；Kubernetes 后端是代理 Service 的 DNS 名（或 cluster IP），服务每次运行前解析它 | 关 |
| `IDP_SANDBOX_EGRESS_KEY_FILE` | 与出口代理共用的密钥文件（至少 32 字节，如 `openssl rand -hex 32`）；设了之后每次运行的代理地址里带着说明调用者租户的凭据（ADR-0001 D21） | 不设：所有运行用同一张白名单 |
| `IDP_SANDBOX_NAMESPACE` / `_STORAGE_CLASS` / `_OWNER` | Kubernetes 后端的命名空间、存储类、清扫标签 | Pod 自己的命名空间 / 集群默认 / `default` |

出口代理（`idp-sandbox-egress`）：`IDP_EGRESS_HOSTS`（精确名或 `*.后缀`，空 = 全部拒绝）、`IDP_EGRESS_PORTS`（默认 443、80）。它自己解析并拒绝回环、私网、链路本地、云元数据，不要开 `IDP_EGRESS_ALLOW_PRIVATE`（仅测试）。白名单里不要放允许上传的主机。

**按租户的白名单（ADR-0001 D21）**：代理设 `IDP_EGRESS_KEY_FILE`（与服务的 `IDP_SANDBOX_EGRESS_KEY_FILE` 是同一个文件）和 `IDP_EGRESS_TENANTS_FILE`：

```yaml
default:                      # 没列出的租户；不写 default 就用 IDP_EGRESS_HOSTS / IDP_EGRESS_PORTS
  hosts: [pypi.org, files.pythonhosted.org]
tenants:
  7b0c0a52-…:                 # 网关转发来的租户 id
    hosts: [pypi.org, "*.acme.example"]
    ports: [443]
  globex:
    hosts: []                 # 什么都到不了
```

- 列出的租户只用自己的名单，不叠加 `default`；端口不写就是 443 和 80。
- 改了文件要重启代理。启动日志里的 `credentialRequired` 与 `tenantsWithTheirOwnList` 说明它读到了什么；每条 `egress` 日志带 `tenant` 与工作区短哈希。
- 两边要一起配：代理有密钥而服务没有，所有出口请求都是 407；服务有密钥而代理没有，代理不看凭据，所有租户仍用同一张名单。只配了租户文件没配密钥，代理不启动。
- 租户取自网关转发的调用者，所以应同时配 `IDP_SANDBOX_CALLER_KEYS_FILE`（D18），否则租户可以被伪造。
- 轮换密钥：两边换成新文件后先重启代理、再重启服务；中间那一小段时间里新的出口运行会得到 407。

**Kubernetes 上启用出口（ADR-0001 D22）**：

```bash
# 1. 服务与代理共用的密钥（不在仓库里）
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl -n idp-sandbox create secret generic idp-sandbox-egress-key --from-literal=egress.key="$(openssl rand -hex 32)"
# 2. 在 deploy/kubernetes-egress/tenants.yaml 里列主机（发布时为空：什么都不放行），把两个镜像摘要和 API 服务器的 ipBlock 换成真实值
# 3. 代替 deploy/kubernetes 应用这一层
kubectl apply -k deploy/kubernetes-egress
```

- 运行 Pod 没有 DNS，也不需要：服务把代理的 cluster IP 写进 `HTTP_PROXY` / `HTTPS_PROXY`，主机名由代理解析。代码里自己解析主机名会失败，这是预期的。
- 名单里用了 443 / 80 之外的端口，代理的 NetworkPolicy（`deploy/kubernetes-egress/networkpolicy.yaml` 里的 `egress-proxy`）也要加上；集群的 Pod、Service、节点网段若不在它排除的私网段里，要加进 `except`。
- 改 `tenants.yaml` 后重新 `kubectl apply -k`：ConfigMap 换了名字，代理滚动重启，正在进行的连接会断。轮换密钥：更新 Secret 后先 `kubectl -n idp-sandbox rollout restart deploy/idp-sandbox-egress`，再重启 `deploy/idp-sandbox`。
- 服务启动时多探测一次：与出口运行同标签的 Pod 不应连得上 API 服务器（连得上就不启动，说明有策略把出口运行放得太宽），应当连得上代理（连不上只告警）。看日志里的 `run pods with network=egress reach the egress proxy and not the API server`。
- 验证：`bash tests/kind/egress.sh`（kind + Calico；会把测试名单和 `IDP_EGRESS_ALLOW_PRIVATE` 留在集群上，只用于测试集群）。

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
| 每个 `tools/call` 都是 403 `the caller assertion is missing or invalid` | 沙箱要签名（D18）而请求没带对的：网关没设 `IDP_GATEWAY_CALLER_KEY_FILE`、公钥不是那把私钥的、`_CALLER_AUDIENCE` 与登记的端点不逐字相同、两边时钟差超过 30 秒，或有人绕过网关直连 | 看沙箱日志里 `caller assertion refused` 的 `reason`；对一下网关启动日志里的 `kid` 与沙箱启动日志里的 `keys` |
| Kubernetes 后端启动即退出：`a pod labelled like a run reached …` | 运行命名空间的 NetworkPolicy 没有应用，或集群的 CNI 不执行 NetworkPolicy（D20）| 应用 `deploy/kubernetes/networkpolicy.yaml`；换一个执行策略的 CNI（Calico、Cilium）。确认只是开发环境时 `IDP_SANDBOX_NETWORK_CHECK=warn` |
| 工具列表变了 / 网关说目录漂移 | 改了 schema（出口、限额）| 对服务器重新 discover、评审、发布 Release |
| `workspace not found or expired` | id 不对、属于别的调用者、已过期或沙箱重启过 | 重新 `sandbox_create`（别的调用者的 id 与不存在的 id 回答一样，这是有意的）|
| `the sandbox is at capacity` | 并发或总数上限 | 稍后重试；调大 `_MAX_CONCURRENT` / `_MAX_WORKSPACES` |
| `you already hold the maximum number of workspaces` | 每个调用者的上限 | `sandbox_delete` 一个 |
| `network access is not enabled` | 没配出口代理 | 配代理并重新发布 Release（schema 会变）|
| 服务不启动：`reached … lets them reach more than the egress proxy`（Kubernetes）| 运行命名空间里有策略把带 `idp.sandbox/network=egress` 的 Pod 放到了代理之外 | `kubectl -n idp-sandbox-runs get networkpolicy`，删掉或收紧多出来的那条 |
| 日志告警 `did not reach the egress proxy`，出口运行全部连接失败（Kubernetes）| 代理没起来（Secret `idp-sandbox-egress-key` 不存在时 Pod 停在 `ContainerCreating`）、Service 名解析不到，或 CNI 没按 Pod 选择器放行 | 建 Secret；核对 `IDP_SANDBOX_EGRESS_PROXY`；代理就绪后重启服务再看探测结果 |
| 出口运行里解析主机名失败（Kubernetes）| 运行 Pod 没有 DNS，这是设计 | 用读 `HTTP_PROXY` / `HTTPS_PROXY` 的客户端，让代理去解析 |
| 出口请求全是 407 | 代理有 `IDP_EGRESS_KEY_FILE`，服务没有 `IDP_SANDBOX_EGRESS_KEY_FILE`，或两边的密钥不是同一个；也可能是代码自己拼了不带凭据的代理地址 | 两边用同一个密钥文件；代码用环境里的 `HTTP_PROXY` / `HTTPS_PROXY` |
| 某个租户到不了默认名单里的主机 | 它在 `IDP_EGRESS_TENANTS_FILE` 里被列出：列出的租户只用自己的名单 | 把需要的主机加进它自己的名单 |
| 运行总是 `timed out` | 代码慢，或在 gVisor 下系统调用密集 | 调大 `_TIMEOUT_S`（上限）；评估运行时的开销 |
| 运行是 `killed: out of memory` | 内存上限 | 调大 `_MEMORY_MB`；Kata 下写满 `/tmp` 也表现为 OOM |
| `docker: HTTP 404: No such image` | 运行镜像不在 Docker 守护进程 / 节点上 | 运维拉取或加载（服务绝不拉镜像）|
| 运行里 `md2pdf: not found`、`pdftext: not found`、`sheetmap: not found`、`sheetcheck: not found`、`officecli: not found` | 这次运行用的是 `language: shell` 或 `python`；这些命令只在 office 镜像里 | 用 `language: office`（Agent 的 skill 里要写明每次运行都用它）|
| 构建 office 镜像时 `is not the pinned …: its SHA-256 differs` | 上游换了这个地址上的文件，或缓存目录里的文件坏了 | 核对上游的校验和后再改脚本里的钉；不要跳过校验 |
| Pod 一直 `ContainerCreating`（Kubernetes）| 运行时问题 | gVisor：用 `kvm` 或 `ptrace` 平台，节点 inotify 上限调到 8192；Kata：Go 运行时 + Cloud Hypervisor，别给 RuntimeClass 加 overhead |
| gVisor 下内存炸弹只被超时停下，运行的内存 / CPU 上限不生效（Kubernetes）| 节点用 systemd cgroup 驱动而 runsc 没开 `systemd-cgroup`：沙箱跑在 Pod 的 cgroup 之外；节点有 swap 时还有一层：kubelet 只给容器关 swap，而整个沙箱在 pause 容器的 cgroup 里 | `runsc.toml` 的 `[runsc_config]` 加 `systemd-cgroup = "true"`；节点有 swap 就给 Pod 关掉（`kubelet-kubepods.slice` 的 `MemorySwapMax=0`）；kind 上 `tests/kind/gvisor.sh` 都做了 |
| 网关到沙箱超时 | 网络策略没放行 8570 | 网关与注册服务的出口策略加 8570；沙箱命名空间要带 `idp-mcp/mcp-upstream: "true"` 标签 |
| 运行 Pod 能联网（Kubernetes）| CNI 不执行 NetworkPolicy | 换 Calico / Cilium；用 `tests/kind/check.py` 的 `no-network` 项验证 |
| gVisor 下 `--pids-limit 16` 起不来 | gVisor 把自己的宿主线程算进上限 | 用 64（默认）|
| 出口请求在 gVisor 下全失败 | Docker 内置 DNS 在 gVisor 网络栈里不生效 | 已按 IP 给代理地址；确认用的是新版本 |

## 7. 安全边界（运维要知道的）

- 默认运行时（runc）与宿主共享内核：对真正不可信的代码，上 gVisor 或 Kata，并保留全部其他层（无网络、限额、非 root、无令牌、按调用者隔离）。
- 身份这一跳的签名要配置：给网关一把 P-256 私钥（`IDP_GATEWAY_CALLER_KEY_FILE`），把公钥给沙箱（`IDP_SANDBOX_CALLER_KEYS_FILE`），并告诉沙箱它在 idp-mcp 里登记的端点（`IDP_SANDBOX_CALLER_AUDIENCE`，要与登记的 URL 逐字相同）。生成：`openssl ecparam -name prime256v1 -genkey -noout -out caller.key && openssl ec -in caller.key -pubout -out caller.pub`。换钥匙时先把新公钥追加进沙箱的文件并重启沙箱，再换网关的私钥，最后删掉旧公钥。没配时，只有网关能到达沙箱的网络策略就是边界，不要把 8570 暴露给别人；配了也保留这条网络策略。
- Docker 后端要 Docker 套接字：专用节点、虚拟机或 rootless 引擎，不要和别的服务共用。
- 出口白名单按主机名，可以按租户分（D21），不按人或角色分；能上传的主机会成为外传通道。Kubernetes 上的出口（D22）只在 kind + Calico 上测过，没有真实的公网流量走过，边界见 ADR 第 5 节。
- 加固运行时的实测数据和限制见 ADR 第 5 节。
