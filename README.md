# Meridian

[![CI](https://github.com/chanhui800/Meridian/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/chanhui800/Meridian/actions/workflows/ci.yml)
[![CodeQL](https://github.com/chanhui800/Meridian/actions/workflows/codeql.yml/badge.svg?branch=main)](https://github.com/chanhui800/Meridian/actions/workflows/codeql.yml)
[![Release](https://img.shields.io/github/v/release/chanhui800/Meridian)](https://github.com/chanhui800/Meridian/releases/latest)
[![License](https://img.shields.io/github/license/chanhui800/Meridian)](LICENSE)

Meridian 是一个轻量的 Emby / Jellyfin 多节点反向代理面板。它把站点入口、自动回源、线路切换、流量统计、请求日志、TLS 和备份恢复放在同一个面板里，安装后即可添加站点使用。

本仓库基于 [snnabb/Meridian](https://github.com/snnabb/Meridian) 修改，界面与部分交互参考了 [CF-EMBY-PROXY-UI](https://github.com/axuitomo/CF-EMBY-PROXY-UI)。当前正式版本为 `v1.8.42`，基于正式版 `v1.8.41`。

## 这次更新了什么

`v1.8.42` 主要优化仪表盘趋势图的长浮窗交互：

- 全部站点数据较多时，浮窗优先显示在指针左右侧，避免遮挡鼠标、触摸位置、十字虚线和后方曲线。
- 取消浮窗高度限制和内部滚动，保留完整站点列表。
- 允许浮窗越过趋势卡片和图表网格边界，并保证它显示在后续的日志、定时任务与站点状态卡片之上。

## 主要功能

- 管理多个 Emby / Jellyfin 节点，站点顺序支持拖拽，并同步到仪表盘。
- 支持独立端口、路径和域名前缀三种入口方式。
- 自动识别并改写 PlaybackInfo、HLS、DASH 与 HTTP 30x 播放地址。
- 主视频流可选反代或直连；网盘服返回合法公网 30x 时可让客户端直接连接最终 CDN。
- 每个站点可配置一条主线路和最多七条备用线路，故障时顺序切换，主线路恢复后自动回切。
- 支持客户端 UA、上游 UA、自定义请求头和真实客户端 IP 透传策略。
- 提供可选单向或双向计费的流量趋势、实时网速、请求日志、最终后端地址和 Telegram 定时日报。
- 支持泛域名证书申请、自动续签、证书过期 HTTP 回退，以及密码加密的备份与恢复。
- 提供黑白主题、响应式侧栏和移动端日志布局。

## 界面预览

截图使用本地白色主题演示数据，不含生产环境信息。

| 仪表盘 | 站点管理 |
| --- | --- |
| ![仪表盘](docs/images/dashboard.png) | ![站点管理](docs/images/sites.png) |
| 请求日志 | 全局设置 |
| ![请求日志](docs/images/request-logs.png) | ![全局设置](docs/images/global-settings-ui.png) |
| Telegram 日报 | TLS 设置 |
| ![Telegram 日报](docs/images/telegram-report.png) | ![TLS 设置](docs/images/tls-certificate.png) |

## Docker 安装

新建 `docker-compose.yml`：

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

启动容器：

```bash
mkdir -p data
docker compose up -d
docker compose logs meridian
```

首次启动会随机生成并持久化登录、上游请求头、动态路由和初始化所需的密钥。请从容器日志中保存首次管理员初始化令牌，然后打开：

```text
http://服务器地址:9090
```

Meridian 在容器内以 UID `10001` 运行。程序只保留绑定低位端口所需的 `CAP_NET_BIND_SERVICE`，因此 80、443 等未被占用的端口也能直接使用。数据库、证书和缓存都保存在 `./data`，正常更新容器不会删除这些数据。

`network_mode: host` 会让面板端口和站点端口直接监听宿主机。请确认端口未被其他服务占用，并在防火墙中放行需要使用的端口。

## Linux 原生安装

交互安装：

```bash
curl -fsSL https://raw.githubusercontent.com/chanhui800/Meridian/main/install.sh | sudo bash
```

无交互安装：

```bash
curl -fsSL https://raw.githubusercontent.com/chanhui800/Meridian/main/install.sh | sudo bash -s -- install -y --no-domain
```

默认数据库位于 `/var/lib/meridian/meridian.db`。服务由 systemd 管理：

```bash
sudo systemctl status meridian --no-pager
sudo journalctl -u meridian -f
```

安装脚本会识别当前系统架构，下载对应的正式版二进制并校验 SHA-256。首次安装所需的随机密钥也会自动生成，无需手动填写。

## 添加站点

新增站点时只需要填写站点名称、入口和上游线路。自动反代默认开启，PlaybackInfo、HLS、DASH 和 HTTP 30x 中的播放地址会自动改写到当前站点入口。

动态发现只接受通过安全校验的公网地址。localhost、回环、私网、链路本地和其他保留地址不会被当作播放后端。

### 入口方式

| 入口 | 适合场景 | 示例 |
| --- | --- | --- |
| 独立端口 | 每个站点使用单独端口 | `http://server.example.com:9001` |
| 路径入口 | 多个站点共用面板域名和端口 | `https://panel.example.com:9090/movie` |
| 域名前缀 | 每个站点使用独立子域名 | `https://movie.example.com:9090` |

路径入口支持自定义名称。Meridian 会自动处理入口前缀与 Emby `/emby`、Jellyfin Base URL 的组合，避免出现重复路径。PlaybackInfo、HLS、DASH、30x、WebSocket、Cookie Path 和自定义播放路径都会沿用当前入口。

域名前缀入口需要先在“全局设置 → TLS 设置”中配置面板域名、泛域名和证书。

### 主视频流策略

- `反代`：视频继续经过 Meridian，适合统一入口、隐藏后端和使用节点限速。默认使用此模式。
- `直连`：MP4、MKV、MOV、AVI、WebM，以及 `/Videos/.../stream`、`/original`、`/download`、`/file` 等主视频请求会先访问主站。上游返回合法公网 30x 后，客户端直接连接最终 CDN。

直连只控制主视频流。普通 API、HLS / DASH、字幕、图片、海报和前端静态资源仍由 Meridian 处理。

### 多线路故障转移

每个站点可添加一条主线路和最多七条备用线路。线路名称、协议、域名和端口都能单独修改，页面上的统一测速按钮会检查全部启用线路。

主线路无法连接时，Meridian 按列表顺序选择备用线路。备用线路沿用站点现有的反代、自动发现、UA、请求头和视频流策略。主线路恢复后，新的请求会自动切回主线路。

## TLS 与自动续签

使用域名前缀或希望面板直接提供 HTTPS 时：

1. 将面板域名和泛域名解析到服务器，例如 `panel.example.com` 与 `*.example.com`。
2. 先通过 HTTP 登录面板。
3. 打开“全局设置 → TLS 设置”，填写面板前缀、泛域名和监听端口。
4. 填写 ACME 邮箱与 Cloudflare DNS API Token，申请证书。
5. 证书签发后按页面提示重启。

ACME 邮箱和 Token 会显示给已登录管理员。Token 在数据库中使用 AES-GCM 加密，仅用于申请和续签证书，不会写入程序日志或公开静态文件。自动续签依赖安装时生成的稳定 `JWT_SECRET`；使用临时密钥启动时不会保存续签凭据。

证书到期前 30 天进入续签窗口。若证书已经过期，程序会关闭 HTTPS 并通过 HTTP 重启，证书文件仍会保留。修复 DNS 或 Token 后，可在 TLS 页面重新申请并启用 HTTPS。

## 日志、流量和通知

请求日志会记录资源类别、状态、客户端 IP、客户端 UA、上游 UA、最终后端地址和时间线。日志设置分为三组：

- `资源类别写入`：决定后续哪些请求进入日志。图片海报和媒体元数据默认不写入。
- `日志字段写入`：决定新日志保存哪些字段，不会改写旧日志。
- `日志字段展示`：只控制日志表格中显示哪些列。

仪表盘显示各站点的实时下载与上传速度，并提供速度、请求和流量趋势。趋势可查看全部站点或单个站点，也能切换实时、1 小时、6 小时、1 天和 7 天范围。

流量计费模式位于“全局设置 → 系统 UI”。单向模式只计算 VPS 发给客户端的出站流量；双向模式同时计算源站进入 VPS 的流量和 VPS 发给客户端的流量。流量周期可以设为不重置，也可以选择每月 1 至 31 日重置。站点额度和面板中的已用流量都按这里的模式与周期计算。

Telegram 日报可按天或按周发送请求量、流量、站点排行和客户端分布。默认调度时区为北京时间，也可在系统 UI 设置中修改。

## 真实客户端 IP 与 UA

站点可选择以下真实客户端 IP 透传方式：

- 同时透传 `X-Real-IP` 和 `X-Forwarded-For`。
- 只保留 `X-Real-IP`。
- 不向上游透传这两个请求头。

Meridian 只会信任 `TRUSTED_PROXY_CIDRS` 中的前置代理。服务器直接暴露公网时无需设置；使用 OpenResty、Cloudflare 或其他前置代理时，再填写实际代理网段。

客户端 UA 默认原样透传，也可为单个站点选择预设 UA 或填写自定义 UA。请求日志中的“客户端 UA”和“上游 UA”会分别显示修改前后的值。

## 备份与恢复

“全局设置 → 备份与恢复”可以下载带密码加密的 `.mrbak` 文件。备份包含账户、站点、流量与请求日志、全局设置和 Telegram 配置。

TLS 设置与证书默认不勾选。需要迁移面板域名、证书、ACME 账户和自动续签凭据时，再手动选择“包含 TLS 设置与证书”。DNS API Token 会使用目标安装的密钥重新加密。

恢复前会检查密码、文件结构和 SQLite 完整性。验证通过后，Meridian 会优雅重启并原子替换数据库；启动失败时自动回滚。没有包含 TLS 的备份会保留目标服务器现有的域名、端口、TLS 开关和证书。

若目标服务器不支持备份中的入口模式，站点数据仍会保留，但入口会被清空并暂时停用。用户选择当前服务器支持的入口并保存后即可重新启用。

## 常用环境变量

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `PANEL_BIND_ADDR` | `0.0.0.0` | 面板监听地址；默认同时提供 IPv4 与 IPv6 监听 |
| `PORT` | `9090` | 首次初始化监听端口 |
| `DB_PATH` | `meridian.db` | SQLite 数据库路径 |
| `JWT_SECRET` | 自动生成 | 登录会话和敏感配置加密密钥 |
| `UPSTREAM_HEADER_KEY` | 自动生成 | 固定上游请求头加密密钥 |
| `DYNAMIC_ROUTE_KEY` | 自动生成 | 动态路由加密密钥 |
| `SETUP_TOKEN` | 自动生成 | 首次管理员初始化令牌 |
| `TRUSTED_PROXY_CIDRS` | 空 | 允许提供真实客户端 IP 的可信代理网段 |
| `ASSET_CACHE_DIR` | 数据库目录下的 `asset-cache` | 图片和静态资源缓存目录 |

## 更新

Docker：

```bash
docker compose pull
docker compose up -d --force-recreate
```

Linux 原生安装可重新运行安装脚本，然后选择“更新到最新版”。升级时会保留数据库、证书和缓存，数据库结构会在启动时自动迁移。重要节点建议先从面板下载备份。

## 常见状态码

- `206`：视频分段请求成功，播放时常见。
- `302`：上游返回重定向；直连模式会校验目标后交给客户端。
- `499`：客户端主动取消请求，常见于播放器切换连接或重新请求分段，不等同于代理故障。
- `502`：Meridian 无法连接上游，或上游连接在返回有效响应前失败，需要结合请求日志中的后端地址排查。

## 项目说明

安全策略见 [SECURITY.md](SECURITY.md)，贡献说明见 [CONTRIBUTING.md](CONTRIBUTING.md)。项目继续使用仓库中的 [LICENSE](LICENSE)。
