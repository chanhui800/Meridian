# CodeQL 告警复核与安全边界

本次复核针对 2026-11-20 的 CodeQL 分析 `2214073439`，基线为 `v1.11.5` / `5bb77162a411f04660ec933601324cc72135ab50`。扫描成功与没有告警是两个不同结果；判断告警需要结合入口权限、实际值流和响应策略。表内“误报”只针对列出的实现与数据流，保护措施变化后需要重新审查。

## 逐项结论

| 告警 | 实际行为 | 处理 |
| --- | --- | --- |
| [#15：SSRF](https://github.com/chanhui800/Meridian/security/code-scanning/15) | 管理员通过“测试回源”或已保存站点诊断发起 HTTP(S) 请求。认证、同源检查有效，但原客户端允许访问云元数据等特殊地址，也会继承环境 HTTP 代理。 | 保留功能，加固诊断拨号与资源使用。拒绝元数据、链路本地、未指定与多播等特殊目标；验证解析地址后直接拨号，限制任意 URL 测试的并发与频率。正常内网回源继续可用。 |
| [#9：反射 XSS](https://github.com/chanhui800/Meridian/security/code-scanning/9) | 媒体清单中的 URL 经结构化重写后进入响应体。写出前强制 CSP 沙箱、`nosniff` 与 `DENY`，不接受上游宽松 CSP/管理 Cookie；资源响应拒绝 HTML、SVG、JavaScript 等活跃类型。 | 未复现可利用 XSS。保留原字节与响应隔离，用回归测试固定边界；不对媒体流做 HTML 转义。 |
| [#10：忽略证书校验](https://github.com/chanhui800/Meridian/security/code-scanning/10) | 只用于本机 `--healthcheck` 的固定 loopback 地址与端口标记，禁用跳转、不携带凭据。面板证书通常不包含 loopback 名称；业务回源不使用该客户端。 | 记录范围明确的本机健康检查例外，不更改正常业务 TLS 验证。 |
| [#16：SHA256 处理敏感数据](https://github.com/chanhui800/Meridian/security/code-scanning/16) | token/device/匿名标识生成保活计数的临时内存 map 键，不是登录密码存储。map 有容量和闲置过期限制，摘要不写入数据库。管理员密码实际使用 bcrypt。 | 不改成慢密码哈希，也不宣称摘要是匿名化保证。保留用途和数据生命周期说明。 |
| [#11](https://github.com/chanhui800/Meridian/security/code-scanning/11)、[#12：跳转检查](https://github.com/chanhui800/Meridian/security/code-scanning/12) | 路径只用于学习表和短时播放资格判断。SARIF 跨结构字段将其传播到输出大小和响应 Location，实际没有对应目标值流。 | 按误报复核；测试路径学习不修改请求或跳转的 Host/Scheme。 |
| [#13](https://github.com/chanhui800/Meridian/security/code-scanning/13)、[#14：跳转检查](https://github.com/chanhui800/Meridian/security/code-scanning/14) | 实际能力路由由固定 `/_meridian/d/` 和密封 token 构成；站点前缀只在已解析 URL 的 Path 中改写，有效入口前缀非空。 | 当前约束下未复现开放重定向，不为消提示改变路径语义。上游本身返回的外部 Location 按反代契约透传。 |
| [#7](https://github.com/chanhui800/Meridian/security/code-scanning/7)、[#8：Cookie Secure](https://github.com/chanhui800/Meridian/security/code-scanning/8) | Cookie 根据真实 TLS 或可信代理的 HTTPS 状态设置 `Secure`；始终启用 `HttpOnly` 与 `SameSite=Strict`。只有显式 HTTP 兼容部署不设置 Secure。 | HTTPS 部署的提示不是缺失保护；HTTP 部署存在明文会话风险，生产管理入口应使用 HTTPS。不为消提示破坏已有 HTTP 兼容模式。 |

## 诊断接口的权限范围

`/api/upstream-test` 仍需要有效管理员会话和严格同源 POST；已保存站点诊断也需要管理员认证。它们不会复制入站 Cookie、Bearer 或站点自定义凭据，即时测试也不返回原始上游响应体。HTTP 客户端正常验证证书，拒绝跨协议、主机或端口的跳转，并限制超时与响应读取。

访问正常 loopback、RFC1918 和 IPv6 ULA 回源属于管理员权限。特殊目标策略用于防止诊断误触云元数据等服务，不意味着所有内部服务都安全，或已限制拥有管理员权限的人配置业务反代。管理会话、服务器网络和容器出口仍应按实际部署保护。

任意 URL 测试每个 Controller 应用实例最多同时执行 4 个请求，允许 24 个请求的突发额度、每秒恢复 1 个额度；超过限制返回 `429` 与 `Retry-After`。普通线路逐个测试、站点列表延迟、节点调度和自动故障切换保持各自功能。审计日志只记录目标主机及结果，不记录 URL 查询、请求凭据或上游响应体。

## 审阅与发布

高风险结果应逐项处理：有效问题修代码；有明确安全边界的结果记录规则、指纹、边界文件与验证依据。不要仅加 `nosec`、按整条规则屏蔽，或把所有历史告警一次性关闭。

审阅记录必须随源码变化复核。若放宽 CSP、扩大可用响应类型、改变 capability 授权、放宽转发头信任、变更健康检查目标，或把临时身份键用于持久密码验证，不能继续沿用本次结论。

门禁只接受精确结果的原始 CodeQL 指纹，记录的保护实现与测试必须保持匹配，有效期最长 90 天。未审阅的严重或高危结果会阻止检查通过；中低风险仍保留在报告中。正式发布还需确认当前标签提交的主分支 CodeQL 已成功执行，并重新核对该提交的扫描结果。具体记录格式、单维护者更新方式和信任前提见[门禁政策](../.github/codeql-review-policy.md)。本文件中的复核不会自动关闭 GitHub 告警。
