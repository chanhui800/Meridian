<div align="center">

# Meridian

基于 Go 的 Emby 反向代理管理面板

多站点代理 · 动态播放地址发现 · SQLite 持久化 · WebSocket · SSE · Docker

[![Go](https://img.shields.io/badge/Go-1.26.5-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker&logoColor=white)](Dockerfile)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

</div>

## 项目概览

Meridian 将多个 Emby 站点的入口、上游回源、播放地址处理、流量统计、请求日志和运行状态集中到一个服务中。后端是单个 Go 程序，前端静态文件通过 `embed` 编译进二进制，数据使用 SQLite 保存。

服务启动后提供管理面板端口，同时可为站点启动独立监听端口或共享 Host 入口。站点配置、代理实例和动态发现能力由同一个进程管理，进程退出时会执行流量刷盘和代理优雅停止。

## 当前分支的实际改动

下面内容对应当前分支相对基础实现已经落地的代码，不是功能规划：

### 管理界面

- 使用左侧功能导航，导航栏支持展开/折叠，并通过 `localStorage` 保存状态。
- 页面提供黑色和白色两种主题，主题选择同样会持久化。
- 桌面布局、站点卡片、筛选区和日志表格采用紧凑样式；移动端使用独立的响应式布局。
- 左上区域显示 Meridian 标识和运行版本。
- 站点管理页显示回源延迟，并支持单个站点测速和全部站点测速。

### 添加站点与自动发现默认值

数据库新站点默认值已写入 `sites` 表结构和创建逻辑：

| 配置 | 当前默认值 |
|---|---|
| User-Agent | `passthrough` |
| 动态发现 | 开启 |
| 动态策略 | `compatible` |
| 默认发现来源 | `redirect`、`playback_info` |
| HTTPS → HTTP 降级 | 允许 |
| 播放回源 | 默认不要求手动填写 |

HLS 和 DASH 仍是受策略控制的发现来源；策略目录、来源校验、域名规则和动态 capability 校验均由后端执行。相对路径、无 scheme 的播放地址和部分重定向响应也在自动播放处理链中归一化。

### 面板请求日志

- 增加独立的请求日志页面和筛选接口。
- 资源分类包含视频请求；后端记录 `resource_category=video`。
- 节点搜索同时匹配当前站点名称和历史日志中的站点名称。
- 日志写入使用异步队列，不让媒体请求等待数据库写入；新增站点后立即接入同一日志管线。
- 页面自动刷新时保留当前筛选和滚动上下文，IPv6 地址和长字段使用专门的布局规则显示。

### 分钟流量统计

- `traffic_logs` 增加 `requests` 字段，并把站点流量按本地分钟聚合。
- 流量页提供最近 1 小时、6 小时、24 小时和 7 天四个时间范围。
- 图表同时展示入站、出站和请求数；长时间范围会压缩图表点数，但先完成分钟聚合再压缩，不改变总量。
- API 的流量快照包含实时 pending 数据、请求数和 `recorded_at_ms`，用于避免本地时区被浏览器误解释。

### 静态资源缓存

当前分支新增 `asset_cache.go` 和对应测试，实现按站点隔离的静态资源缓存。缓存上限、TTL、规则和启用状态都属于站点配置；缓存目录使用 `os.Root` 限定在缓存根目录内。媒体类型和媒体扩展名过滤规则会阻止视频、音频、HLS、DASH 等内容进入缓存。

## 源码中实现的功能

### 站点与入口

- 多站点配置，每个站点拥有名称、监听端口、公共 Host、主回源地址和播放回源配置。
- 入口模式支持独立端口、共享 Host，以及两者同时启用。
- 共享 Host 入口按请求 Host 选择站点；管理面板域名可单独配置。
- 站点可以启用/停用、更新和删除，运行中的代理实例由 `ProxyManager` 负责生命周期管理。
- 支持 HTTP、WebSocket、上传和长时间媒体响应；面板服务器与站点监听器分别管理。

### 动态播放后端发现

源码定义了四类发现来源：

- `redirect`：解析 HTTP 30x 的 `Location`。
- `playback_info`：解析 Emby PlaybackInfo 响应中的播放地址和相关字段。
- `hls`：处理 HLS 清单和媒体地址。
- `dash`：处理 DASH 清单和媒体地址。

动态发现使用 `safe`、`compatible`、`extreme` 三种策略，策略会限制可用发现来源、域名规则、端口、跳转次数、请求速率和并发容量。发现到的动态目标通过 `/\_meridian/d/` capability 路由访问，capability 使用独立密钥签名，并带有版本、站点、策略版本和过期时间校验。

### UA 与上游请求头

- UA 模式包含 `infuse`、`web`、`client`、`custom` 和 `passthrough`。
- 自定义模式同时保存 User-Agent、Client 和 Version，并执行长度和字符校验。
- 上游固定请求头使用独立密钥加密保存，API 只返回脱敏视图，不回显明文值。
- 上游请求头绑定目标 authority，避免将主回源的敏感头发送到其他播放域名。

### 流量与请求日志

- 每个站点统计入站字节、出站字节、请求数、限速和配额使用量。
- 流量按本地分钟写入 `traffic_logs`，后台周期性刷盘，进程退出时执行最终刷盘。
- 请求日志记录方法、路径、状态码、响应大小、资源分类、站点、客户端地址和 User-Agent。
- 日志接口支持日期、资源类别、状态码、节点名称、客户端 IP、UA 和路径筛选，并支持分页、清理和 SSE 仪表盘更新。

### 静态资源缓存

`asset_cache.go` 实现了按站点隔离的本地缓存：

- 规则按真实目标的 Host 与路径匹配，支持 `*` 通配符。
- 缓存键包含站点、目标 URL、Accept、Accept-Encoding 和认证身份摘要。
- 响应必须是可缓存的 200 响应，并通过 `Content-Type`、`Cache-Control`、`Vary`、Cookie 和大小检查。
- 视频、音频、HLS、DASH 和常见媒体扩展名被排除。
- 每个站点按容量上限清理最久未访问的缓存文件。
- 文件操作使用 Go `os.Root` 限定在缓存根目录内，避免路径越界和目录竞态。

### 认证与安全边界

- 单管理员模式，首次启动通过 `SETUP_TOKEN` 创建管理员。
- 密码使用 bcrypt 保存，登录使用 HttpOnly 会话 Cookie 和 JWT 签名。
- 登录和初始化接口带有失败限流。
- 修改状态的请求需要同源校验，响应包含安全 Header。
- `TRUSTED_PROXY_CIDRS` 控制哪些代理可以提供客户端 IP 和转发协议。
- `PANEL_DOMAIN` 可限制管理面板允许的 Host。
- `JWT_SECRET`、`UPSTREAM_HEADER_KEY`、`DYNAMIC_ROUTE_KEY` 分离使用；动态路由密钥不能与其他密钥相同。

### 诊断与运行接口

源码提供管理 API、健康检查和运行能力查询，包括：

```text
GET  /api/auth/check
POST /api/auth/setup
POST /api/auth/login
POST /api/auth/logout
GET  /api/dashboard
GET  /api/sites
GET  /api/traffic/{siteID}
GET  /api/request-logs
GET  /api/ingress-capabilities
GET  /api/dynamic-profiles
GET  /api/ua-profiles
GET  /api/events
```

`/api/events` 使用 Server-Sent Events 推送仪表盘流量快照。容器健康检查访问 `/api/auth/check`。

## Linux 安装

Linux 安装脚本从 GitHub Releases 下载对应架构的二进制和 `SHA256SUMS`，校验通过后安装 systemd 服务。脚本支持 Debian/Ubuntu、RHEL 系、Alpine、Arch 等常见发行版的包管理器，并可选配置 Nginx + Certbot 作为管理面板 HTTPS 入口。

### 一键安装

脚本默认从 `chanhui800/Meridian` 获取 Release。使用其他仓库时，通过 `MERIDIAN_REPO` 指定 `owner/repository`：

```bash
curl -fsSL https://raw.githubusercontent.com/chanhui800/Meridian/main/install.sh \
  -o /tmp/meridian-install.sh

bash /tmp/meridian-install.sh install
```

也可以使用非交互方式安装，并配置面板域名：

```bash
bash /tmp/meridian-install.sh install \
  --domain panel.example.com \
  --email admin@example.com \
  -y
```

不配置域名、直接使用服务器 IP：

```bash
bash /tmp/meridian-install.sh install --no-domain -y
```

安装器默认路径：

| 内容 | 默认路径 |
|---|---|
| 二进制 | `/usr/local/bin/meridian` |
| 环境配置 | `/opt/meridian/.env` |
| SQLite 数据库 | `/opt/meridian/meridian.db` |
| 备份 | `/opt/meridian-backups` |
| systemd 服务 | `/etc/systemd/system/meridian.service` |
| Nginx 配置 | `/etc/nginx/conf.d/meridian-panel.conf` |

首次安装完成后，脚本会显示初始化令牌。使用它在面板中创建管理员；令牌和环境密钥应妥善保存。

### Linux 运维命令

```bash
# 更新到最新 Release；脚本会备份、健康检查，失败时尝试回滚
bash /tmp/meridian-install.sh update -y

# 修改管理员密码，并轮换 JWT_SECRET
bash /tmp/meridian-install.sh password

# 卸载程序但保留数据和备份
bash /tmp/meridian-install.sh uninstall -y

# 卸载并删除数据目录；备份目录仍保留
bash /tmp/meridian-install.sh uninstall --purge -y
```

### 从源码运行

```bash
git clone https://github.com/chanhui800/Meridian.git
cd Meridian

go test ./...
go build -trimpath -o meridian .

export JWT_SECRET="$(openssl rand -hex 32)"
export UPSTREAM_HEADER_KEY="$(openssl rand -hex 32)"
export DYNAMIC_ROUTE_KEY="$(openssl rand -hex 32)"
export SETUP_TOKEN="$(openssl rand -hex 32)"
export PORT=9090
export DB_PATH="$PWD/meridian.db"

./meridian
```

可用 `./meridian --version` 查看版本，也可使用管理员离线改密令：

```bash
printf '%s\n' '新的管理员密码' \
  | ./meridian admin reset-password \
      --db /path/to/meridian.db \
      --password-stdin
```

## Docker Compose 安装

### 1. 创建目录和密钥

```bash
mkdir -p /opt/meridian
cd /opt/meridian

cat > .env <<'EOF'
JWT_SECRET=替换为至少32字节的随机值
UPSTREAM_HEADER_KEY=替换为不同的随机值
DYNAMIC_ROUTE_KEY=替换为第三个不同的随机值
SETUP_TOKEN=替换为首次初始化令牌
EOF
```

生产环境建议用 `openssl rand -hex 32` 生成四个不同的值，不要把 `.env` 提交到 Git。

### 2. 创建 `compose.yaml`

```yaml
services:
  meridian:
    image: ghcr.io/chanhui800/meridian:latest
    container_name: meridian
    restart: unless-stopped
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=16m
    ulimits:
      nofile:
        soft: 65536
        hard: 65536
    ports:
      - "9090:9090"
      - "8001-8010:8001-8010"
    volumes:
      - meridian-data:/app/data
    env_file:
      - .env

volumes:
  meridian-data:
```

`9090` 是面板端口；`8001-8010` 只是示例站点端口范围，实际暴露端口应与站点配置一致。若只通过 Nginx、Cloudflare 或其他入口访问面板，可将 `9090` 绑定为 `127.0.0.1:9090:9090`。

### 3. 启动和检查

```bash
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail=100 meridian
```

面板默认地址：

```text
http://服务器IP:9090
```

需要由面板直接终止 TLS 时，可让同一个端口监听 HTTPS。建议先准备一个专用的面板域名和一个泛域名，例如：

```text
面板域名：panel.abc.com
站点泛域名：*.abc.com
```

在 `.env` 中配置基础域名、面板域名和 TLS：

```env
PANEL_TLS_ENABLED=true
PANEL_DOMAIN=panel.abc.com
PANEL_ROUTE_DOMAIN=abc.com
```

证书路径默认位于数据目录：`/app/data/tls/fullchain.pem` 和 `/app/data/tls/privkey.pem`。Docker Compose 中的 `meridian-data:/app/data` 必须保持可写；如果使用外部 ACME 证书，也可以通过 `PANEL_TLS_CERT_FILE` 和 `PANEL_TLS_KEY_FILE` 指向挂载进容器的 PEM 文件。

域名前缀配置步骤：

1. 在 DNS 中将 `panel.abc.com` 和 `*.abc.com` 指向面板服务器。Cloudflare 使用橙云代理时，9090 不是通用 HTTPS 代理端口，建议使用 DNS-only、Cloudflare Tunnel 或受支持的 HTTPS 端口。
2. 在站点管理页打开“TLS 证书”，使用 Cloudflare DNS-01 申请通配符证书。DNS API Token 只用于当前申请，不会保存。
3. 证书申请成功后设置 `PANEL_TLS_ENABLED=true` 并重启容器。不要在证书文件尚未生成时启用 TLS，否则面板无法启动。
4. 新增或编辑站点时选择“域名前缀”，填写 `123` 等前缀，面板会生成 `https://123.abc.com:9090`。面板自身通过 `https://panel.abc.com:9090` 访问，未配置的前缀会被拒绝。

站点管理卡片中的“访问地址”默认隐藏。点击眼睛按钮显示地址，点击复制按钮即可复制完整地址；复制不需要先显示地址。

首次启动时，使用 `SETUP_TOKEN` 创建唯一管理员。数据保存在 `meridian-data` 卷中，升级时不要删除该卷。

### Docker 固定版本

生产环境建议使用固定标签，而不是 `latest`：

```yaml
image: ghcr.io/chanhui800/meridian:vX.Y.Z
```

升级步骤：

```bash
docker compose pull
docker compose up -d --force-recreate
docker compose ps
```

## 域名前缀 + 独立端口方案

该方案让 Meridian 只监听一个独立端口（默认 `9090`），再通过不同的域名前缀把请求分配给不同站点。面板和节点共用同一个 TLS 监听器，因此不需要占用通常已被其他业务使用的 `443` 端口。

### 访问模型

```text
面板： https://panel.example.com:9090
节点： https://123.example.com:9090
       └── 123 是站点管理中填写的域名前缀
```

请求到达 `9090` 后，Meridian 会先识别 Host：面板域名进入管理界面，节点域名前缀进入对应的站点代理；未配置的前缀会被拒绝。每个前缀必须唯一，建议不要把 `panel` 作为节点前缀使用。

### 配置教程

1. 在 DNS 中添加 `panel.example.com` 和 `*.example.com`，都指向 Meridian 所在服务器。若使用 Cloudflare 橙云代理，请确认该端口受代理支持；否则使用 DNS-only、Cloudflare Tunnel 或其他能转发 HTTPS `9090` 的入口。
2. 在面板的 TLS 设置中申请包含面板域名和泛域名的证书，例如 `panel.example.com` 与 `*.example.com`。申请证书需要可写的数据目录，Docker 必须保留 `/app/data` 卷，Linux 安装必须保留 `/opt/meridian` 数据目录。
3. 在环境变量中配置：

   ```env
   PORT=9090
   PANEL_DOMAIN=panel.example.com
   PANEL_ROUTE_DOMAIN=example.com
   PANEL_TLS_ENABLED=true
   ```

4. 重启服务后，在“站点管理 → 添加站点”中选择“域名前缀”入口模式，填写 `123` 等前缀并保存。卡片中的访问地址会显示为 `https://123.example.com:9090`，可直接复制到 Emby 客户端。

### 使用注意

- `PANEL_ROUTE_DOMAIN` 只填写基础域名，不要填写协议、端口或通配符，例如填写 `example.com`，不要填写 `https://*.example.com:9090`。
- 证书必须覆盖节点前缀；推荐使用 DNS-01 申请泛域名证书。证书文件和 ACME 账户密钥都保存在数据目录中，升级或重建容器时不要删除数据卷。
- `9090` 是 HTTPS 监听端口，客户端地址必须带 `https://` 和 `:9090`。若前面还有反向代理，请让它透传 Host，否则 Meridian 无法判断目标节点。
- 面板始终使用 `PANEL_DOMAIN` 访问；节点使用“前缀 + `PANEL_ROUTE_DOMAIN`”访问。这样即使服务器的 `443` 已被 Nginx、OpenResty 或其他业务占用，也不会冲突。
- 修改基础域名、证书或入口模式后，需要重启容器/服务让监听器重新加载配置；站点数据本身不会丢失。

## 配置项

| 环境变量 | 默认值 | 作用 |
|---|---|---|
| `PORT` | `9090` | 管理面板监听端口 |
| `DB_PATH` | `meridian.db` | SQLite 数据库路径 |
| `PANEL_BIND_ADDR` | `0.0.0.0` | 面板监听 IP |
| `PANEL_DOMAIN` | 空 | 管理面板允许的公共域名 |
| `PANEL_ROUTE_DOMAIN` | 空 | 域名前缀入口的基础域名，例如 `abc.com` |
| `PANEL_TLS_ENABLED` | `false` | 是否让面板端口直接监听 HTTPS |
| `PANEL_TLS_CERT_FILE` | 数据库目录下 `tls/fullchain.pem` | 面板 TLS 证书或证书链 PEM 文件 |
| `PANEL_TLS_KEY_FILE` | 数据库目录下 `tls/privkey.pem` | 面板 TLS 私钥 PEM 文件 |
| `JWT_SECRET` | 启动时临时生成 | JWT 签名密钥；生产环境应固定设置 |
| `UPSTREAM_HEADER_KEY` | 空 | 上游固定请求头加密密钥 |
| `DYNAMIC_ROUTE_KEY` | 空 | 动态播放 capability 密钥 |
| `SETUP_TOKEN` | 空 | 首次管理员初始化令牌 |
| `TRUSTED_PROXY_CIDRS` | 空 | 可信反向代理 CIDR，影响客户端 IP/协议读取 |
| `ASSET_CACHE_DIR` | 数据库目录下 `asset-cache` | 静态资源缓存根目录 |

`JWT_SECRET`、`UPSTREAM_HEADER_KEY` 和 `DYNAMIC_ROUTE_KEY` 应使用不同的随机值。密钥丢失或更换可能导致会话、已保存上游请求头或动态播放 capability 失效。

## 数据和备份

SQLite 数据库保存站点、管理员、流量和请求日志。默认 Linux 安装会在更新和改密操作前创建备份；Docker 用户需要自行备份卷：

```bash
docker run --rm \
  -v meridian_meridian-data:/data:ro \
  -v "$PWD":/backup \
  alpine:3.24 \
  tar czf /backup/meridian-data-$(date +%Y%m%d-%H%M%S).tar.gz -C /data .
```

恢复前停止服务，将备份内容解压回数据卷，再执行 `docker compose up -d` 或启动 systemd 服务。

## 构建与发布

本地构建：

```bash
go test ./...
go build -trimpath -buildvcs=false -o meridian .
docker build --build-arg VERSION=dev -t meridian:dev .
```

Release 工作流会执行 Go 测试、`go vet`、前端 JavaScript 语法检查、Shell 检查、漏洞扫描和安全检查，然后构建 Linux、Windows、macOS 二进制，并在版本标签下推送 Linux amd64/arm64 镜像到 GHCR。

## License

本项目使用 [MIT License](LICENSE)。
