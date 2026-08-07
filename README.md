# Meridian（基于原项目的修改版）

本仓库是基于原项目 [snnabb/Meridian](https://github.com/snnabb/Meridian) 的修改版，保留原项目的核心反向代理能力，并针对实际部署补充和调整了域名前缀入口、自动后端改写、TLS 面板配置、缓存、日志和 UI。当前修改版仓库地址为 [chanhui800/Meridian](https://github.com/chanhui800/Meridian)。

Meridian 是面向 Emby 及兼容媒体服务的反向代理面板。它把多个上游站点统一到一个管理界面，提供独立端口、域名前缀入口、动态后端发现、播放地址改写、TLS、流量统计、请求日志和静态资源缓存。

当前发布版本：`v1.8.15`

## 主要特点

- 面板管理多个节点：新增、编辑、启停、延迟测试和运行诊断。
- 两种入口：独立端口，或 `节点前缀.泛域名:面板端口` 的域名前缀入口。
- 自动发现后端并改写 PlaybackInfo、HLS、DASH 和 HTTP 30x 中的播放地址，使后端切换后仍经过本站点代理。
- 默认动态发现策略为 `compatible`，默认来源为 HTTP 30x 和 PlaybackInfo；HTTPS → HTTP 降级默认允许。
- 对 localhost、回环、私网、链路本地和其他特殊目标执行拒绝，防止动态发现绕过目标安全边界。
- 媒体认证头默认完全透传；UA 默认透传，可按站点覆盖。
- 面板日志支持站点、资源分类、状态码、客户端 IP、UA、方法和路径检索，并可清理日志与缓存。
- 每个节点独立配置缓存上限，按真实目标路径匹配并按最旧使用时间淘汰；视频流、音频流、HLS/DASH 清单和分片不会写入静态缓存。
- TLS 页面直接申请泛域名证书，证书申请后可在页面点击“启用 HTTPS 并重启”。
- 仪表盘实时显示当前完整面板访问地址。
- Docker 和 Linux systemd 均支持健康检查、优雅停止和自动重启。

## 快速开始

### Docker

```yaml
services:
  meridian:
    image: ghcr.io/chanhui800/meridian:v1.8.15
    container_name: meridian
    restart: unless-stopped
    # Linux Docker 使用宿主机网络，面板设置的监听端口直接绑定宿主机
    network_mode: host
    volumes:
      - ./data:/app/data
    environment:
      PORT: "9090"
      DB_PATH: /app/data/meridian.db
      JWT_SECRET: change-this-to-a-long-random-secret
```

```bash
mkdir -p data
docker compose up -d
docker compose logs -f meridian
```

首次打开 `http://服务器地址:9090` 完成管理员初始化。之后在 TLS 页面修改监听端口并重启，容器会直接在宿主机的新端口监听。`network_mode: host` 适用于 Linux Docker；它不使用 `ports` 映射，需在宿主机防火墙中放行所选端口。数据目录必须可写，并应限制为仅运行用户可读。

### Linux 原生 systemd

安装脚本会从 GitHub Releases 下载对应架构的二进制并校验 `SHA256SUMS`：

```bash
curl -fsSL https://raw.githubusercontent.com/chanhui800/Meridian/main/install.sh | sudo bash
sudo systemctl status meridian
sudo journalctl -u meridian -f
```

原生安装默认使用 `/var/lib/meridian/meridian.db`，服务文件位于 `/etc/systemd/system/meridian.service`。首次启动后访问 `http://服务器地址:9090`。

## 域名前缀入口与 TLS

域名前缀方案不要求单独占用 443 端口。面板监听 9090 时，节点地址可以是：

```text
https://movie.example.com:9090
```

其中 `movie` 是节点前缀，`*.example.com` 是节点泛域名。面板本身使用单独的子域名前缀，例如 `panel.example.com:9090`。

### 配置步骤

1. 在 DNS 中将 `panel.example.com` 和 `*.example.com` 解析到 Meridian 服务器。
2. 先用 HTTP 登录面板，进入“TLS 证书”。
3. 填写“面板访问域名前缀”（只填 `panel`）、“节点泛域名”（填写 `*.example.com`）和面板监听端口（默认 `9090`）。面板完整域名会自动拼接为 `panel.example.com`。
4. 点击“保存设置”。保存前缀不会申请证书；修改监听端口会记录为待重启。
5. 首次申请或泛域名发生变化时，填写 ACME 邮箱、DNS 服务商和 DNS API Token，点击“申请证书”。面板前缀变化不会触发证书申请。
6. 证书签发成功或监听端口改变后，点击 TLS 页面中的重启按钮。重启会短暂中断面板和节点连接，Docker/systemd 会自动拉起新进程。
7. 在站点管理中将入口模式设为“域名前缀”，为每个站点填写唯一的公开域名，例如 `movie.example.com`。

ACME 证书订单只申请 `*.example.com` 泛域名，不会重复提交 `panel.example.com`；泛域名证书可同时覆盖面板和一层节点域名。面板完整域名由“面板访问域名前缀”与泛域名自动拼接。域名和 TLS 状态保存在数据库中，后续不需要再修改 `.env`。旧版本环境中的 `PANEL_DOMAIN`、`PANEL_ROUTE_DOMAIN`、`PANEL_TLS_ENABLED` 只会在数据库尚未配置时导入一次，之后以面板设置为准。

注意事项：

- 泛域名 DNS 必须指向同一台 Meridian；Cloudflare 代理状态和防火墙规则应允许 9090 端口。
- DNS API Token 只用于当前申请，不保存到数据库、证书文件或日志。
- 修改泛域名会自动迁移已有的单层节点前缀；域名冲突会在保存前拒绝。
- 证书申请需要服务器能够访问 ACME 和 DNS 服务；测试环境证书不应投入生产。

## 站点与动态发现

站点可以使用独立端口、域名前缀或兼容两者的入口。动态发现按真实目标路径处理，默认仅启用：

```text
HTTP 30x
PlaybackInfo
```

HLS、DASH、播放地址和重定向中的后端地址会被重新指向当前 Meridian 入口。对于只有 Extreme 模式才能正确抓取的上游，可在站点高级选项中切换策略；常规用户保持 `compatible` 即可。

认证头采用完全透传：`Authorization`、`X-Emby-Authorization` 和 `X-MediaBrowser-Authorization` 不会被面板改写。`X-Real-IP` 和 `X-Forwarded-For` 按可信代理配置生成或透传；只有明确配置的可信代理才会被采纳为前级身份来源。

## 缓存

“缓存大小上限（MB）”是单节点上限。每次写入后，如果节点缓存总量超过上限，会删除最旧使用的缓存文件直到回到上限以内。

缓存规则按真实目标路径匹配，也支持在前面加域名限制。默认规则：

```text
*/file/*
*/emby/Items/*/Images/*
```

程序还会检查扩展名和 Content-Type。m3u8、mpd、ts、m4s、mp4、mkv、webm、音频文件、`video/*`、`audio/*`、HLS/DASH 类型等不会写入静态缓存。

## 环境变量

大多数用户只需设置数据库、端口和 JWT 密钥；域名和 TLS 在面板配置。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PANEL_BIND_ADDR` | `0.0.0.0` | 监听地址 |
| `PORT` | `9090` | 首次初始化时的默认面板监听端口；之后以 TLS 页面设置为准 |
| `DB_PATH` | `meridian.db` | SQLite 数据库路径 |
| `JWT_SECRET` | 无 | 建议设置为长度足够的随机字符串 |
| `TRUSTED_PROXY_CIDRS` | 空 | 允许读取前级转发身份的 CIDR 列表 |
| `ASSET_CACHE_DIR` | 数据库目录下 `asset-cache` | 静态缓存目录 |
| `PANEL_TLS_CERT_FILE` | 数据目录 `tls/fullchain.pem` | 可选的外部证书链路径 |
| `PANEL_TLS_KEY_FILE` | 数据目录 `tls/privkey.pem` | 可选的外部私钥路径 |

旧环境变量 `PANEL_DOMAIN`、`PANEL_ROUTE_DOMAIN`、`PANEL_TLS_ENABLED` 仅用于首次导入，不建议新部署继续依赖它们。

## 更新与回滚

正式发布使用无后缀版本标签，例如：

```bash
docker compose pull
docker compose up -d --force-recreate
```

原生 systemd 更新：

```bash
sudo systemctl stop meridian
# 下载并校验目标版本二进制后替换 /usr/local/bin/meridian
sudo systemctl start meridian
```

升级前请备份数据库和 `data/tls` 目录。数据库迁移在启动时自动执行；如果健康检查失败，应先查看容器日志或 `journalctl -u meridian`，确认数据目录权限和证书文件是否完整。

## 开发与测试

```bash
go test ./...
go vet ./...
git diff --check
```

Windows 上个别依赖端口占用时序的动态启动测试可能受操作系统调度影响；Linux Runner 是发布验证环境。

## 许可证

本项目沿用仓库中的许可证文件，详见 [LICENSE](LICENSE)。
