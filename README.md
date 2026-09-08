# Meridian

[![CI](https://github.com/chanhui800/Meridian/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/chanhui800/Meridian/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/chanhui800/Meridian)](https://github.com/chanhui800/Meridian/releases/latest)
[![License](https://img.shields.io/github/license/chanhui800/Meridian)](LICENSE)

Meridian 是 Emby 和 Jellyfin 的多站点反向代理面板。它把站点入口、备用线路、播放地址改写、流量统计、请求日志、观看历史、TLS 和可选的节点调度放在一个控制面板里。

## 界面预览

下面的截图只用于展示界面布局，使用的是演示数据。截图中没有生产环境的域名、IP、令牌或证书。

| 仪表盘 | 站点管理 |
| --- | --- |
| ![仪表盘](docs/images/dashboard.png) | ![站点管理](docs/images/sites.png) |
| 节点调度 | 请求日志 |
| ![节点调度](docs/images/node-scheduling.png) | ![请求日志](docs/images/request-logs.png) |
| 观看历史 | 流量统计 |
| ![观看历史](docs/images/watch-history.png) | ![流量统计](docs/images/traffic.png) |
| TLS 设置 | Telegram 日报 |
| ![TLS 设置](docs/images/tls-certificate.png) | ![Telegram 日报](docs/images/telegram-report.png) |

示例图片保存在 [`docs/images`](docs/images/)。更新文档时请保留这些文件和上面的引用。

## 功能

- 管理多个站点和上游线路，支持主线路、备用线路和自动故障切换。
- 支持独立端口、路径前缀和域名前缀三种入口方式。
- 改写 PlaybackInfo、HLS、DASH、HTTP 重定向、字幕、图片和 WebSocket 地址。
- 主视频流可以反代，也可以在上游返回合法公网 30x 时让客户端直连 CDN。
- 记录请求、最终后端、客户端信息、收发字节和趋势。
- 可选观看历史、媒体库数量、保号提醒、TMDB 补全和 Telegram 日报。
- 支持 ACME DNS-01、自动续签、加密备份与恢复。
- 可选节点调度：一个 Controller 管理多台 Linux Agent，DNS 只切换新连接。
- 站点图标由用户上传图标包，支持按图标名称搜索和分配；项目不内置图标包。

## 快速安装

### Docker

```yaml
services:
  meridian:
    image: ghcr.io/chanhui800/meridian:latest
    container_name: meridian
    restart: unless-stopped
    network_mode: host
    volumes:
      - ./data:/app/data
    environment:
      PORT: "9090"
      DB_PATH: /app/data/meridian.db
```

```bash
mkdir -p data
docker compose up -d
docker compose logs -f meridian
```

首次启动时日志会输出一次管理员初始化令牌。打开 `http://服务器地址:9090` 完成初始化。数据库、TLS 状态和缓存位于 `./data`，更新容器不会删除它们。`network_mode: host` 会让面板和站点端口直接使用宿主机网络，请先确认端口没有被其他服务占用。

生产环境可以把 `latest` 换成固定版本。固定版本便于回滚和排查问题。

### Linux 原生安装

安装最新正式版：

```bash
curl -fsSL \
  https://github.com/chanhui800/Meridian/releases/latest/download/install.sh \
  | sudo bash -s -- install -y --no-domain
```

安装器会自动获取最新正式 Release，并校验实际下载的程序。需要固定版本或在高安全环境中安装时，再使用固定 Release 的校验流程；不要从可变的 `main` 分支直接执行安装脚本。

卸载 Meridian（默认保留数据库、TLS 状态和备份）：

```bash
curl -fsSL \
  https://github.com/chanhui800/Meridian/releases/latest/download/install.sh \
  | sudo bash -s -- uninstall -y
```

确认不再保留数据时使用 `uninstall -y --purge`；该操作会删除 Meridian 数据目录。

默认数据库为 `/var/lib/meridian/meridian.db`，服务名为 `meridian`：

```bash
sudo systemctl status meridian --no-pager
sudo journalctl -u meridian -f
```

安装器会按 CPU 架构下载并校验 GitHub Release 二进制。升级时重新运行安装器，数据库、TLS 状态和缓存会保留。

## 站点管理

新增站点时填写站点名称、入口方式和上游线路。入口示例：

| 方式 | 示例 |
| --- | --- |
| 独立端口 | `http://panel.example.com:9001` |
| 路径前缀 | `https://panel.example.com:9090/movie` |
| 域名前缀 | `https://movie.example.com:9090` |

每个站点可以配置一条主线路和最多七条备用线路。普通 API、HLS/DASH、字幕、图片和 WebSocket 默认反代。主视频流选择“直连”时，只有上游返回合法公网 30x 才会把连接交给客户端。

### 站点图标

图标包完全由用户提供，Meridian 不内置任何图标。打开“站点管理”，点击“图标包”，可以上传本地 JSON 文件，也可以填写公开的 HTTPS JSON 地址。系统只保存图标名称和 HTTPS 图片地址，不会在服务器端抓取图标地址。

图标包格式如下：

```json
{
  "name": "我的图标包",
  "description": "可选说明",
  "icons": [
    { "name": "Emby", "url": "https://cdn.example.com/emby.png" },
    { "name": "Jellyfin", "url": "https://cdn.example.com/jellyfin.png" }
  ]
}
```

导入后，点击站点卡片或站点实时状态中的圆形图标，在二级浮窗中按名称搜索并选择图标。清空图标包不会清除已经分配给站点的图标；若要移除某个站点图标，在该站点的图标管理浮窗中选择清除。

图标包大小上限为 4 MiB，最多 4096 个图标。图片地址必须是 HTTPS，名称不能包含换行或控制字符。

域名入口需要在“全局设置 → TLS 设置”中分别填写面板完整域名（例如 `panel.admin.example.com`）和节点泛域名（例如 `*.example.com`）。两者必须属于同一注册域但不能相同，现有站点域名无需迁移。Controller 证书会按配置覆盖面板域名，并在需要承载 Host 入口时包含节点泛域名；每台 Edge Agent 使用自己的证书和私钥，不共享 Controller 私钥。

## 节点调度

节点调度是可选模块。Controller 保存节点、站点分配、优先级、流量周期和 DNS 状态；每台 VPS 只运行一个轻量 `meridian-agent`，不需要安装完整面板。详细步骤见：[节点调度](docs/node-scheduling.md) 和 [Agent 安装与卸载](docs/agent-installation.md)。

调度过程如下：

```text
客户端 -> 站点域名 -> 精确 DNS A/AAAA -> Edge Agent -> 上游 Emby/Jellyfin
                                      ^
                                      |
                              Controller 配置与状态
```

站点启用调度后，Controller 会把路由、线路、播放改写和 TLS 配置下发给目标 Agent。Agent 应用配置并通过 HTTPS/SNI 健康检查后，Controller 才创建或更新自己管理的精确 DNS 记录。DNS 只影响新连接，已有播放连接不会被强制迁移。

自动模式会排除未启用、心跳超时、配置未应用或已达到流量额度的节点，再按优先级和周期用量选择节点。手动模式只使用管理员指定的节点；指定节点不可用时会保持等待，不会偷偷切到其他节点。停用或删除调度时，只删除 Meridian 自己保存的 DNS 记录 ID，不触碰用户手动创建的同名记录。

Agent 每 15 秒上报一次心跳、网卡累计收发字节、配置版本和监听错误。Controller 会在报告写入前验证 Node → Site 授权，并对 HTTP 和 WebSocket 报告使用同一套每节点速率与并发限制。一个被攻陷的 Agent 只能影响当前 `desired_node_id` 或 `applied_node_id` 范围内的站点。

主控配置 Cloudflare ACME 凭据后，可以执行：

```bash
docker exec meridian /app/meridian admin issue-edge-certificate
```

该命令为已注册且启用的节点签发独立 Edge 证书。新节点注册后会进入证书任务队列，失败会按退避策略重试。证书缺失或配置尚未应用时，站点会保持等待状态，并在节点调度页面显示最近一次错误。

## TLS、备份和数据边界

面板可以直接监听 HTTPS，也可以放在已有反向代理后面。使用 80 或 443 前，请先确认没有其他进程占用。

`TLS_STATE_DIR` 是 Meridian 唯一可以整体管理的 TLS 状态目录，默认位于数据库目录下的 `tls`。面板和边缘证书、ACME 账户、节点证书状态以及恢复事务文件都保存在这个目录。`PANEL_TLS_*` 和 `EDGE_TLS_*` 可以指向运维方管理的外部证书文件；包含 TLS 的备份恢复不会删除或替换这些外部文件。

备份页面生成带密码的 `.mrbak` 文件。新备份使用 `MRDBKP02` 分块加密格式，旧的 `MRDBKP01` 备份仍可读取。备份恢复会校验文件数量、展开大小、数据库 schema 版本、路径和证书密钥对；恢复失败时原数据库和 TLS 状态不会被部分覆盖。默认不包含 TLS，迁移面板域名或节点证书时再显式勾选。

## 常用环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PANEL_BIND_ADDR` | `0.0.0.0` | 面板监听地址 |
| `PORT` | `9090` | 面板端口 |
| `DB_PATH` | `meridian.db` | SQLite 数据库路径 |
| `JWT_SECRET` | 自动生成 | 登录和敏感配置加密密钥 |
| `UPSTREAM_HEADER_KEY` | 自动生成 | 上游请求头密钥 |
| `DYNAMIC_ROUTE_KEY` | 自动生成 | 动态路由密钥 |
| `TRUSTED_PROXY_CIDRS` | 空 | 可信前置代理网段 |
| `ASSET_CACHE_DIR` | 数据库目录下 `asset-cache` | 图片缓存目录 |
| `TLS_STATE_DIR` | 数据库目录下 `tls` | Meridian 管理的 TLS 状态目录，必须使用绝对路径，并与数据库、恢复目录分开 |
| `PANEL_TLS_CERT_FILE` / `PANEL_TLS_KEY_FILE` | TLS 状态目录中的面板证书 | 可指定外部文件；恢复流程不会删除或替换 |
| `EDGE_TLS_CERT_FILE` / `EDGE_TLS_KEY_FILE` | TLS 状态目录中的边缘证书 | 可指定外部文件；节点证书状态仍保存在 `TLS_STATE_DIR` |
| `DNS_PROPAGATION_RESOLVERS` | 系统默认 resolver | 逗号分隔的 IPv4/IPv6 DNS resolver |
| `DNS_PROPAGATION_TIMEOUT` | `120s` | DNS-01 传播检查超时时间 |

## 升级和发布

正式版本、校验文件和变更说明见 [GitHub Releases](https://github.com/chanhui800/Meridian/releases)。固定版本镜像格式为 `ghcr.io/chanhui800/meridian:<版本号>`，`latest` 只指向最新成功的正式 Release。

升级 Controller 后，已注册 Agent 会通过受认证的配置接口获取匹配版本的二进制，校验平台和 SHA-256 后原子替换。普通升级不会使节点令牌失效，也不需要重新注册。只有节点令牌被主动刷新、状态文件丢失或节点更换 Controller 时，才需要运行面板生成的 `--reenroll` 安装命令。

Windows 和 macOS 构建产物目前用于开发和测试；生产部署支持 Linux Controller 与 Linux amd64/arm64 Agent。

## 开发检查

```bash
go test ./...
go test -race ./...
go vet ./...
find web/static/js -name '*.js' -print0 | xargs -0 -n1 node --check
node --test tests/*.test.js
```

项目许可证见 [LICENSE](LICENSE)，安全问题请按 [SECURITY.md](SECURITY.md) 的方式私下报告。
