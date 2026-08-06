<div align="center">

# Meridian

面向 Emby 多站点场景的反向代理管理面板

自动识别播放后端 · 分钟级流量统计 · 请求日志 · 静态资源缓存

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-GHCR-2496ED?logo=docker&logoColor=white)](https://github.com/chanhui800/Meridian/pkgs/container/meridian)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

</div>

## 项目介绍

Meridian 是一个使用 Go 和原生 Web 技术开发的 Emby 反向代理管理面板。它将站点配置、自动播放后端发现、代理转发、流量统计、请求日志、静态资源缓存和运行诊断集中在同一个界面中，适合在一台服务器上管理多个 Emby 站点。

本仓库是在原 Meridian 基础上持续修改的易用版，主要目标是降低配置门槛：添加站点后尽量自动识别播放地址和后端类型，让普通用户不需要理解复杂的安全规则和反向代理配置也能开始使用。

## 修改版特点

### 自动反代

- 新建站点默认开启自动发现。
- 默认启用 HTTP 30x 和 PlaybackInfo 发现。
- HLS、DASH 等较激进的发现方式放在高级选项中，可在播放失败时按需开启。
- 默认使用 `Compatible` 兼容模式，不再要求普通用户先理解 `Safe`、`Extreme` 等安全配置。
- 默认允许 HTTPS 上游跳转到 HTTP 播放后端。
- 对动态发现的公网媒体地址生成受保护的内部路由，客户端仍通过 Meridian 播放，不直接暴露内部实现。
- 保留手动播放回源能力，方便处理无法自动识别的特殊服务端。

### 站点管理

- 支持 `host`、`port` 和 `both` 三种入口方式。
- 新建站点的 User-Agent 默认透传，保留客户端真实身份。
- 支持单个站点测速和全部站点统一测速。
- 新增站点后立即启动代理和日志记录，不需要重启容器。
- 支持主回源请求头、流量配额、限速和静态资源缓存规则。

### 管理界面

- 按照现代媒体面板样式重构 UI，桌面端采用左侧功能导航。
- 左侧导航支持展开和折叠，并记住用户选择。
- 支持明暗主题切换。
- 页面、卡片和筛选控件使用紧凑布局，适配桌面与移动设备。
- 左上角展示项目标识和版本号。

### 流量统计

- 按分钟记录每个站点的入站、出站和请求数。
- 支持最近 1 小时、6 小时、24 小时和 7 天查看。
- 7 天图表会压缩显示点数，但统计总量不会丢失。
- 旧数据库会自动迁移并保留已有小时流量记录。

### 面板请求日志

- 记录状态码、节点、客户端 IP、User-Agent、资源类型和请求路径。
- 资源分类包含页面、API、图片、字幕、视频流等类型。
- 支持按节点名称搜索；节点改名后仍可检索历史日志。
- 自动刷新不会把正在浏览的日志列表强制拉回顶部。
- IPv6、长路径和长 User-Agent 会自动换行或截断，不再遮挡页面。

### 静态资源缓存

- 缓存按真实目标域名与路径规则匹配。
- 默认规则可用于 `/file/` 和 Emby 图片资源。
- 每个节点可设置独立缓存容量上限；超出后按最久未使用顺序自动清理。
- 视频、音频、HLS、DASH 和常见媒体扩展名始终不会写入本地缓存。
- 缓存会结合响应扩展名、`Content-Type`、`Cache-Control`、`Vary` 和身份信息判断，避免缓存不应共享的内容。

## 工作原理

```text
客户端
  │
  ▼
Meridian 入口（域名或独立端口）
  ├─ 普通网页/API ──────────────► 主回源
  ├─ 30x / PlaybackInfo ─┐
  ├─ HLS / DASH（高级）──┤ 自动发现并校验播放后端
  │                      ▼
  ├─ 动态受保护路由 ────────────► 实际媒体后端
  ├─ 请求日志 ──────────────────► SQLite
  ├─ 分钟流量统计 ──────────────► SQLite
  └─ 可缓存静态资源 ────────────► 本地缓存目录
```

后端由单个 Go 程序提供 HTTP/WebSocket 代理、管理 API、SSE、站点生命周期和 SQLite 持久化；前端为嵌入二进制的 SPA，因此运行时不需要 Node.js、独立数据库或额外 Web 服务。

## Docker Compose 安装

### 1. 准备目录和密钥

```bash
mkdir -p /opt/meridian
cd /opt/meridian

openssl rand -hex 32
openssl rand -hex 32
openssl rand -hex 32
openssl rand -hex 32
```

保存四个不同的随机值，分别用于 `JWT_SECRET`、`UPSTREAM_HEADER_KEY`、`DYNAMIC_ROUTE_KEY` 和 `SETUP_TOKEN`。这些值不能相同，部署后不要随意更换。

### 2. 创建 `.env`

```dotenv
JWT_SECRET=替换为第一个随机值
UPSTREAM_HEADER_KEY=替换为第二个随机值
DYNAMIC_ROUTE_KEY=替换为第三个随机值
SETUP_TOKEN=替换为第四个随机值
```

### 3. 创建 `compose.yaml`

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
    environment:
      JWT_SECRET: ${JWT_SECRET}
      UPSTREAM_HEADER_KEY: ${UPSTREAM_HEADER_KEY}
      DYNAMIC_ROUTE_KEY: ${DYNAMIC_ROUTE_KEY}
      SETUP_TOKEN: ${SETUP_TOKEN}

volumes:
  meridian-data:
```

`9090` 是管理面板和共享域名入口，`8001-8010` 是示例站点端口范围，可按实际配置调整。若管理面板直接暴露公网，请配置防火墙和 HTTPS 反向代理。

### 4. 启动

```bash
docker compose pull
docker compose up -d
docker compose ps
```

浏览器打开：

```text
http://服务器IP:9090
```

首次进入时使用 `.env` 中的 `SETUP_TOKEN` 创建管理员。初始化完成后仍应保留原有密钥和数据卷，不要因升级而重新生成。

## 添加第一个站点

1. 进入“站点管理”，选择“添加站点”。
2. 填写节点名称和 Emby 面板地址。
3. 选择域名入口或独立端口入口。
4. 保持默认 UA 透传、自动发现和 HTTPS → HTTP 降级。
5. 保存后先执行站点测速，再使用客户端登录并播放。
6. 如果个别服务无法发现播放后端，再到高级选项中开启 HLS/DASH 或更宽松的发现来源。

普通站点不需要填写独立播放回源地址。只有自动发现确实无法覆盖的特殊服务，才建议手动配置播放回源。

## 缓存配置

缓存规则按真实目标路径匹配，也可以在规则前限制域名。默认示例：

```text
*/file/*
*/emby/Items/*/Images/*
```

即使规则命中，下列媒体内容也不会缓存：

```text
m3u8, mpd, ts, m4s, mp4, m4v, mkv, webm, mov, avi, flv,
mp3, aac, m4a, flac, ogg, opus, wav,
video/*, audio/*, application/*mpegurl*, application/dash+xml
```

“缓存大小上限（MB）”是单节点上限。写入后如果超过限制，程序会删除最久未使用的缓存文件，直到容量回到上限以内。

## 升级

升级前先备份 Compose 配置、`.env` 和数据卷。不要只备份数据库而丢失长期密钥。

```bash
cd /opt/meridian
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail=100 meridian
```

数据库结构会在启动时自动迁移。升级过程中不要删除 `meridian-data` 数据卷。

## 备份与回滚

备份数据卷示例：

```bash
docker run --rm \
  -v meridian_meridian-data:/data:ro \
  -v "$PWD":/backup \
  alpine:3.24 \
  tar czf /backup/meridian-data-backup.tar.gz -C /data .
```

回滚时把 `compose.yaml` 中的镜像标签改为之前使用的固定版本，然后重新创建容器：

```bash
docker compose pull
docker compose up -d --force-recreate
```

旧版本可能无法读取新版本迁移后的数据库，因此重要升级应同时保留升级前的数据卷备份。

## 从源码构建

```bash
git clone https://github.com/chanhui800/Meridian.git
cd Meridian
go test ./...
go build -trimpath -o meridian .
```

Docker 构建：

```bash
docker build --build-arg VERSION=dev -t meridian:dev .
```

## 主要技术实现

- 后端：Go 标准库 HTTP 服务器与反向代理。
- 数据：SQLite，自动执行兼容迁移。
- 前端：原生 HTML、CSS、JavaScript SPA，使用 Go `embed` 打包。
- 实时数据：Server-Sent Events 与定时 API 刷新。
- 播放代理：结构化解析 30x、PlaybackInfo、HLS、DASH，并通过加密 capability 路由转发动态目标。
- 安全：HttpOnly 会话 Cookie、bcrypt 密码、长期密钥分离、受信代理网段和上游地址校验。
- 容器：非 root 用户、只读根文件系统、移除 Linux capabilities，并提供健康检查。

## 使用注意

- `JWT_SECRET` 变化会让所有现有登录失效。
- `UPSTREAM_HEADER_KEY` 丢失后，已保存的加密上游请求头无法恢复。
- `DYNAMIC_ROUTE_KEY` 变化后，已生成的动态播放路由会失效。
- 真实客户端 IP 只应从明确配置的受信反向代理读取；不要将任意公网网段设为可信代理。
- 自动发现会尽量兼容不同 Emby 面板，但无法保证第三方服务端的所有私有实现都能识别。
- 请确保你有权访问并反向代理所配置的媒体服务。

## 致谢

- 原项目：[snnabb/Meridian](https://github.com/snnabb/Meridian)
- 自动反代思路参考：[Gsy-allen/emby-reverse-proxy-go](https://github.com/Gsy-allen/emby-reverse-proxy-go)
- UI 参考：[axuitomo/CF-EMBY-PROXY-UI](https://github.com/axuitomo/CF-EMBY-PROXY-UI)
- 缓存功能参考：[zxcll/emby-proxy-panel](https://github.com/zxcll/emby-proxy-panel)

## License

本项目遵循 [MIT License](LICENSE)。
