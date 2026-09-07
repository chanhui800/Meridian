# Agent 安装与卸载

Meridian Agent 只负责节点入口、反代、流量采集和状态上报。它运行在 Linux amd64 或 arm64 节点上，Controller 本身不需要把 Agent 二进制打进 Docker 镜像。

## 安装脚本从哪里获取

面板生成的安装命令从当前 Controller 的固定入口获取：

```text
https://<你的主控域名>/api/agent/install.sh
```

运行中的 Controller 会返回与自身版本绑定的安装器、平台信息、SHA-256 和 Release Manifest。安装器随后从该版本的 GitHub Release 下载对应架构的 Agent，并在替换本机服务前完成校验。

## 一键安装

在“节点调度”页面创建节点，复制一次性显示的命令，在目标 Linux VPS 以 root 执行：

```bash
curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
  https://panel.example.com:9090/api/agent/install.sh | \
  sudo bash -s -- \
  --controller https://panel.example.com:9090 \
  --token 'ONE_TIME_ENROLLMENT_TOKEN'
```

参数：

- `-c`、`-e`、`--controller`、`--endpoint`：Controller 的 HTTPS 地址，必须带正确端口。
- `-t`、`--token`：Controller 为该节点生成的一次性注册令牌，默认有效期 24 小时。
- `--reenroll`：重新生成脚本后使用。它会清理本机旧的注册状态，等待新的注册状态写入。

安装器只支持 Linux amd64 和 arm64，并要求节点能通过 HTTPS 访问 Controller。脚本会先下载并校验二进制、平台和 SHA-256，再停止现有服务。首次安装和重新注册会等待 Agent 真正完成注册，不会仅凭 systemd 的 `active` 状态报告成功。

## 安装后检查

```bash
sudo systemctl is-enabled meridian-agent
sudo systemctl is-active meridian-agent
sudo journalctl -u meridian-agent -n 100 --no-pager
sudo ss -lntp | grep ':9090'
```

如果节点使用其他端口，请替换最后一条命令中的端口号。Controller 的“节点调度”页面应显示在线、网卡名称、最近心跳和“配置已应用”。如果日志出现监听端口冲突，请先释放端口或修改节点端口。

## 升级和重新注册

普通升级只替换经过校验的 Agent 二进制，并保留 `state.json`、节点 GUID 和长期令牌。不要为了升级而删除 `/var/lib/meridian-agent`。

只有以下情况需要使用 `--reenroll`：

- 在 Controller 中主动刷新了节点注册脚本；
- 节点上的 Agent 状态文件已经丢失；
- 节点需要绑定到另一套 Controller；
- 日志明确显示长期令牌已撤销。

重新生成脚本不会立即中断正在运行的 Agent。旧令牌会继续有效，直到新脚本完成注册并由 Controller 原子替换。新脚本一直没有执行时，一次性令牌会过期，但旧 Agent 不会因为这个过期时间被自动踢下线。

## 卸载 Agent

先在 Controller 中删除节点，再在目标节点执行：

```bash
sudo systemctl disable --now meridian-agent.service
sudo rm -f /etc/systemd/system/meridian-agent.service
sudo systemctl daemon-reload
sudo rm -rf /opt/meridian-agent /var/lib/meridian-agent /etc/meridian-agent
```

卸载只会删除 Meridian Agent 的服务、程序、状态、事件队列和令牌文件，不会删除站点数据，也不会修改 Nginx、Caddy、Xray 或其他业务配置。

## 安全注意事项

- 不要把完整安装命令、一次性令牌或长期 Agent 令牌提交到 GitHub、聊天记录或公开日志。
- 只从自己的 Controller 获取安装脚本；安装命令中的域名和令牌应使用面板刚生成的值。
- 不要让不可信用户获得 Controller 管理员账号，否则对方可以创建、删除或刷新节点。
- `/api/agent/install.sh` 本身是公开脚本，但注册、配置、二进制清单和上报接口都需要有效的 Bearer 令牌。
