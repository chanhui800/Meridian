// Sites management page
function renderSites() {
  const page = document.getElementById('page-sites');
  page.innerHTML = `
    <h1 class="section-title fade-up">站点管理</h1>
    <p class="section-sub fade-up stagger-1">管理所有 Emby 反代站点与双上游配置</p>
    <div class="page-toolbar fade-up stagger-1">
      <div class="toolbar-info" id="sites-count"></div>
      <button class="btn-add" id="btn-add-site">
        <svg viewBox="0 0 24 24"><line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/></svg>
        添加站点
      </button>
    </div>
    <div class="sites-grid" id="sites-grid"></div>
  `;

  document.getElementById('btn-add-site').onclick = () => showSiteModal();
  loadSites();
}

async function loadSites() {
  try {
    const sites = await API.listSites();
    document.getElementById('sites-count').innerHTML = `共 <strong>${sites.length}</strong> 个站点`;

    const grid = document.getElementById('sites-grid');
    if (!sites || sites.length === 0) {
      grid.innerHTML = '<div style="text-align:center;color:var(--white-38);padding:60px;grid-column:1/-1">暂无站点，点击右上角添加</div>';
      return;
    }

	grid.innerHTML = sites.map((s, i) => {
      const pct = s.traffic_quota > 0 ? (s.traffic_used / s.traffic_quota * 100).toFixed(1) : 0;
      const pctClass = pct > 85 ? 'danger' : pct > 50 ? 'warn' : 'normal';
		const playbackRow = renderPlaybackRow(s);
		const upstreamHeaderCount = Array.isArray(s.upstream_headers) ? s.upstream_headers.length : 0;
		const ingressRows = renderIngressSummary(s);

      return `
      <div class="site-card fade-up stagger-${Math.min(i + 1, 6)}">
        <div class="site-top">
          <div class="site-name">${esc(s.name)}</div>
          <span class="status-badge">
            <span class="status-led ${s.running ? 'on' : 'off'}"></span>
            ${s.running ? '运行中' : '已停止'}
          </span>
        </div>
        <div class="site-rows">
          <div class="site-row">
            <span class="site-row-label">主回源地址</span>
            <span class="mono">${esc(s.target_url)}</span>
          </div>
          ${playbackRow}
		  ${ingressRows}
		  ${upstreamHeaderCount > 0 ? `
		  <div class="site-row">
			<span class="site-row-label">上游请求头</span>
			<span>${upstreamHeaderCount} 个（加密）</span>
		  </div>` : ''}
          <div class="site-row">
            <span class="site-row-label">UA 模式</span>
            <span class="pill ${uaClassMap[s.ua_mode] || 'pill-blue'}">${esc(uaNameMap[s.ua_mode] || s.ua_mode)}</span>
          </div>
          ${s.traffic_quota > 0 ? `
          <div class="progress-wrap">
            <div class="progress-labels">
              <span>已用 ${formatBytes(s.traffic_used)}</span>
              <span>${formatBytes(s.traffic_quota)}</span>
            </div>
            <div class="progress-track">
              <div class="progress-fill ${pctClass}" style="width:${Math.min(pct, 100)}%"></div>
            </div>
          </div>
          ` : `
          <div class="site-row">
            <span class="site-row-label">已用流量</span>
            <span>${formatBytes(s.traffic_used)}</span>
          </div>
          `}
        </div>
        <div class="site-actions">
          <button class="btn-ghost" data-site-action="toggle" data-site-id="${s.id}">${s.enabled ? '停用' : '启用'}</button>
          <button class="btn-ghost" data-site-action="edit" data-site-id="${s.id}">编辑</button>
          <button class="btn-ghost danger" data-site-action="delete" data-site-id="${s.id}">删除</button>
        </div>
      </div>`;
    }).join('');

    const sitesById = new Map(sites.map(site => [site.id, site]));
    grid.querySelectorAll('[data-site-action]').forEach(button => {
      button.addEventListener('click', () => {
        const id = Number(button.dataset.siteId);
        const site = sitesById.get(id);
        if (!site) return;
        if (button.dataset.siteAction === 'toggle') toggleSiteAction(id);
        if (button.dataset.siteAction === 'edit') showSiteModal(site);
        if (button.dataset.siteAction === 'delete') deleteSiteAction(id, site.name);
      });
    });
  } catch (e) {
    Toast.error('加载站点失败: ' + e.message);
  }
}

function renderPlaybackRow(site) {
  const playback = (site.playback_target_url || '').trim();
  const extraHosts = normalizeStreamHosts(site.stream_hosts);
  const totalHosts = (playback ? 1 : 0) + extraHosts.length;

  if (totalHosts === 0) {
    return `
      <div class="site-row">
        <span class="site-row-label">播放回源</span>
        <span class="mono mono-subtle">跟随主回源</span>
      </div>
    `;
  }

  if (totalHosts === 1 && playback === (site.target_url || '').trim()) {
    return `
      <div class="site-row">
        <span class="site-row-label">播放回源</span>
        <span class="mono mono-subtle">与主回源相同</span>
      </div>
    `;
  }

  const modeLabel = site.playback_mode === 'redirect' ? '重定向跟随' : '直连分流';
  let rows = '';
  if (playback) {
    rows += `
    <div class="site-row">
      <span class="site-row-label">播放回源</span>
      <span class="mono">${esc(playback)}</span>
    </div>`;
  }
  for (const h of extraHosts) {
    rows += `
    <div class="site-row">
      <span class="site-row-label">播放回源</span>
      <span class="mono">${esc(h)}</span>
    </div>`;
  }
  rows += `
    <div class="site-row">
      <span class="site-row-label">播放模式</span>
      <span class="mono">${modeLabel}</span>
    </div>`;
  return rows;
}

function customUAFormState(mode, site) {
  const isCustom = mode === 'custom';
  return {
    visible: isCustom,
    required: isCustom,
    customUserAgent: isCustom && site ? (site.custom_user_agent || '') : '',
    customClient: isCustom && site ? (site.custom_client || '') : '',
    customVersion: isCustom && site ? (site.custom_version || '') : '',
  };
}

function buildCustomUAPayload(mode, customUserAgent, customClient, customVersion) {
  if (mode !== 'custom') {
    return {
      custom_user_agent: '',
      custom_client: '',
      custom_version: '',
    };
  }
  return {
    custom_user_agent: String(customUserAgent || '').trim(),
    custom_client: String(customClient || '').trim(),
    custom_version: String(customVersion || '').trim(),
  };
}

function buildUpstreamHeaderPayload(headers) {
	return headers
		.filter(header => header.configured || String(header.name || '').trim() || String(header.value || '').trim())
		.map(header => ({
			name: String(header.name || '').trim(),
			value: String(header.value || '').trim(),
		}));
}

const DEFAULT_MAX_PLAYBACK_ADDRESSES = 128;

function normalizeStreamHosts(value) {
	let hosts = value;
	if (typeof hosts === 'string') {
		try {
			hosts = JSON.parse(hosts || '[]');
		} catch (_) {
			return [];
		}
	}
	if (!Array.isArray(hosts)) return [];
	return hosts
		.filter(host => typeof host === 'string' && host.trim())
		.map(host => host.trim());
}

function normalizeSiteCapabilities(value) {
	const capabilities = value && typeof value === 'object' ? value : {};
	const requestedMax = Number(capabilities.max_playback_addresses);
	return {
		host_only_available: capabilities.host_only_available !== false,
		upstream_headers_available: capabilities.upstream_headers_available !== false,
		max_playback_addresses: Number.isInteger(requestedMax) && requestedMax > 0
			? requestedMax
			: DEFAULT_MAX_PLAYBACK_ADDRESSES,
	};
}

const DYNAMIC_PROFILE_IDS = ['safe', 'compatible', 'extreme'];
const DYNAMIC_SOURCE_IDS = ['redirect', 'playback_info', 'hls', 'dash'];
const DYNAMIC_SOURCE_LABELS = {
	redirect: 'HTTP 30x',
	playback_info: 'PlaybackInfo',
	hls: 'HLS',
	dash: 'DASH',
};
const DYNAMIC_PROFILE_SOURCE_IDS = {
	safe: ['redirect', 'playback_info'],
	compatible: [...DYNAMIC_SOURCE_IDS],
	extreme: [...DYNAMIC_SOURCE_IDS],
};
const DYNAMIC_PROFILE_LABELS = {
	safe: 'Safe（安全）',
	compatible: 'Compatible（兼容）',
	extreme: 'Extreme（极限）',
};
const DYNAMIC_PROFILE_NETWORK_DEFAULTS = {
	safe: { allowed_schemes: ['https'], allowed_ports: [443], allow_any_port: false },
	compatible: { allowed_schemes: ['http', 'https'], allowed_ports: [], allow_any_port: true },
	extreme: { allowed_schemes: ['http', 'https'], allowed_ports: [], allow_any_port: true },
};
const DYNAMIC_LIMIT_FIELDS = [
	'allowed_schemes',
	'allowed_ports',
	'allow_any_port',
	'max_redirects',
	'max_authorities',
	'max_active_capabilities',
	'max_urls_per_response',
	'max_body_bytes',
	'max_dns_ips',
	'max_new_authorities_per_minute',
	'max_streams',
	'idle_expiry_seconds',
	'absolute_lifetime_seconds',
];
const DYNAMIC_GLOBAL_LIMIT_FIELDS = [
	'max_authorities',
	'max_active_capabilities',
	'max_streams',
	'max_new_authorities_per_minute',
	'max_dns_workers',
	'max_concurrent_parses',
	'max_site_concurrent_parses',
	'max_parse_memory_bytes',
	'max_site_parse_memory_bytes',
	'max_capability_memory_bytes',
	'max_site_capability_memory_bytes',
	'max_parse_depth',
	'max_string_bytes',
	'max_target_url_bytes',
];
const DYNAMIC_FEATURES = [
	['redirect_discovery', 'HTTP 30x 发现', true],
	['playback_info', 'PlaybackInfo 改写', true],
	['hls', 'HLS 解析', true],
	['dash', 'DASH 解析', true],
	['private_targets', '私网目标', false],
	['custom_ca', '自定义 CA', false],
	['raw_fallback', '原始响应回退', false],
];
const DYNAMIC_OBSERVATION_REASON_CODES = new Set([
	'redirect_allowed',
	'candidate_allowed',
	'invalid_location',
	'unsupported_status',
	'redirect_loop',
	'hop_limit',
	'scheme_denied',
	'port_denied',
	'domain_denied',
	'https_downgrade_denied',
	'self_target',
	'dns_failure',
	'address_denied',
	'dial_failure',
	'tls_failure',
	'capacity_limit',
	'rate_limit',
	'parse_failure',
	'request_unclassified',
	'structured_body_limit',
	'playback_info_denied',
	'hls_feature_denied',
	'dash_feature_denied',
	'redirect_body_replay_denied',
	'capability_invalid',
	'capability_expired',
	'response_failure',
	'runtime_unavailable',
]);

function hasOwnDynamicField(value, field) {
	return Object.prototype.hasOwnProperty.call(value, field);
}

function isStructuredDiscoveryContract(value) {
	if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
	if (value.stage !== 'structured-discovery' || typeof value.available !== 'boolean' || typeof value.key_configured !== 'boolean') return false;
	if (value.available !== value.key_configured) return false;
	if (!value.global_limits || typeof value.global_limits !== 'object' || Array.isArray(value.global_limits)) return false;
	if (!DYNAMIC_GLOBAL_LIMIT_FIELDS.every(field => hasOwnDynamicField(value.global_limits, field) && Number.isInteger(value.global_limits[field]) && value.global_limits[field] > 0)) return false;
	if (!Array.isArray(value.profiles) || value.profiles.length !== DYNAMIC_PROFILE_IDS.length) return false;

	const profiles = new Map(value.profiles.map(profile => [profile && profile.id, profile]));
	if (profiles.size !== DYNAMIC_PROFILE_IDS.length) return false;
	return DYNAMIC_PROFILE_IDS.every(id => {
		const profile = profiles.get(id);
		if (!profile || typeof profile.label !== 'string' || typeof profile.recommended !== 'boolean') return false;
		if (!profile.limits || typeof profile.limits !== 'object' || Array.isArray(profile.limits)) return false;
		if (!DYNAMIC_LIMIT_FIELDS.every(field => hasOwnDynamicField(profile.limits, field))) return false;
		if (!Array.isArray(profile.limits.allowed_schemes) || profile.limits.allowed_schemes.length === 0 || !profile.limits.allowed_schemes.every(scheme => scheme === 'http' || scheme === 'https')) return false;
		if (!Array.isArray(profile.limits.allowed_ports) || !profile.limits.allowed_ports.every(port => Number.isInteger(port) && port > 0 && port <= 65535)) return false;
		if (typeof profile.limits.allow_any_port !== 'boolean') return false;
		if (!DYNAMIC_LIMIT_FIELDS.slice(3).every(field => Number.isInteger(profile.limits[field]) && profile.limits[field] > 0)) return false;
		if (!profile.features || typeof profile.features !== 'object' || Array.isArray(profile.features)) return false;
		return DYNAMIC_FEATURES.every(([field, , expected]) => {
			const profileExpected = id === 'safe' && (field === 'hls' || field === 'dash') ? false : expected;
			return hasOwnDynamicField(profile.features, field) && profile.features[field] === profileExpected;
		});
	});
}

function normalizeDynamicProfiles(value) {
	const recognized = isStructuredDiscoveryContract(value);
	const sourceProfiles = recognized
		? new Map(value.profiles.map(profile => [profile.id, profile]))
		: new Map();
	return {
		stage: 'structured-discovery',
		available: recognized && value.available === true,
		key_configured: recognized && value.key_configured === true,
		recognized,
		profiles: DYNAMIC_PROFILE_IDS.map(id => {
			const profile = sourceProfiles.get(id);
			return {
				id,
				label: profile ? profile.label : DYNAMIC_PROFILE_LABELS[id],
				recommended: profile ? profile.recommended : id === 'safe',
				limits: profile ? profile.limits : DYNAMIC_PROFILE_NETWORK_DEFAULTS[id],
				features: profile ? profile.features : {},
			};
		}),
		global_limits: recognized ? value.global_limits : {},
	};
}

async function loadDynamicProfiles() {
	try {
		return normalizeDynamicProfiles(await API.getDynamicProfiles());
	} catch (_) {
		return normalizeDynamicProfiles(null);
	}
}

function normalizeDynamicProfile(value) {
	const profile = String(value || '').trim().toLowerCase();
	return DYNAMIC_PROFILE_IDS.includes(profile) ? profile : 'safe';
}
function dynamicSourcesForProfile(value) {
	return [...DYNAMIC_PROFILE_SOURCE_IDS[normalizeDynamicProfile(value)]];
}


function normalizeDynamicDiscoverySources(value, profile = 'safe') {
	const allowed = new Set(dynamicSourcesForProfile(profile));
	if (!Array.isArray(value)) return dynamicSourcesForProfile(profile);
	const selected = new Set(value.map(source => String(source || '').trim().toLowerCase()));
	return DYNAMIC_SOURCE_IDS.filter(source => allowed.has(source) && selected.has(source));
}

function normalizeDynamicDomainRules(value) {
	if (!Array.isArray(value)) return [];
	return value.flatMap(rule => {
		if (!rule || typeof rule !== 'object' || Array.isArray(rule)) return [];
		const type = String(rule.type || '').trim().toLowerCase();
		const host = String(rule.value || '').trim().toLowerCase();
		if ((type !== 'exact' && type !== 'suffix') || !host) return [];
		return [{ type, value: host }];
	});
}

function isPlausibleSafeDynamicDNSRule(rule) {
	const normalized = normalizeDynamicDomainRules([rule])[0];
	if (!normalized) return false;
	let host = normalized.value;
	if (host.startsWith('.') || host.includes('*') || /[\s/\\@?#:%]/.test(host)) return false;
	host = host.replace(/\.$/, '');
	if (!host) return false;
	let asciiHost;
	try {
		asciiHost = new URL(`https://${host}/`).hostname.toLowerCase();
	} catch (_) {
		return false;
	}
	if (!asciiHost || asciiHost.startsWith('[') || /^\d+(?:\.\d+){3}$/.test(asciiHost)) return false;
	const labels = asciiHost.split('.');
	if (labels.length < 2 || !/[a-z]/.test(labels[labels.length - 1])) return false;
	return labels.every(label => label.length > 0 && label.length <= 63 && !label.startsWith('-') && !label.endsWith('-') && /^[a-z0-9-]+$/.test(label));
}

function hasRequiredSafeDynamicRules(profile, rules) {
	return normalizeDynamicProfile(profile) !== 'safe' || normalizeDynamicDomainRules(rules).some(isPlausibleSafeDynamicDNSRule);
}

function normalizeDynamicSitePolicy(site) {
	const value = site && typeof site === 'object' ? site : {};
	const revision = value.dynamic_policy_revision;
	const profile = normalizeDynamicProfile(value.dynamic_profile);
	return {
		dynamic_discovery_enabled: value.dynamic_discovery_enabled === true,
		dynamic_profile: profile,
		dynamic_discovery_sources: normalizeDynamicDiscoverySources(value.dynamic_discovery_sources, profile),
		dynamic_domain_rules: normalizeDynamicDomainRules(value.dynamic_domain_rules),
		dynamic_allow_https_downgrade: value.dynamic_allow_https_downgrade === true,
		dynamic_policy_revision: Number.isInteger(revision) && revision > 0 ? revision : 1,
	};
}

function buildDynamicPolicyPayload(policy, capabilities) {
	const normalized = normalizeDynamicSitePolicy(policy);
	const dynamicCapabilities = normalizeDynamicProfiles(capabilities);
	if (!dynamicCapabilities.recognized) return {};
	return {
        dynamic_discovery_enabled: normalized.dynamic_discovery_enabled,
		dynamic_profile: normalized.dynamic_profile,
		dynamic_discovery_sources: normalized.dynamic_discovery_sources,
		dynamic_domain_rules: normalized.dynamic_domain_rules,
		dynamic_allow_https_downgrade: normalized.dynamic_allow_https_downgrade,
	};
}

function renderDynamicProfileOptions(capabilities, selectedProfile) {
	const dynamicCapabilities = normalizeDynamicProfiles(capabilities);
	const selected = normalizeDynamicProfile(selectedProfile);
	return dynamicCapabilities.profiles.map(profile => `
		<option value="${esc(profile.id)}" ${profile.id === selected ? 'selected' : ''}>${esc(profile.label)}${profile.recommended ? '（推荐）' : ''}</option>
	`).join('');
}

function renderDynamicProfileSummaries(capabilities) {
	const dynamicCapabilities = normalizeDynamicProfiles(capabilities);
	return dynamicCapabilities.profiles.map(profile => {
		const schemes = Array.isArray(profile.limits.allowed_schemes)
			? profile.limits.allowed_schemes.map(scheme => String(scheme).toUpperCase()).join('/')
			: '—';
		const ports = profile.limits.allow_any_port === true
			? '全部端口'
			: (Array.isArray(profile.limits.allowed_ports) ? profile.limits.allowed_ports.join(', ') : '—');
		const sources = dynamicSourcesForProfile(profile.id).map(source => DYNAMIC_SOURCE_LABELS[source]).join(' + ');
		const compatibility = profile.id === 'extreme'
			? '；额外启用全数据面 30x/303、受限请求体重放、PlaybackInfo 完整 URL 兼容、安全 RequiredHttpHeaders、HLS 变量/扩展标签与 DASH 惰性扩展/DRM 元数据'
			: '；使用严格协议字段与已审核结构';
		const accent = profile.id === 'safe' ? 'var(--green)' : profile.id === 'compatible' ? 'var(--orange)' : 'var(--red)';
		return `<div class="form-help" data-profile-summary="${esc(profile.id)}" style="padding:8px 10px;margin-top:6px;border:1px solid ${accent};border-radius:6px;background:var(--surface-hover);color:var(--white-87)"><strong style="color:${accent}">${esc(profile.label)}</strong>：${esc(sources)}；仅公网 ${esc(schemes)}，端口 ${esc(ports)}${esc(compatibility)}</div>`;
	}).join('');
}

function renderDynamicRuleRows(rules) {
	const rows = Array.isArray(rules) ? rules : [];
	return rows.map((rule, index) => {
		const type = rule && rule.type === 'suffix' ? 'suffix' : 'exact';
		const value = rule && rule.value !== undefined ? rule.value : '';
		return `
		<div class="m-dynamic-rule-row" data-idx="${index}" style="display:flex;gap:6px;margin-bottom:6px;align-items:center">
		  <select class="form-select modal-select m-dynamic-rule-type" data-idx="${index}" style="width:auto;flex-shrink:0">
			<option value="exact" ${type === 'exact' ? 'selected' : ''}>精确</option>
			<option value="suffix" ${type === 'suffix' ? 'selected' : ''}>后缀</option>
		  </select>
		  <input type="text" class="form-input m-dynamic-rule-value" data-idx="${index}" value="${esc(value)}" placeholder="media.example.com" maxlength="253" autocapitalize="none" autocorrect="off" spellcheck="false" style="flex:1">
		  <button type="button" class="btn-ghost danger m-dynamic-rule-remove" data-idx="${index}" style="padding:4px 8px;font-size:13px;flex-shrink:0">删除</button>
		</div>`;
	}).join('');
}

function renderDynamicStatus(capabilities) {
	const dynamicCapabilities = normalizeDynamicProfiles(capabilities);
	const contractWarning = dynamicCapabilities.recognized
		? ''
		: '<div class="form-help" style="color:var(--orange)">动态能力数据缺失、格式异常或版本过旧，已按不可用处理。</div>';
	const keyStatus = !dynamicCapabilities.recognized ? '未知' : dynamicCapabilities.key_configured ? '已配置' : '未配置';
	const unavailableFeatures = DYNAMIC_FEATURES
		.filter(([, , available]) => !available)
		.map(([, label]) => `${esc(label)}：不可用`)
		.join('；');
	return `
		<div class="form-help"><strong>自动播放后端发现</strong>：支持安全 HTTP 30x、PlaybackInfo、HLS 和 DASH；Safe/Compatible 仅处理严格协议字段，Extreme 另启用有界扩展兼容，但仍不扫描任意正文；任何目标都须通过 capability、DNS/SSRF/TLS 与 Header 清洗。</div>
		${contractWarning}
		<div class="form-help">部署状态：${dynamicCapabilities.available ? '可用' : '不可用'}；DYNAMIC_ROUTE_KEY：${keyStatus}（仅显示配置状态，不显示密钥）</div>
		<div class="form-help">仍明确不可用：${unavailableFeatures}</div>
	`;
}

function dynamicProfileRiskNotice(profile) {
	const normalized = normalizeDynamicProfile(profile);
	switch (normalized) {
	case 'compatible':
		return {
			level: 'compatible',
			badge: '黄色确认',
			color: 'var(--orange)',
			background: 'var(--orange-dim)',
			message: '可访问任意公网域名和 1–65535 端口；不开放私网、自定义 CA 或原始地址回退。首次启用必须确认。',
		};
	case 'extreme':
		return {
			level: 'extreme',
			badge: '高风险',
			color: 'var(--red)',
			background: 'var(--red-dim)',
			message: '除放大公网 authority、动态流和生命周期上限外，还会对 CONNECT/Upgrade/保留路径之外的数据面方法和路径处理 30x/303，并可能把有界请求体重放到上游指定且通过安全校验的公网目标；同时启用 PlaybackInfo、HLS 与 DASH 扩展兼容。仍不开放隧道、私网、自定义 CA、原始地址回退或未签名 target。进入此档必须勾选确认、输入站点名称并通过弹窗。',
		};
	default:
		return {
			level: 'safe',
			badge: '推荐',
			color: 'var(--green)',
			background: 'var(--green-dim)',
			message: '仅允许 HTTPS:443，且未知目标必须命中精确或后缀 DNS 域名规则。',
		};
	}
}

function renderDynamicProfileRisk(profile) {
	const notice = dynamicProfileRiskNotice(profile);
	const badgeTextColor = notice.level === 'extreme' ? '#fff' : '#111';
	return `<div class="form-help" data-profile-risk="${esc(notice.level)}" style="padding:8px 10px;border:1px solid ${notice.color};border-radius:8px;background:${notice.background};color:var(--white-87)"><span data-profile-risk-badge="${esc(notice.level)}" style="display:inline-block;padding:2px 7px;border-radius:999px;background:${notice.color};color:${badgeTextColor};font-weight:800;margin-right:5px">${esc(notice.badge)}</span>${esc(notice.message)}</div>`;
}

function dynamicProfileConfirmationRequirement(initialPolicy, nextPolicy) {
	const initial = normalizeDynamicSitePolicy(initialPolicy);
	const next = normalizeDynamicSitePolicy(nextPolicy);
	if (!next.dynamic_discovery_enabled) return 'none';
	if (next.dynamic_profile === 'extreme' && (!initial.dynamic_discovery_enabled || initial.dynamic_profile !== 'extreme')) return 'extreme';
	if (next.dynamic_profile === 'compatible' && (!initial.dynamic_discovery_enabled || initial.dynamic_profile === 'safe')) return 'compatible';
	return 'none';
}

function confirmDynamicProfileChange(initialPolicy, nextPolicy, siteName, extremeAcknowledged, extremeTypedName) {
	const requirement = dynamicProfileConfirmationRequirement(initialPolicy, nextPolicy);
	if (requirement === 'compatible') {
		const accepted = window.confirm('Compatible（兼容）可访问任意公网域名和 1–65535 端口。仍会拒绝私网、特殊地址和未验证拨号。确定启用吗？');
		return { ok: accepted, requirement, error: '' };
	}
	if (requirement === 'extreme') {
		if (!extremeAcknowledged) return { ok: false, requirement, error: '启用 Extreme 前必须勾选高风险确认' };
		if (String(extremeTypedName || '').trim() !== String(siteName || '').trim()) return { ok: false, requirement, error: '启用 Extreme 时必须准确输入站点名称' };
		const accepted = window.confirm('Extreme（极限）会启用全数据面 30x/303、受限请求体重放和更宽的 PlaybackInfo/HLS/DASH 兼容，并显著放大公网发现与并发上限。请求体可能被重放到上游指定且通过安全校验的公网目标；仍不启用私网、自定义 CA、原始地址回退或未签名 target。确定继续吗？');
		return { ok: accepted, requirement, error: '' };
	}
	return { ok: true, requirement, error: '' };
}

function renderDynamicEnableControl(capabilities, policy) {
	const dynamicCapabilities = normalizeDynamicProfiles(capabilities);
	const dynamicPolicy = normalizeDynamicSitePolicy(policy);
	const sources = new Set(dynamicPolicy.dynamic_discovery_sources);
	const allowedSources = new Set(dynamicSourcesForProfile(dynamicPolicy.dynamic_profile));
	const policyEditable = dynamicCapabilities.recognized && dynamicCapabilities.available;
	const enableEditable = dynamicCapabilities.recognized && (dynamicCapabilities.available || dynamicPolicy.dynamic_discovery_enabled);
	return `
		<label style="display:flex;gap:8px;align-items:center;margin-top:10px">
          <input type="checkbox" id="m-dynamic-enabled" ${dynamicPolicy.dynamic_discovery_enabled ? 'checked' : ''} ${enableEditable ? '' : 'disabled'}>
		  启用自动播放后端发现
		</label>
		<div class="form-help">Safe 仅启用严格 HTTP 30x 与 PlaybackInfo；Compatible 可增加严格 HLS/DASH；Extreme 额外支持全数据面重定向、受限请求体重放及有界协议扩展，但不扫描 HTML/JavaScript 或 prose 子串。</div>
		<div id="m-dynamic-sources" style="display:flex;flex-wrap:wrap;gap:12px;margin-top:10px">
		  ${DYNAMIC_SOURCE_IDS.map(source => `<label style="display:flex;gap:6px;align-items:center"><input type="checkbox" id="m-dynamic-source-${esc(source)}" data-dynamic-source="${esc(source)}" ${sources.has(source) ? 'checked' : ''} ${policyEditable && allowedSources.has(source) ? '' : 'disabled'}>${esc(DYNAMIC_SOURCE_LABELS[source])}</label>`).join('')}
		</div>
	`;
}

function privacySafeObservationAuthority(value) {
	if (typeof value !== 'string') return '—';
	const match = /^(https?):\/\/(\[[0-9a-f:.]+\]|[a-z0-9.-]+):([0-9]{1,5})$/i.exec(value.trim());
	if (!match) return '—';
	const port = Number(match[3]);
	if (!Number.isInteger(port) || port < 1 || port > 65535) return '—';
	const host = match[2].toLowerCase();
	if (!host.startsWith('[')) {
		const labels = host.split('.');
		if (!labels.every(label => label.length > 0 && label.length <= 63 && !label.startsWith('-') && !label.endsWith('-') && /^[a-z0-9-]+$/.test(label))) return '—';
	}
	try {
		new URL(`${match[1].toLowerCase()}://${host}:${port}/`);
	} catch (_) {
		return '—';
	}
	return `${match[1].toLowerCase()}://${host}:${port}`;
}

function privacySafeObservationReason(value) {
	return typeof value === 'string' && DYNAMIC_OBSERVATION_REASON_CODES.has(value) ? value : '—';
}

function formatObservationTimestamp(value) {
	if (!Number.isSafeInteger(value) || value < 0) return '—';
	const timestamp = new Date(value);
	return Number.isNaN(timestamp.getTime()) ? '—' : timestamp.toISOString();
}

function normalizeDynamicObservationsResponse(value) {
	const response = value && typeof value === 'object' && !Array.isArray(value) ? value : {};
	const observations = Array.isArray(response.observations) ? response.observations : [];
	return {
		observations: observations.map(observation => {
			const item = observation && typeof observation === 'object' && !Array.isArray(observation) ? observation : {};
			return {
				authority: privacySafeObservationAuthority(item.canonical_authority),
				source: DYNAMIC_SOURCE_IDS.includes(item.source) ? item.source : '—',
				decision: item.decision === 'allowed' || item.decision === 'denied' ? item.decision : '—',
				reason: privacySafeObservationReason(item.reason_code),
				firstSeen: formatObservationTimestamp(item.first_seen_ms),
				lastSeen: formatObservationTimestamp(item.last_seen_ms),
				count: Number.isSafeInteger(item.count) && item.count > 0 ? item.count : '—',
			};
		}),
		dropped: Number.isSafeInteger(response.dropped_observations) && response.dropped_observations >= 0
			? response.dropped_observations
			: '—',
	};
}

function renderDynamicObservations(value) {
	const response = normalizeDynamicObservationsResponse(value);
	const rows = response.observations.map(observation => `
		<tr>
		  <td>${esc(observation.authority)}</td>
		  <td>${esc(observation.source)}</td>
		  <td>${esc(observation.decision)}</td>
		  <td>${esc(observation.reason)}</td>
		  <td>${esc(observation.firstSeen)}</td>
		  <td>${esc(observation.lastSeen)}</td>
		  <td>${esc(observation.count)}</td>
		</tr>
	`).join('');
	return `
		<div class="form-help">已丢弃观察记录：${esc(response.dropped)}</div>
		${rows ? `
		<div style="overflow-x:auto;margin-top:8px">
		  <table>
			<thead><tr><th>规范化权威</th><th>来源</th><th>决策</th><th>原因代码</th><th>首次观察</th><th>最近观察</th><th>次数</th></tr></thead>
			<tbody>${rows}</tbody>
		  </table>
		</div>` : '<div class="form-help" style="margin-top:8px">暂无观察记录。</div>'}
	`;
}

function renderDynamicObservationsPanel(supported) {
	return `
		<div style="margin-top:16px;padding-top:12px;border-top:1px solid var(--glass-border)">
		  <label>自动发现观察记录</label>
		  <div class="form-help">记录由服务器限量保留并定期过期清理。这里只显示规范化权威、有限原因代码和聚合时间/次数；不会显示完整 URL、路径、查询参数、令牌、请求头或正文。</div>
		  <div style="display:flex;gap:8px;margin-top:8px">
			<button type="button" class="btn-ghost" id="m-refresh-dynamic-observations" ${supported ? '' : 'disabled'}>刷新</button>
			<button type="button" class="btn-ghost danger" id="m-clear-dynamic-observations" ${supported ? '' : 'disabled'}>清空</button>
		  </div>
		  <div id="m-dynamic-observations" style="margin-top:8px">${supported ? '<div class="form-help">正在读取观察记录…</div>' : '<div class="form-help">当前后端不提供自动发现观察记录。</div>'}</div>
		</div>
	`;
}

function canAddPlaybackAddress(currentCount, maxPlaybackAddresses) {
	return currentCount < maxPlaybackAddresses;
}

function renderUpstreamHeaderRows(headers, upstreamHeadersAvailable) {
	return headers.map((header, idx) => `
		<div style="display:flex;gap:6px;margin-bottom:6px;align-items:center">
		  <input type="text" class="form-input m-upstream-header-name" data-idx="${idx}" value="${esc(header.name)}" placeholder="Header 名称" maxlength="64" autocapitalize="none" autocorrect="off" spellcheck="false" style="flex:1" ${upstreamHeadersAvailable ? '' : 'disabled'}>
		  <input type="password" class="form-input m-upstream-header-value" data-idx="${idx}" value="" placeholder="${header.configured ? '已配置；留空保持不变' : 'Header 值'}" maxlength="1024" autocomplete="new-password" style="flex:1" ${upstreamHeadersAvailable ? '' : 'disabled'}>
		  <button type="button" class="btn-ghost danger m-upstream-header-remove" data-idx="${idx}" style="padding:4px 8px;font-size:13px;flex-shrink:0">删除</button>
		</div>
	`).join('');
}

function normalizedIngressMode(site) {
	const mode = String((site && site.ingress_mode) || '').trim().toLowerCase();
	if (mode === 'port' || mode === 'host' || mode === 'both') return mode;
	return site && String(site.public_host || '').trim() ? 'host' : 'port';
}

function ingressFormState(mode) {
	const normalized = ['port', 'host', 'both'].includes(mode) ? mode : 'host';
	return {
		mode: normalized,
		showPublicHost: normalized !== 'port',
		requirePublicHost: normalized !== 'port',
		portLabel: normalized === 'host' ? '保留端口（此模式不监听）' : '监听端口',
		warning: normalized === 'both'
			? '此模式会同时开放独立高端口；若前方使用 CDN，请用防火墙限制该端口，避免绕过 CDN。'
			: normalized === 'port'
				? '独立端口会绑定所有网络接口；公网部署时请配置防火墙。'
				: '仅通过共享 Host 入口代理，不会绑定保留端口；要求面板绑定回环地址，或用 TRUSTED_PROXY_CIDRS 限定可信入口来源。',
	};
}

function buildIngressPayload(mode, port, publicHost) {
	const state = ingressFormState(mode);
	return {
		ingress_mode: state.mode,
		listen_port: parseInt(port),
		public_host: state.showPublicHost ? String(publicHost || '').trim() : '',
	};
}

function defaultIngressMode(capabilities) {
	return capabilities && capabilities.host_only_available === false ? 'port' : 'host';
}

function renderIngressSummary(site) {
	const mode = normalizedIngressMode(site);
	const labels = { port: '仅独立端口', host: '仅共享域名', both: '共享域名 + 独立端口' };
	let rows = `
	  <div class="site-row">
		<span class="site-row-label">入口模式</span>
		<span>${labels[mode]}</span>
	  </div>`;
	if (mode === 'port' || mode === 'both') {
		rows += `
	  <div class="site-row">
		<span class="site-row-label">监听端口</span>
		<span class="mono">:${site.listen_port}</span>
	  </div>`;
	}
	if (mode === 'host' || mode === 'both') {
		rows += `
	  <div class="site-row">
		<span class="site-row-label">共享入口</span>
		<span class="mono">Host: ${esc(site.public_host || '')}</span>
	  </div>`;
	}
	return rows;
}

function normalizedTargetAuthority(value) {
	let candidate = String(value || '').trim().replaceAll('：', ':');
	if (!candidate) return '';
	if (!candidate.includes('://')) {
		const authority = candidate.split(/[/?#]/, 1)[0];
		candidate = authority.endsWith(':443') ? `https://${candidate}` : `http://${candidate}`;
	}
	try {
		const parsed = new URL(candidate);
		const scheme = parsed.protocol.toLowerCase();
		if (scheme !== 'http:' && scheme !== 'https:') return '';
		const defaultPort = scheme === 'https:' ? '443' : '80';
		return `${scheme}//${parsed.hostname.toLowerCase()}:${parsed.port || defaultPort}`;
	} catch (_) {
		return '';
	}
}

async function showSiteModal(site) {
  const isEdit = !!site;
  const title = isEdit ? '编辑站点' : '添加站点';
	let siteCapabilities;
	try {
		siteCapabilities = normalizeSiteCapabilities(await API.ingressCapabilities());
	} catch (error) {
		Toast.error(`无法读取站点能力：${error.message}`);
		return;
	}
	const hostOnlyAvailable = siteCapabilities.host_only_available;
	const upstreamHeadersAvailable = siteCapabilities.upstream_headers_available;
	const maxPlaybackAddresses = siteCapabilities.max_playback_addresses;
	const dynamicCapabilities = await loadDynamicProfiles();
	const dynamicPolicy = normalizeDynamicSitePolicy(site);
	const dynamicPolicyEditable = dynamicCapabilities.recognized && dynamicCapabilities.available;

  document.getElementById('modal-title').textContent = title;
  document.getElementById('modal-body').innerHTML = `
    <div class="form-group">
      <label>站点名称</label>
      <input type="text" class="form-input" id="m-name" value="${isEdit ? esc(site.name) : ''}" placeholder="如：Emby-US-01" maxlength="100" required>
    </div>
    <div class="form-group">
      <label>主回源地址</label>
      <input type="text" class="form-input" id="m-target" value="${isEdit ? esc(site.target_url) : ''}" placeholder="如：192.168.1.10:8096 或 https://emby.example.com" inputmode="url" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="2048" required>
      <div class="form-help">网页、API 和默认回源都走这里。未写协议时，:443 自动使用 HTTPS，其他端口默认 HTTP。</div>
    </div>
    <div class="form-group">
      <label>播放回源列表（可选，留空跟随主回源）</label>
      <div id="m-playback-list"></div>
      <button type="button" class="btn-ghost" id="m-add-playback" style="margin-top:6px;font-size:13px">+ 添加播放回源</button>
      <div class="form-help">第一个地址是实际播放回源；额外地址仅作为重定向模式的允许列表，不会自动探测、轮询或故障转移（最多 ${maxPlaybackAddresses} 个）。未写协议时，:443 自动使用 HTTPS。</div>
    </div>
    <div class="form-group" id="playback-mode-group" style="display:none">
      <label>播放模式</label>
      <select class="form-select modal-select" id="m-playback-mode">
        <option value="direct" ${(!isEdit || site.playback_mode !== 'redirect') ? 'selected' : ''}>直连分流</option>
        <option value="redirect" ${isEdit && site.playback_mode === 'redirect' ? 'selected' : ''}>重定向跟随</option>
      </select>
      <div class="form-help">直连分流：播放请求直接发送到首个播放回源（适合完整 Emby 实例）。重定向跟随：所有请求经主回源，自动跟随重定向到任一播放回源（适合多节点 CDN）。</div>
    </div>
	<div class="form-group">
	  <label>入口模式</label>
	  <select class="form-select modal-select" id="m-ingress-mode">
		<option value="host" ${hostOnlyAvailable ? '' : 'disabled'}>仅共享域名（推荐${hostOnlyAvailable ? '' : '，当前部署不可用'}）</option>
		<option value="port">仅独立端口</option>
		<option value="both">共享域名 + 独立端口（高风险）</option>
	  </select>
	  <div class="form-help" id="m-ingress-warning"></div>
	  ${hostOnlyAvailable ? '' : '<div class="form-help">当前面板既未绑定回环地址，也没有可信代理来源白名单；请先设置 PANEL_BIND_ADDR 或 TRUSTED_PROXY_CIDRS 并重启，才能启用仅共享域名。</div>'}
	</div>
	<div class="form-group" id="m-port-group">
	  <label id="m-port-label">监听端口</label>
	  <input type="number" class="form-input" id="m-port" value="${isEdit ? site.listen_port : ''}" placeholder="如：8001" min="1" max="65535" inputmode="numeric" required>
	</div>
	<div class="form-group" id="m-public-host-group">
	  <label>共享入口域名</label>
	  <input type="text" class="form-input" id="m-public-host" value="${isEdit ? esc(site.public_host || '') : ''}" placeholder="如：emby.example.com" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="253">
	  <div class="form-help">通过面板监听入口按精确 Host 转发到本站点。只填域名，不填协议、端口、路径或通配符。</div>
	</div>
		<div class="form-group">
		  <label>主回源固定请求头（可选）</label>
		  <div id="m-upstream-headers"></div>
		  <button type="button" class="btn-ghost" id="m-add-upstream-header" style="margin-top:6px;font-size:13px" ${upstreamHeadersAvailable ? '' : 'disabled'}>+ 添加请求头</button>
		  <div class="form-help">值使用 UPSTREAM_HEADER_KEY 加密保存且不会回显，只发送给主回源的精确协议、域名和端口；更换主回源的协议、域名或端口后必须重新输入这些值。</div>
		  ${upstreamHeadersAvailable ? '' : '<div class="form-help" style="color:var(--orange)">当前部署未配置 UPSTREAM_HEADER_KEY，不能新增、重命名或修改 Header 值；仍可删除旧配置。配置密钥并重启后可恢复编辑。</div>'}
		</div>
    <div class="form-group">
      <label>UA 模式</label>
      <select class="form-select modal-select" id="m-ua">
        <option value="infuse" ${(!isEdit || site.ua_mode === 'infuse') ? 'selected' : ''}>Infuse</option>
        <option value="web" ${isEdit && site.ua_mode === 'web' ? 'selected' : ''}>Web</option>
        <option value="client" ${isEdit && site.ua_mode === 'client' ? 'selected' : ''}>客户端</option>
        <option value="custom">自定义</option>
        <option value="passthrough" ${isEdit && site.ua_mode === 'passthrough' ? 'selected' : ''}>透传（保留客户端身份）</option>
      </select>
    </div>
    <div class="form-group" id="m-custom-ua-group" hidden>
      <label>自定义身份</label>
      <input type="text" class="form-input" id="m-custom-ua" placeholder="User-Agent" maxlength="1024" autocapitalize="none" autocorrect="off" spellcheck="false">
      <input type="text" class="form-input" id="m-custom-client" placeholder="Emby Client" maxlength="128" autocapitalize="none" autocorrect="off" spellcheck="false" style="margin-top:8px">
      <input type="text" class="form-input" id="m-custom-version" placeholder="Emby Version" maxlength="64" autocapitalize="none" autocorrect="off" spellcheck="false" style="margin-top:8px">
      <div class="form-help">仅改写 User-Agent、Client 和 Version；Device 与 DeviceId 保持原样。</div>
    </div>
    <div class="form-group">
      <label>自动播放后端发现策略</label>
      ${renderDynamicStatus(dynamicCapabilities)}
      ${renderDynamicEnableControl(dynamicCapabilities, dynamicPolicy)}
      <fieldset id="m-dynamic-policy-fields" ${dynamicPolicyEditable ? '' : 'disabled'} style="border:0;padding:0;margin:0;min-width:0">
      <label for="m-dynamic-profile" style="display:block;margin-top:12px">安全配置</label>
      <select class="form-select modal-select" id="m-dynamic-profile">
        ${renderDynamicProfileOptions(dynamicCapabilities, dynamicPolicy.dynamic_profile)}
      </select>
      <div id="m-dynamic-profile-risk" style="margin-top:8px">${renderDynamicProfileRisk(dynamicPolicy.dynamic_profile)}</div>
      <div id="m-dynamic-extreme-confirm" hidden style="margin-top:10px;padding:10px;border:1px solid var(--red);border-radius:8px;background:var(--red-dim)">
        <label style="display:flex;gap:8px;align-items:flex-start;color:var(--red)">
          <input type="checkbox" id="m-dynamic-extreme-ack">
          我理解 Extreme 会扩大协议兼容与公网发现边界，并可能按 30x 将有界请求体重放到经校验的公网目标
        </label>
        <input type="text" class="form-input" id="m-dynamic-extreme-name" placeholder="输入站点名称以确认" maxlength="100" autocomplete="off" style="margin-top:8px">
      </div>
      ${renderDynamicProfileSummaries(dynamicCapabilities)}
      <label style="display:block;margin-top:12px">允许域名规则</label>
      <div id="m-dynamic-rules"></div>
      <button type="button" class="btn-ghost" id="m-add-dynamic-rule" style="margin-top:6px;font-size:13px">+ 添加域名规则</button>
      <div class="form-help">精确规则只允许该域名；后缀规则允许该域名及其子域名。请分开选择类型，不要填写通配符、协议、端口、路径或目标 URL。</div>
      <div class="form-help">前端检查仅用于提前发现误配；所有规则仍由服务器重新规范化、校验并执行，界面不是安全策略边界。</div>
      <label style="display:flex;gap:8px;align-items:center;margin-top:12px">
        <input type="checkbox" id="m-dynamic-downgrade" ${dynamicPolicy.dynamic_allow_https_downgrade ? 'checked' : ''}>
        允许动态目标从 HTTPS 降级到 HTTP
      </label>
      <div class="form-help">此项控制所有发现来源中的 HTTPS 到 HTTP 降级；目标仍须通过服务器的完整安全策略。</div>
      <div class="form-help">策略修订：${esc(dynamicPolicy.dynamic_policy_revision)}（由服务器管理，仅在策略实际变化时递增）</div>
      </fieldset>
      ${isEdit ? renderDynamicObservationsPanel(dynamicCapabilities.recognized) : ''}
    </div>

    <div class="form-group">
      <label>流量额度 (GB, 0=不限)</label>
      <input type="number" class="form-input" id="m-quota" value="${isEdit ? Math.round((site.traffic_quota || 0) / 1073741824) : 0}" placeholder="0" min="0" inputmode="numeric">
    </div>
    <div class="form-group">
      <label>单连接限速 (Mbps, 0=不限)</label>
      <input type="number" class="form-input" id="m-speed" value="${isEdit ? (site.speed_limit || 0) : 0}" placeholder="0" min="0" max="1000000" step="1" inputmode="numeric">
      <div class="form-help">限制单个 HTTP 响应和 WebSocket 下行连接的速度；上传方向不受此项影响。</div>
    </div>
  `;

  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="m-cancel">取消</button>
    <button class="btn-modal primary" id="m-submit">${isEdit ? '保存' : '创建'}</button>
  `;

	document.getElementById('m-cancel').addEventListener('click', closeModal);

	const ingressSelect = document.getElementById('m-ingress-mode');
	const publicHostGroup = document.getElementById('m-public-host-group');
	const publicHostInput = document.getElementById('m-public-host');
	const portLabel = document.getElementById('m-port-label');
	const ingressWarning = document.getElementById('m-ingress-warning');
	ingressSelect.value = isEdit ? normalizedIngressMode(site) : defaultIngressMode(siteCapabilities);
	function updateIngressFields() {
		const state = ingressFormState(ingressSelect.value);
		publicHostGroup.hidden = !state.showPublicHost;
		publicHostInput.required = state.requirePublicHost;
		portLabel.textContent = state.portLabel;
		ingressWarning.textContent = state.warning;
	}
	updateIngressFields();
	ingressSelect.addEventListener('change', updateIngressFields);

	const uaSelect = document.getElementById('m-ua');
  const customUAGroup = document.getElementById('m-custom-ua-group');
  const customUAInputs = [
    document.getElementById('m-custom-ua'),
    document.getElementById('m-custom-client'),
    document.getElementById('m-custom-version'),
  ];
  const initialUAState = customUAFormState(isEdit ? site.ua_mode : 'infuse', site);
  uaSelect.value = isEdit && site.ua_mode ? site.ua_mode : 'infuse';
  customUAInputs[0].value = initialUAState.customUserAgent;
  customUAInputs[1].value = initialUAState.customClient;
  customUAInputs[2].value = initialUAState.customVersion;

  function toggleCustomUAFields() {
    const state = customUAFormState(uaSelect.value);
    customUAGroup.hidden = !state.visible;
    customUAInputs.forEach(input => {
      input.required = state.required;
    });
  }
  toggleCustomUAFields();
  uaSelect.addEventListener('change', toggleCustomUAFields);

	const dynamicEnabledInput = document.getElementById('m-dynamic-enabled');
	const dynamicProfileSelect = document.getElementById('m-dynamic-profile');
	const dynamicDowngradeInput = document.getElementById('m-dynamic-downgrade');
	const dynamicSourceInputs = DYNAMIC_SOURCE_IDS.map(source => document.getElementById(`m-dynamic-source-${source}`));
	const dynamicRulesContainer = document.getElementById('m-dynamic-rules');
	const dynamicProfileRiskContainer = document.getElementById('m-dynamic-profile-risk');
	const dynamicExtremeConfirmation = document.getElementById('m-dynamic-extreme-confirm');
	const dynamicExtremeAcknowledged = document.getElementById('m-dynamic-extreme-ack');
	const dynamicExtremeTypedName = document.getElementById('m-dynamic-extreme-name');
	function updateDynamicSourceAvailability() {
		if (!dynamicPolicyEditable) {
			dynamicSourceInputs.forEach(input => { input.disabled = true; });
			return;
		}
		const allowed = new Set(dynamicSourcesForProfile(dynamicProfileSelect.value));
		dynamicSourceInputs.forEach((input, index) => {
			const sourceAllowed = allowed.has(DYNAMIC_SOURCE_IDS[index]);
			input.disabled = !sourceAllowed;
			if (!sourceAllowed) input.checked = false;
		});
	}


	function updateDynamicProfileRisk() {
		dynamicProfileRiskContainer.innerHTML = renderDynamicProfileRisk(dynamicProfileSelect.value);
		const requirement = dynamicProfileConfirmationRequirement(dynamicPolicy, {
			dynamic_discovery_enabled: dynamicEnabledInput.checked,
			dynamic_profile: dynamicProfileSelect.value,
		});
		dynamicExtremeConfirmation.hidden = requirement !== 'extreme';
		if (dynamicExtremeConfirmation.hidden) {
			dynamicExtremeAcknowledged.checked = false;
			dynamicExtremeTypedName.value = '';
		}
	}
	dynamicProfileSelect.addEventListener('change', () => {
		dynamicExtremeAcknowledged.checked = false;
		dynamicExtremeTypedName.value = '';
		updateDynamicSourceAvailability();
		updateDynamicProfileRisk();
	});
	dynamicEnabledInput.addEventListener('change', updateDynamicProfileRisk);
	updateDynamicSourceAvailability();
	updateDynamicProfileRisk();
	let dynamicRules = dynamicPolicy.dynamic_domain_rules.map(rule => ({ ...rule }));
	if (dynamicRules.length === 0) dynamicRules.push({ type: 'exact', value: '' });

	function renderDynamicRules() {
		dynamicRulesContainer.innerHTML = renderDynamicRuleRows(dynamicRules);
		dynamicRulesContainer.querySelectorAll('.m-dynamic-rule-type').forEach(input => {
			input.onchange = () => { dynamicRules[Number(input.dataset.idx)].type = input.value; };
		});
		dynamicRulesContainer.querySelectorAll('.m-dynamic-rule-value').forEach(input => {
			input.oninput = () => { dynamicRules[Number(input.dataset.idx)].value = input.value; };
		});
		dynamicRulesContainer.querySelectorAll('.m-dynamic-rule-remove').forEach(button => {
			button.onclick = () => {
				dynamicRules.splice(Number(button.dataset.idx), 1);
				renderDynamicRules();
			};
		});
	}
	renderDynamicRules();

	document.getElementById('m-add-dynamic-rule').onclick = () => {
		dynamicRules.push({ type: 'exact', value: '' });
		renderDynamicRules();
		const inputs = dynamicRulesContainer.querySelectorAll('.m-dynamic-rule-value');
		if (inputs.length) inputs[inputs.length - 1].focus();
	};

	const dynamicObservationsContainer = isEdit ? document.getElementById('m-dynamic-observations') : null;
	const refreshDynamicObservationsButton = isEdit ? document.getElementById('m-refresh-dynamic-observations') : null;
	const clearDynamicObservationsButton = isEdit ? document.getElementById('m-clear-dynamic-observations') : null;
	let dynamicObservationRequest = 0;

	function dynamicObservationsPanelIsCurrent() {
		return dynamicObservationsContainer && document.getElementById('m-dynamic-observations') === dynamicObservationsContainer;
	}

	function setDynamicObservationButtonsDisabled(disabled) {
		if (refreshDynamicObservationsButton) refreshDynamicObservationsButton.disabled = disabled;
		if (clearDynamicObservationsButton) clearDynamicObservationsButton.disabled = disabled;
	}

	async function refreshDynamicObservations() {
		if (!isEdit || !dynamicCapabilities.recognized || !dynamicObservationsContainer) return;
		const request = ++dynamicObservationRequest;
		setDynamicObservationButtonsDisabled(true);
		dynamicObservationsContainer.innerHTML = '<div class="form-help">正在读取观察记录…</div>';
		try {
			const response = await API.getDynamicObservations(site.id);
			if (request !== dynamicObservationRequest || !dynamicObservationsPanelIsCurrent()) return;
			dynamicObservationsContainer.innerHTML = renderDynamicObservations(response);
		} catch (_) {
			if (request !== dynamicObservationRequest || !dynamicObservationsPanelIsCurrent()) return;
			dynamicObservationsContainer.innerHTML = '<div class="form-help" style="color:var(--orange)">无法读取观察记录；未显示任何后端返回值。</div>';
		} finally {
			if (request === dynamicObservationRequest && dynamicObservationsPanelIsCurrent()) setDynamicObservationButtonsDisabled(false);
		}
	}

	if (isEdit && dynamicCapabilities.recognized) {
		refreshDynamicObservationsButton.onclick = refreshDynamicObservations;
		clearDynamicObservationsButton.onclick = async () => {
			if (!window.confirm('确定清空本站点的全部动态观察记录吗？此操作不可撤销。')) return;
			const request = ++dynamicObservationRequest;
			setDynamicObservationButtonsDisabled(true);
			try {
				const response = await API.deleteDynamicObservations(site.id);
				if (request !== dynamicObservationRequest || !dynamicObservationsPanelIsCurrent()) return;
				dynamicObservationsContainer.innerHTML = renderDynamicObservations(response);
				Toast.success('观察记录已清空');
			} catch (_) {
				if (request === dynamicObservationRequest && dynamicObservationsPanelIsCurrent()) Toast.error('无法清空观察记录');
			} finally {
				if (request === dynamicObservationRequest && dynamicObservationsPanelIsCurrent()) setDynamicObservationButtonsDisabled(false);
			}
		};
	}

  // Build initial playback list from existing data
  const listContainer = document.getElementById('m-playback-list');
  const modeGroup = document.getElementById('playback-mode-group');
  const addPlaybackButton = document.getElementById('m-add-playback');
  let existingHosts = [];
  if (isEdit) {
    if ((site.playback_target_url || '').trim()) existingHosts.push(site.playback_target_url.trim());
    for (const host of normalizeStreamHosts(site.stream_hosts)) existingHosts.push(host);
  }
  if (existingHosts.length === 0) existingHosts = [''];

  function updatePlaybackAddState() {
    const canAdd = canAddPlaybackAddress(existingHosts.length, maxPlaybackAddresses);
    addPlaybackButton.disabled = !canAdd;
    addPlaybackButton.title = canAdd ? '' : `每个站点最多配置 ${maxPlaybackAddresses} 个播放回源`;
  }

  function renderPlaybackInputs() {
    listContainer.innerHTML = existingHosts.map((val, idx) => `
      <div style="display:flex;gap:6px;margin-bottom:6px;align-items:center">
        <input type="text" class="form-input m-pb-input" value="${esc(val)}" placeholder="${idx === 0 ? '主播放回源地址' : '额外播放回源地址'}" inputmode="url" autocapitalize="none" autocorrect="off" spellcheck="false" maxlength="2048" style="flex:1">
        ${existingHosts.length > 1 ? `<button type="button" class="btn-ghost danger m-pb-remove" data-idx="${idx}" style="padding:4px 8px;font-size:13px;flex-shrink:0">删除</button>` : ''}
      </div>
    `).join('');
    listContainer.querySelectorAll('.m-pb-remove').forEach(btn => {
      btn.onclick = () => {
        existingHosts.splice(parseInt(btn.dataset.idx), 1);
        renderPlaybackInputs();
        toggleModeGroup();
      };
    });
    listContainer.querySelectorAll('.m-pb-input').forEach((inp, idx) => {
      inp.oninput = () => { existingHosts[idx] = inp.value; toggleModeGroup(); };
    });
    updatePlaybackAddState();
  }
  renderPlaybackInputs();

  addPlaybackButton.onclick = () => {
    if (!canAddPlaybackAddress(existingHosts.length, maxPlaybackAddresses)) {
      Toast.error(`每个站点最多配置 ${maxPlaybackAddresses} 个播放回源`);
      return;
    }
    existingHosts.push('');
    renderPlaybackInputs();
    const inputs = listContainer.querySelectorAll('.m-pb-input');
    if (inputs.length) inputs[inputs.length - 1].focus();
  };

  function toggleModeGroup() {
    const hasAny = existingHosts.some(h => h.trim());
    modeGroup.style.display = hasAny ? '' : 'none';
  }
  toggleModeGroup();

  const upstreamHeadersContainer = document.getElementById('m-upstream-headers');
  let upstreamHeaders = isEdit && Array.isArray(site.upstream_headers)
    ? site.upstream_headers.map(header => ({ name: header.name || '', value: '', configured: !!header.configured }))
    : [];

  function renderUpstreamHeaders() {
    upstreamHeadersContainer.innerHTML = renderUpstreamHeaderRows(upstreamHeaders, upstreamHeadersAvailable);
    upstreamHeadersContainer.querySelectorAll('.m-upstream-header-name').forEach(input => {
      input.oninput = () => { upstreamHeaders[Number(input.dataset.idx)].name = input.value; };
    });
    upstreamHeadersContainer.querySelectorAll('.m-upstream-header-value').forEach(input => {
      input.oninput = () => { upstreamHeaders[Number(input.dataset.idx)].value = input.value; };
    });
    upstreamHeadersContainer.querySelectorAll('.m-upstream-header-remove').forEach(button => {
      button.onclick = () => {
        upstreamHeaders.splice(Number(button.dataset.idx), 1);
        renderUpstreamHeaders();
      };
    });
  }
  renderUpstreamHeaders();

  const addUpstreamHeaderButton = document.getElementById('m-add-upstream-header');
  addUpstreamHeaderButton.onclick = () => {
    if (!upstreamHeadersAvailable) {
      Toast.error('请先配置 UPSTREAM_HEADER_KEY 并重启 Meridian');
      return;
    }
    if (upstreamHeaders.length >= 16) {
      Toast.error('每个站点最多配置 16 个上游请求头');
      return;
    }
    upstreamHeaders.push({ name: '', value: '', configured: false });
    renderUpstreamHeaders();
    const inputs = upstreamHeadersContainer.querySelectorAll('.m-upstream-header-name');
    if (inputs.length) inputs[inputs.length - 1].focus();
  };

  document.getElementById('m-submit').onclick = async () => {
    const allHosts = existingHosts.map(h => h.trim()).filter(Boolean);
    if (allHosts.length > maxPlaybackAddresses) {
      Toast.error(`每个站点最多配置 ${maxPlaybackAddresses} 个播放回源`);
      return;
    }
    const uaMode = uaSelect.value;
    const customUAPayload = buildCustomUAPayload(
      uaMode,
      customUAInputs[0].value,
      customUAInputs[1].value,
      customUAInputs[2].value,
    );
		const ingressPayload = buildIngressPayload(
		  ingressSelect.value,
		  document.getElementById('m-port').value,
		  publicHostInput.value,
		);
		const data = {
	      name: document.getElementById('m-name').value.trim(),
	      target_url: document.getElementById('m-target').value.trim(),
      playback_target_url: allHosts.length > 0 ? allHosts[0] : '',
      playback_mode: document.getElementById('m-playback-mode').value,
		stream_hosts: allHosts.length > 1 ? allHosts.slice(1) : [],
			...ingressPayload,
		upstream_headers: buildUpstreamHeaderPayload(upstreamHeaders),
      ua_mode: uaMode,
      ...customUAPayload,
      ...buildDynamicPolicyPayload({
        dynamic_discovery_enabled: dynamicEnabledInput.checked,
        dynamic_profile: dynamicProfileSelect.value,
		  dynamic_discovery_sources: DYNAMIC_SOURCE_IDS.filter((source, index) => dynamicSourceInputs[index].checked),
        dynamic_domain_rules: dynamicRules,
        dynamic_allow_https_downgrade: dynamicDowngradeInput.checked,
      }, dynamicCapabilities),
      traffic_quota: parseInt(document.getElementById('m-quota').value || 0) * 1073741824,
      speed_limit: parseInt(document.getElementById('m-speed').value || 0),
    };

		if (!data.name || !data.target_url || !data.listen_port || ((data.ingress_mode === 'host' || data.ingress_mode === 'both') && !data.public_host)) {
	      Toast.error('请填写所有必填项');
	      return;
	    }
	  if (data.dynamic_discovery_enabled && data.dynamic_discovery_sources.length === 0) {
		Toast.error('启用自动播放后端发现时，至少选择一种发现来源');
		return;
	  }
	  if (data.dynamic_discovery_enabled && !hasRequiredSafeDynamicRules(data.dynamic_profile, data.dynamic_domain_rules)) {
		Toast.error('启用 Safe 安全配置时，至少需要一条有效的精确或后缀 DNS 域名规则');
		return;
	  }
	  const profileConfirmation = confirmDynamicProfileChange(
		dynamicPolicy,
		data,
		data.name,
		dynamicExtremeAcknowledged.checked,
		dynamicExtremeTypedName.value,
	  );
	  if (!profileConfirmation.ok) {
		if (profileConfirmation.error) Toast.error(profileConfirmation.error);
		return;
	  }
	  if (uaMode === 'custom' && (!data.custom_user_agent || !data.custom_client || !data.custom_version)) {
      Toast.error('请完整填写自定义 User-Agent、Client 和 Version');
		return;
	  }
	  const invalidHeader = upstreamHeaders.some(header => {
		const name = String(header.name || '').trim();
		const value = String(header.value || '').trim();
		if (!header.configured && !name && !value) return false;
		return !name || (!header.configured && !value);
	  });
		if (invalidHeader) {
			Toast.error('请完整填写新增请求头的名称和值；已有值可留空保持不变');
			return;
		}
		if (isEdit && normalizedTargetAuthority(site.target_url) !== normalizedTargetAuthority(data.target_url)) {
			const retainedSecret = upstreamHeaders.some(header => header.configured && !String(header.value || '').trim());
			if (retainedSecret) {
				Toast.error('主回源的协议、域名或端口已变化，请重新输入每个已配置的固定请求头，或删除对应行');
				return;
			}
		}

    try {
      if (isEdit) {
        await API.updateSite(site.id, data);
        Toast.success('站点已更新');
      } else {
        await API.createSite(data);
        Toast.success('站点已创建');
      }
      closeModal();
      loadSites();
    } catch (e) {
      Toast.error(e.message);
    }
  };

  openModal({ closeOnBackdrop: false });
	if (isEdit && dynamicCapabilities.recognized) refreshDynamicObservations();
}

// Global actions
window.toggleSiteAction = async function(id) {
  try {
    const res = await API.toggleSite(id);
    Toast.success(res.enabled ? '站点已启用' : '站点已停用');
    loadSites();
  } catch (e) {
    Toast.error(e.message);
  }
};

window.editSiteAction = async function(id) {
  try {
    const sites = await API.listSites();
    const site = sites.find(s => s.id === id);
    if (site) showSiteModal(site);
  } catch (e) {
    Toast.error(e.message);
  }
};

window.deleteSiteAction = function(id, name) {
  document.getElementById('modal-title').textContent = '确认删除';
  const modalBody = document.getElementById('modal-body');
  modalBody.replaceChildren();
  const message = document.createElement('p');
  message.style.color = 'var(--white-60)';
  message.append('确定要删除站点 ');
  const strong = document.createElement('strong');
  strong.textContent = String(name);
  message.append(strong, ' 吗？此操作不可撤销。');
  modalBody.appendChild(message);
  document.getElementById('modal-footer').innerHTML = `
    <button class="btn-modal secondary" id="delete-cancel">取消</button>
    <button class="btn-modal primary" id="delete-confirm" style="background:var(--red)">删除</button>
  `;
  document.getElementById('delete-cancel').addEventListener('click', closeModal);
  document.getElementById('delete-confirm').addEventListener('click', () => confirmDelete(id));
  openModal({ closeOnBackdrop: true });
};

window.confirmDelete = async function(id) {
  try {
    await API.deleteSite(id);
    Toast.success('站点已删除');
    closeModal();
    loadSites();
  } catch (e) {
    Toast.error(e.message);
  }
};
