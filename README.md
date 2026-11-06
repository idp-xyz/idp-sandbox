# idp-sandbox

IDP TELOS 的 Agent 执行沙箱：一个 MCP 服务器，让 Agent 在隔离的容器里运行自己写的代码（Python、Shell），并在配额受限的工作区里读写文件。它放在 idp-mcp 网关后面作为上游：认证、授权、参数校验和调用审计都是网关的事，idp-host 不需要任何改动（HOST 的设计就是"工具都经网关"）。设计与取舍见 [`docs/adr/ADR-0001-sandbox-design.md`](docs/adr/ADR-0001-sandbox-design.md)。

状态：**M1 完成并实测**。

## 隔离

每次运行是一个全新的容器：

| 约束 | 做法 |
|---|---|
| 网络 | 默认无（`NetworkMode none`，只有回环）。运维配置了 `idp-sandbox-egress` 时可选 `network: egress`：运行得到自己的内部网络，只连着按主机名白名单放行的代理（拒绝回环、私网、云元数据，防 DNS 重绑定），见 ADR D13 |
| 文件系统 | 根文件系统只读；`/tmp` 与工作区是有大小上限的 tmpfs（内核执行配额） |
| 权限 | 用户 65534；丢弃全部 capabilities；`no-new-privileges`；默认 seccomp；非特权；私有 IPC；`init` |
| 资源 | 内存（含交换）、CPU、进程数、`nofile`、`fsize`、单次运行时间、输出大小（超限即终止并标记截断） |
| 镜像 | 由运维配置，调用方不能指定 |
| 会话 | 工作区属于创建它的调用者（网关转发的 tenant + subject；别人拿着 id 也只会得到"不存在"）；每个调用者最多 8 个；id 另外是 128 位随机数；一次只能跑一个命令；空闲 30 分钟、存活 4 小时后删除；总数有上限 |
| 后端 | `IDP_SANDBOX_RUNTIME=runsc`（gVisor）或 `kata` 换成更强的隔离，其余不变 |

## 工具

`sandbox_create`、`sandbox_run`（`python` 或 `shell`）、`sandbox_write_file`、`sandbox_read_file`、`sandbox_list_files`、`sandbox_delete`。经网关时名字带上目标名前缀（`sandbox__sandbox_run`）。

## 使用

```bash
make test               # gofmt、vet、单元测试（-race）
make test-integration   # 真实 Docker：网络、出口代理、fork 炸弹、内存炸弹、死循环、输出洪水、磁盘写满、符号链接、工作区互相隔离
bash tests/kind/smoke.sh # Kubernetes 后端在真实 kind 集群上（CI 的 kubernetes 任务）
bash deploy/e2e.sh      # 经真实的 idp-mcp 网关：注册、评审、授权、发布，再只经网关使用沙箱（需要并排的 idp-mcp）
bash deploy/build.sh    # 镜像（scratch）
```

运行：`IDP_SANDBOX_LISTEN=127.0.0.1:8570 ./bin/idp-sandbox`（配置项见 `cmd/idp-sandbox/main.go` 文件头）。绑定非回环地址必须设置 `IDP_SANDBOX_PRIVATE_NETWORK=true`：服务器自己没有认证，只有网关能到达它。

## Kubernetes 后端

`IDP_SANDBOX_BACKEND=kubernetes`：同一套隔离模型，只用 Kubernetes REST API（标准库 net/http，集群内用服务账号令牌与 CA；没有 exec / websocket，只用 Job、PVC、ConfigMap 和 Pod 日志）。实现在 `internal/kube`，清单在 `deploy/kubernetes`（kustomize）。

| 对象 | 做法 |
|---|---|
| 工作区 | PVC（`ReadWriteOnce`，大小 = 工作区配额，可选 `IDP_SANDBOX_STORAGE_CLASS`），标签 `idp.sandbox=<owner>` |
| 运行 | 一个 Job（`backoffLimit 0`，`activeDeadlineSeconds` = 超时 + 30 s 宽限）；Pod：用户 65534、`runAsNonRoot`、`allowPrivilegeEscalation false`、只读根、丢弃全部 capabilities、seccomp `RuntimeDefault`、不挂服务账号令牌、不用宿主命名空间、内存与 CPU 的 requests = limits、`/tmp` 是内存型 emptyDir（`sizeLimit` = 配额）；可选 `IDP_SANDBOX_RUNTIME_CLASS`（gVisor / Kata） |
| 无网络 | `idp-sandbox-runs` 命名空间里默认拒绝入站与出站，并对标签 `idp.sandbox/role in (run, file)` 的 Pod 明确全拒（NetworkPolicy，需要 CNI 执行它） |
| 代码与文件 | 不能 exec：字节放进 ConfigMap（`binaryData.data`，上限 1 MiB），挂到 `/in`，由一个短 `file` Job 复制到 `/work`；读与列目录是同一类 Job，脚本与 Docker 后端相同（拒绝符号链接，退出码 44 / 45） |
| 结果 | 轮询 Job 状态；stdout 与 stderr 合并在 Pod 日志里，所以命令被包一层，stderr 写到文件，再在每次运行随机的标记之后打印；退出码、`OOMKilled` 取自容器的终止状态，`DeadlineExceeded` 或超过运行时钟即 `TimedOut`；Job、Pod、ConfigMap 总是被删除（含取消与出错） |
| 命名空间 | 服务在 `idp-sandbox`，运行在 `idp-sandbox-runs`（都强制 `restricted`）；服务账号只在运行命名空间有一个 Role：Job / ConfigMap / PVC 的 get、list、create、delete，Pod 的 get、list、delete，Pod 日志的 get；ResourceQuota 与 LimitRange 限制 Pod、CPU、内存、PVC 数量与容量 |

环境变量：`IDP_SANDBOX_NAMESPACE`（默认 Pod 自己的命名空间，否则 `idp-sandbox`）、`IDP_SANDBOX_STORAGE_CLASS`、`IDP_SANDBOX_RUNTIME_CLASS`、`IDP_SANDBOX_OWNER`；测试或集群外用 `IDP_SANDBOX_KUBE_API`、`IDP_SANDBOX_KUBE_TOKEN` / `_TOKEN_FILE`、`IDP_SANDBOX_KUBE_CA_FILE`。

与 Docker 后端的差别：进程数上限（`Pids`）在 Pod 上无法设置，由节点的 kubelet（`podPidsLimit`）或 gVisor 承担；每次文件操作是一个 Pod（秒级延迟）；超时被杀时 stderr 拿不到（容器整体被杀，标记没打印）；`/tmp` 没有 `noexec`；部署必须单副本（`Recreate`），因为启动清扫会删除同一 owner 的全部对象。网关的 NetworkPolicy 出站目前只放行 443 / 8443，要到沙箱需要给网关加 8570（idp-mcp 的清单，不在本仓库）。`kubectl apply --dry-run=server -k deploy/kubernetes` 与 kind 运行尚未做；`go test ./tests/deploy/` 只检查清单自身的一致性。

## 必读的限制

- **容器共享宿主内核**。默认的 runc 加固挡不住内核漏洞；对不可信代码的生产部署应使用 gVisor 或 Kata（本机没有 runsc，未实测）。
- **服务需要容器引擎的套接字，那等于宿主的 root**。开发拓扑直接挂载 `/var/run/docker.sock`；生产应放在专用节点或虚拟机，或使用 rootless 引擎 / 套接字代理。Kubernetes 后端见下（`IDP_SANDBOX_BACKEND=kubernetes`，用 Job，不需要容器引擎套接字；只有对假 API 服务器的单元测试，未在真实集群上验证）。
- **调用者身份来自网关，这一跳本身没有认证**：沙箱要求每个请求带网关转发的调用者（在 idp-mcp 里以 `identity: forward` 注册，idp-mcp ADR-0001 §12），工作区按它隔离。身份没有签名：沙箱靠"只有网关能到达它的私网"相信网关，所以网络策略就是边界。`IDP_SANDBOX_REQUIRE_CALLER=false` 只用于不经网关的开发运行（所有匿名调用共享一个所有者）。
- 单实例：工作区表在内存里，重启即清空（工作区本来就在内存里）。

## 许可证

[PolyForm Noncommercial License 1.0.0](LICENSE)。
