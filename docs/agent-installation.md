# Agent 安装与卸载

## 安装脚本从哪里获取

Agent 安装器由每个 Meridian 主控实例公开提供：

```text
https://你的主控域名:端口/api/agent/install.sh
```

GitHub 仓库保存源码和版本；运行中的主控提供实际安装入口，并从自己的 `/api/agent/binary` 返回匹配版本的 Agent。这样用户部署自己的 Meridian 后，不需要访问项目作者的面板，也不会把节点注册到其他人的主控。

## 一键安装

在自己的主控面板中创建节点，复制面板生成的完整命令，在目标 Linux x86_64 VPS 以 root 执行：

```bash
wget -qO- https://panel.example.com:9090/api/agent/install.sh | sudo bash -s -- \
  -c https://panel.example.com:9090 \
  -t ONE_TIME_ENROLLMENT_TOKEN
```

参数说明：

- `-c` / `--controller`：自己的主控 HTTPS 地址，必须带正确端口。
- `-t` / `--token`：面板为该节点生成的一次性注册令牌，默认 24 小时有效。

安装器只支持 Linux x86_64，并要求节点可以通过 HTTPS 访问主控。脚本会自动识别默认路由网卡，不需要手动填写 `eth0`、`ens3` 等网卡名。

## 安装后检查

```bash
sudo systemctl is-enabled meridian-agent
sudo systemctl is-active meridian-agent
sudo journalctl -u meridian-agent -n 100 --no-pager
```

主控“节点调度”页面应显示在线、网卡名称、最近心跳和配置已应用。若端口冲突，日志会显示监听错误；修改面板中的节点端口后，等待 Agent 轮询配置即可。

## 卸载 Agent

先在主控删除节点或刷新节点注册令牌，使旧 Agent 失去授权。然后在目标节点执行：

```bash
sudo systemctl disable --now meridian-agent.service
sudo rm -f /etc/systemd/system/meridian-agent.service
sudo systemctl daemon-reload
sudo rm -rf /opt/meridian-agent /var/lib/meridian-agent /etc/meridian-agent
```

卸载只会删除 Meridian Agent 的服务、程序、状态、事件队列和令牌文件，不会删除站点数据，不会修改 Nginx、Caddy、Xray 或其他业务配置。完成后可用下面的命令确认 9090 不再由 Agent 监听：

```bash
sudo systemctl status meridian-agent.service --no-pager
sudo ss -lntp | grep ':9090' || true
```

如果节点使用的是其他端口，请替换最后一条命令中的端口号。

## 安全注意事项

- 不要把完整安装命令、一次性令牌或 Agent 长期令牌提交到 GitHub。
- 不要让不可信用户获得主控管理员账号，否则对方可以创建、删除或刷新节点。
- `/api/agent/install.sh` 不包含节点凭据；真正的二进制下载、注册、配置和上报接口都需要 Bearer 令牌。
- 需要重新绑定主控时，应先删除旧节点，再从新主控生成新的安装命令。

