# 节点调度

节点调度是 Meridian 的可选分布式入口模块。Controller 保存节点、站点分配、优先级、流量周期和 DNS 状态；每台 VPS 只运行一个轻量 `meridian-agent`，负责 HTTPS 监听、站点反代、网卡流量采集和状态上报。

![节点调度总览示例](images/node-scheduling.png)

> 图片只用于说明页面布局，节点地址、站点域名和统计数据均为虚构示例。

## 工作方式

```text
客户端 -> 站点域名 -> 精确 DNS A/AAAA -> 节点 Agent -> 站点上游 Emby/Jellyfin
                                      ^
                                      |
                              Controller 配置与状态
```

- 未启用调度的站点继续使用原面板入口，不会改变现有 DNS。
- 启用调度后，Controller 把站点 Host、上游线路、播放改写、TLS 证书和运行配置下发给目标 Agent。
- Agent 应用配置并通过 HTTPS/SNI 健康检查后，Controller 才创建或更新自己管理的精确 DNS 记录。
- DNS 切换只影响新连接，已经建立的播放连接不会被强制迁移。
- 节点流量额度按 Agent 采集的网卡总收发字节计算，也会包含该 VPS 上其他业务产生的流量。
- Controller 会验证每条 Agent 报告中的 Node → Site 授权；一个 Agent 不能替其他节点写入站点统计、请求事件或观看历史。

## 前置条件

1. Controller 必须通过有效 HTTPS 证书访问，不能使用自签名证书。
2. 节点需要 Linux amd64 或 arm64、root 权限和可访问 Controller 的出站 HTTPS。
3. 节点端口不能被 Xray、Nginx、Caddy 或其他服务占用。443 被占用时，可以使用 9090 或 9443 等空闲端口。
4. 站点域名需要由 DNS 服务商解析到节点地址。使用自动 DNS 时，在 Controller TLS 设置中配置 Cloudflare DNS API Token。
5. 面板域名和节点泛域名必须属于同一注册域但不能相同。面板可以使用多级子域名，现有站点域名无需迁移。Controller 使用自己的 Panel 证书；每台 Agent 使用独立的 Edge 证书和私钥，不能复用 Panel 私钥。
6. Meridian 自己管理的 TLS 状态必须位于 `TLS_STATE_DIR`。`PANEL_TLS_*` 和 `EDGE_TLS_*` 可以指向运维方管理的外部证书文件，恢复流程不会删除这些文件。

Docker Controller 在配置 Cloudflare ACME 凭据后，可以执行：

```bash
docker exec meridian /app/meridian admin issue-edge-certificate
```

该命令为已注册且启用的节点签发独立 Edge 证书。节点注册后会进入证书任务队列，失败按退避策略重试；证书缺失时，Agent 配置会保持等待，并在调度页面显示最近一次错误。

## 创建并安装节点

1. 打开 Controller 的“节点调度”页面，点击“添加节点”。
2. 填写节点名称、显示地址和 Agent HTTPS 端口。
3. 设置流量上限、每月重置日、计费方式和优先级。流量上限填 `0` 表示不限额，重置日填 `0` 表示不自动重置。
4. Controller 地址填写当前主控的完整 HTTPS 地址，例如 `https://panel.example.com:9090`。
5. 保存后，面板会显示一次性注册令牌和一键安装命令。只在目标节点上执行该命令。

典型命令如下，实际令牌必须使用面板生成的值：

```bash
wget -qO- https://panel.example.com:9090/api/agent/install.sh | sudo bash -s -- \
  --controller https://panel.example.com:9090 \
  --token 'ONE_TIME_ENROLLMENT_TOKEN'
```

安装器会从固定版本的 GitHub Release 下载并校验对应架构的 Agent，创建并启动 `meridian-agent.service`。注册成功后，一次性令牌会被替换为长期凭据。Controller 镜像不需要携带 Agent 二进制。

## 验证 Agent

在节点上执行：

```bash
sudo systemctl status meridian-agent --no-pager
sudo journalctl -u meridian-agent -n 100 --no-pager
sudo ss -lntp | grep ':9090'
```

在 Controller 的节点调度页面确认状态为“在线”、网卡名称已识别、配置状态为“配置已应用”。如果节点端口不是 9090，把最后一条命令中的端口替换为实际值。

## 配置站点调度

1. 在“站点调度”列表找到目标站点。
2. 勾选“启用节点调度”。
3. 选择“跟随全局调度”或“固定节点”。固定模式还要选择具体节点。
4. 点击“保存”，等待 Agent 应用配置和 DNS 健康检查完成。

自动模式会排除未启用、心跳超时、配置未应用或已达到额度的节点，再按优先级和周期用量选择节点。手动模式只使用管理员指定的节点；指定节点不可用时保持等待，不会悄悄改用其他节点。

站点状态会显示期望节点、生效节点、节点端口、DNS 状态、最近请求和最终 HTTP 状态。停用调度时，Meridian 只删除自己创建并记录的精确 DNS 记录，不会删除用户自行创建的同名记录。

Agent 每 15 秒上报一次心跳和累计流量。Controller 对 HTTP 与 WebSocket 报告共用每节点速率限制和单请求并发限制；正常的 15 秒周期不会触发限制。越权报告会被丢弃并计入安全审计，不会进入站点数据库。

## 升级与重装

正常更新 Controller 后，Agent 会通过已认证的配置接口获取新二进制，校验 SHA-256 后原子替换并重启。普通更新不会使节点令牌失效，也不需要重复安装。

只有以下情况才需要重新生成脚本并重新注册：

- 管理员在 Controller 中主动刷新节点注册脚本；
- 节点上的 Agent 状态文件被删除；
- 节点更换到另一套 Controller；
- 日志明确显示长期令牌已被撤销。

重新生成脚本不会立即中断当前 Agent。旧令牌会继续有效，直到新的安装脚本完成注册并由 Controller 原子替换；脚本一直没有执行时，一次性令牌会过期，但现有 Agent 不会因此被自动踢下线。

## 删除节点

先在 Controller 中删除节点，主控会撤销该节点的 Agent 授权。然后在节点执行卸载步骤。删除节点不会自动删除节点上的其他业务进程，也不会修改 Nginx、Caddy 或 Xray 配置。
