# idp-sandbox

IDP TELOS 的 Agent 执行沙箱：一个 MCP 服务器，让 Agent 在隔离的容器里运行自己写的代码（Python、Shell），并在配额受限的工作区里读写文件。它放在 idp-mcp 网关后面作为上游：认证、授权、参数校验和调用审计都是网关的事，idp-host 不需要任何改动（HOST 的设计就是"工具都经网关"）。设计与取舍见 [`docs/adr/ADR-0001-sandbox-design.md`](docs/adr/ADR-0001-sandbox-design.md)。

状态：**M1 完成并实测**。

## 隔离

每次运行是一个全新的容器：

| 约束 | 做法 |
|---|---|
| 网络 | 无（`NetworkMode none`，只有回环），不能联网，也不能安装包 |
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
make test-integration   # 真实 Docker：网络、fork 炸弹、内存炸弹、死循环、输出洪水、磁盘写满、符号链接、工作区互相隔离
bash deploy/e2e.sh      # 经真实的 idp-mcp 网关：注册、评审、授权、发布，再只经网关使用沙箱（需要并排的 idp-mcp）
bash deploy/build.sh    # 镜像（scratch）
```

运行：`IDP_SANDBOX_LISTEN=127.0.0.1:8570 ./bin/idp-sandbox`（配置项见 `cmd/idp-sandbox/main.go` 文件头）。绑定非回环地址必须设置 `IDP_SANDBOX_PRIVATE_NETWORK=true`：服务器自己没有认证，只有网关能到达它。

## 必读的限制

- **容器共享宿主内核**。默认的 runc 加固挡不住内核漏洞；对不可信代码的生产部署应使用 gVisor 或 Kata（本机没有 runsc，未实测）。
- **服务需要容器引擎的套接字，那等于宿主的 root**。开发拓扑直接挂载 `/var/run/docker.sock`；生产应放在专用节点或虚拟机，或使用 rootless 引擎 / 套接字代理。Kubernetes Job 后端未实现（`Backend` 是接口）。
- **调用者身份来自网关，这一跳本身没有认证**：沙箱要求每个请求带网关转发的调用者（在 idp-mcp 里以 `identity: forward` 注册，idp-mcp ADR-0001 §12），工作区按它隔离。身份没有签名：沙箱靠"只有网关能到达它的私网"相信网关，所以网络策略就是边界。`IDP_SANDBOX_REQUIRE_CALLER=false` 只用于不经网关的开发运行（所有匿名调用共享一个所有者）。
- 单实例：工作区表在内存里，重启即清空（工作区本来就在内存里）。

## 许可证

[PolyForm Noncommercial License 1.0.0](LICENSE)。
