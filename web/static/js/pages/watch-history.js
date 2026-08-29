// Viewing history page
const WATCH_HISTORY_PAGE_SIZE = 24;
const WATCH_HISTORY_REFRESH_INTERVAL_MS = 5000;
const WATCH_HISTORY_TICKS_PER_SECOND = 10000000;
let watchHistoryLoadGeneration = 0;
let watchHistoryRefreshTimer = 0;
let watchHistoryKeydownHandler = null;
let watchHistoryDetailItemID = '';
let watchHistoryDetailSource = 'history';
let watchHistorySitesCache = [];
let watchHistoryRenderedItemsSignature = '';
let watchHistoryRenderedLiveSignature = '';
let watchHistoryState = {
  siteId: 'all',
  mediaType: 'all',
  range: 'all',
  items: [],
  liveItems: [],
  nextCursor: '',
  hasMore: false,
  loading: false,
  tmdbConfigured: null,
  tmdbInvalid: false,
  sitesLoadError: '',
  tmdbLoadError: '',
  liveLoadError: '',
};

function watchHistoryTypeLabel(value) {
  const type = String(value || '').trim().toLowerCase();
  return ({
    movie: '电影',
    episode: '剧集',
    series: '剧集',
    video: '视频',
    audio: '音频',
    musicvideo: '音乐视频',
    trailer: '预告片',
  })[type] || '其他';
}

function watchHistoryRangeStart(range, now = Date.now()) {
  const value = String(range || 'all');
  if (value === 'today') {
    if (typeof meridianDateOnlyValue === 'function' && typeof meridianParseDateOnly === 'function') {
      return meridianParseDateOnly(meridianDateOnlyValue(now), false);
    }
    const date = new Date(now);
    date.setHours(0, 0, 0, 0);
    return date.getTime();
  }
  if (value === '7d') return now - (7 * 24 * 60 * 60 * 1000);
  if (value === '30d') return now - (30 * 24 * 60 * 60 * 1000);
  return 0;
}

function watchHistoryProgress(item) {
  const explicit = Number(item && item.progress_percent);
  if (Number.isFinite(explicit)) return Math.max(0, Math.min(100, explicit));
  const position = Number(item && item.position_ticks);
  const runtime = Number(item && item.runtime_ticks);
  if (!Number.isFinite(position) || !Number.isFinite(runtime) || runtime <= 0) return 0;
  return Math.max(0, Math.min(100, (position / runtime) * 100));
}

function watchHistoryDurationLabel(ticks) {
  const value = Number(ticks);
  if (!Number.isFinite(value) || value < 0) return '';
  const totalSeconds = Math.floor(value / WATCH_HISTORY_TICKS_PER_SECOND);
  const hours = Math.floor(totalSeconds / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;
  if (hours > 0) return `${hours}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`;
  return `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`;
}

function watchHistoryTimeProgress(item) {
  const runtime = Number(item && item.runtime_ticks);
  const position = Number(item && item.position_ticks);
  if (!Number.isFinite(runtime) || runtime <= 0 || !Number.isFinite(position)) return '';
  const boundedPosition = Math.max(0, Math.min(position, runtime));
  const watched = watchHistoryDurationLabel(boundedPosition);
  const total = watchHistoryDurationLabel(runtime);
  return watched && total ? `${watched} / ${total}` : '';
}

function watchHistoryElapsedLabel(item) {
  const runtime = Number(item && item.runtime_ticks);
  const position = Number(item && item.position_ticks);
  if (!Number.isFinite(runtime) || runtime <= 0 || !Number.isFinite(position)) return '';
  return watchHistoryDurationLabel(Math.max(0, Math.min(position, runtime)));
}

function watchHistoryPosterURL(item) {
  if (!item || (item.poster_available !== true && !item.poster_path)) return '';
  const mediaItemID = Number(item.media_item_id);
  if (!Number.isSafeInteger(mediaItemID) || mediaItemID <= 0) return '';
  if (typeof API !== 'undefined' && typeof API.watchHistoryPosterURL === 'function') {
    return API.watchHistoryPosterURL(mediaItemID);
  }
  return '/api/watch-history/posters/' + mediaItemID;
}

function watchHistoryBackdropURL(item) {
  if (!item || !String(item.backdrop_path || '').trim()) return '';
  const mediaItemID = Number(item.media_item_id);
  if (!Number.isSafeInteger(mediaItemID) || mediaItemID <= 0) return '';
  if (typeof API !== 'undefined' && typeof API.watchHistoryBackdropURL === 'function') {
    return API.watchHistoryBackdropURL(mediaItemID);
  }
  return '/api/watch-history/backdrops/' + mediaItemID;
}

function watchHistoryCastURL(item, index) {
  const mediaItemID = Number(item && item.media_item_id);
  const position = Number(index);
  if (!Number.isSafeInteger(mediaItemID) || mediaItemID <= 0 || !Number.isSafeInteger(position) || position < 0 || position >= 20) return '';
  if (typeof API !== 'undefined' && typeof API.watchHistoryCastURL === 'function') {
    return API.watchHistoryCastURL(mediaItemID, position);
  }
  return `/api/watch-history/cast/${mediaItemID}/${position}`;
}

function watchHistoryBackgroundURLs(item) {
  if (!item) return [];
  const urls = [];
  const backdrop = watchHistoryBackdropURL(item);
  if (backdrop) urls.push(backdrop);
  const stills = watchHistoryStillURLs(item);
  if (stills.length) urls.push(stills[0]);
  const poster = watchHistoryPosterURL(item);
  if (poster) urls.push(poster);
  return [...new Set(urls)];
}

function watchHistoryCardImageURL(item) {
  return watchHistoryPosterURL(item) || watchHistoryBackdropURL(item) || watchHistoryStillURLs(item)[0] || '';
}

// Live playback is presented as a horizontal media moment. Match the detail
// view's image priority so the card uses a backdrop first, rather than the
// portrait artwork reserved for library cards.
function watchHistoryLiveCardImageURLs(item) {
  return watchHistoryBackgroundURLs(item);
}

function watchHistoryStillURLs(item) {
  if (!item) return [];
  const mediaItemID = Number(item.media_item_id);
  if (!Number.isSafeInteger(mediaItemID) || mediaItemID <= 0 || !Array.isArray(item.stills)) return [];
  return item.stills.slice(0, 12).map((path, index) => {
    if (!String(path || '').trim()) return '';
    if (typeof API !== 'undefined' && typeof API.watchHistoryStillURL === 'function') {
      return API.watchHistoryStillURL(mediaItemID, index);
    }
    return '/api/watch-history/stills/' + mediaItemID + '/' + index;
  }).filter(Boolean);
}

function watchHistoryFormatTime(value) {
  const timestamp = Number(value);
  if (!Number.isFinite(timestamp) || timestamp <= 0 || timestamp > 8640000000000000) return '时间未知';
  if (typeof meridianFormatDateTime === 'function') return meridianFormatDateTime(timestamp);
  return new Date(timestamp).toLocaleString('zh-CN', { hour12: false });
}

function watchHistoryEpisodeLabel(item) {
  if (String((item && item.media_type) || '').trim().toLowerCase() !== 'episode') return '';
  const series = String((item && item.series_name) || '').trim();
  const season = Number(item && item.season_number);
  const episode = Number(item && item.episode_number);
  const index = Number.isInteger(season) && season >= 0 && Number.isInteger(episode) && episode > 0
    ? `S${String(season).padStart(2, '0')}E${String(episode).padStart(2, '0')}`
    : '';
  return [series, index].filter(Boolean).join(' · ');
}

function watchHistoryIdentityLabel(item) {
  const user = String((item && (item.user_name || item.user_id)) || '').trim();
  const client = String((item && item.client_name) || '').trim();
  return [user, client].filter(Boolean).join(' · ');
}

function watchHistoryCardHTML(item, index, context = {}) {
  const title = String((item && (item.title || item.name)) || '').trim() || '未知媒体';
  const mediaType = String((item && (item.media_type || item.item_type)) || '').toLowerCase();
  const episodeLabel = watchHistoryEpisodeLabel(item);
  const year = Number(item && (item.production_year || item.year));
  const secondary = episodeLabel || (Number.isInteger(year) && year > 1800 ? String(year) : '');
  const siteName = String((item && item.site_name) || '').trim() || '未知站点';
  const watchedAt = Number(item && (item.last_watched_at_ms || item.last_seen_at_ms || item.started_at_ms));
  const timeText = watchHistoryFormatTime(watchedAt);
  const isoTime = Number.isFinite(watchedAt) && watchedAt > 0 && watchedAt <= 8640000000000000 ? new Date(watchedAt).toISOString() : '';
  const progress = watchHistoryProgress(item);
  const progressRounded = Math.round(progress);
  const elapsedLabel = watchHistoryElapsedLabel(item);
  const playCount = Number(item && item.play_count);
  const source = context && context.source === 'live' ? 'live' : 'history';
  const live = context && context.live === true;
  const imageURLs = live ? watchHistoryLiveCardImageURLs(item) : [watchHistoryCardImageURL(item)].filter(Boolean);
  const posterURL = imageURLs[0] || '';
  const imageFallbacks = imageURLs.slice(1);
  const pending = watchHistoryState.tmdbConfigured === true && String(item && (item.match_status || item.metadata_status) || '').toLowerCase() === 'pending';
  const safeIndex = Number.isSafeInteger(Number(index)) ? Number(index) : 0;
  const titleID = `watch-history-title-${source}-${safeIndex}`;
  const identity = watchHistoryIdentityLabel(item);

  return `<article class="watch-history-card${live ? ' is-live' : ''}" role="button" tabindex="0" data-history-index="${safeIndex}" data-history-source="${source}" aria-labelledby="${titleID}" aria-haspopup="dialog">
    <div class="watch-history-poster">
      ${posterURL ? `<img src="${posterURL}"${imageFallbacks.length ? ` data-watch-history-card-fallbacks="${esc(JSON.stringify(imageFallbacks))}"` : ''} alt="" loading="lazy" decoding="async">` : ''}
      <span class="watch-history-poster-fallback" aria-hidden="true">
        <svg viewBox="0 0 24 24"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="m8 10 3 2-3 2z"/><path d="M15 8h3M15 12h3M15 16h3"/></svg>
      </span>
      <span class="watch-history-type">${esc(watchHistoryTypeLabel(mediaType))}</span>
      ${progress > 0 && elapsedLabel ? `<span class="watch-history-progress-time" aria-hidden="true">${esc(elapsedLabel)}</span>` : ''}
      ${progress > 0 ? `<div class="watch-history-progress" role="progressbar" aria-label="观看进度 ${progressRounded}%" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${progressRounded}"><span style="width:${progress}%"></span></div>` : ''}
    </div>
    <div class="watch-history-card-body">
      <div class="watch-history-card-heading">
        <h2 id="${titleID}">${esc(title)}</h2>
        ${live ? '<span class="watch-history-live-badge"><i></i>正在观看</span>' : (pending ? '<span class="watch-history-pending">补全中</span>' : '')}
      </div>
      ${secondary ? `<p class="watch-history-secondary">${esc(secondary)}</p>` : ''}
      <p class="watch-history-site"><span>${esc(siteName)}</span>${Number.isInteger(playCount) && playCount > 1 ? `<b>播放 ${playCount} 次</b>` : ''}</p>
      ${identity ? `<p class="watch-history-identity">${esc(identity)}</p>` : ''}
      <time class="watch-history-time" ${isoTime ? `datetime="${isoTime}"` : ''}>${live ? `同步于 ${esc(timeText)}` : esc(timeText)}</time>
    </div>
    <button class="watch-history-card-delete" type="button" hidden aria-label="删除${esc(title)}">删除</button>
  </article>`;
}

function watchHistoryCast(item) {
  const cast = Array.isArray(item && item.cast) ? item.cast : [];
  return cast.map(member => ({
    name: String((member && member.name) || '').trim(),
    character: String((member && member.character) || '').trim(),
    profilePath: String((member && member.profile_path) || '').trim(),
  })).filter(member => member.name).slice(0, 20);
}

function watchHistoryCastInitial(name) {
  const value = String(name || '').trim();
  return value ? value.slice(0, 1).toUpperCase() : '？';
}

function watchHistoryDateLabel(value) {
  const text = String(value || '').trim();
  if (!/^\d{4}-\d{2}-\d{2}$/.test(text)) return '';
  return text.replace(/^(\d{4})-(\d{2})-(\d{2})$/, '$1 年 $2 月 $3 日');
}

function watchHistoryGenres(item) {
  return Array.isArray(item && item.genres)
    ? item.genres.map(value => String(value || '').trim()).filter(Boolean).slice(0, 8)
    : [];
}

function watchHistoryEpisodeInfo(item, mediaType) {
  const isTV = mediaType === 'episode' || mediaType === 'series' || String(item && item.tmdb_type || '').toLowerCase() === 'tv';
  if (!isTV) return '';
  const parts = [];
  const seasons = Number(item && item.season_count);
  const episodes = Number(item && item.episode_count);
  if (Number.isInteger(seasons) && seasons > 0) parts.push(`${seasons} 季`);
  if (Number.isInteger(episodes) && episodes > 0) parts.push(`${episodes} 集`);
  const season = Number(item && item.season_number);
  const episode = Number(item && item.episode_number);
  if (Number.isInteger(season) && season >= 0 && Number.isInteger(episode) && episode > 0) {
    parts.push(`本次 S${String(season).padStart(2, '0')}E${String(episode).padStart(2, '0')}`);
  }
  return parts.join(' · ');
}

function watchHistoryUpdateInfo(item, mediaType) {
  const isTV = mediaType === 'episode' || mediaType === 'series' || String(item && item.tmdb_type || '').toLowerCase() === 'tv';
  if (!isTV) return '';
  const parts = [];
  const rawStatus = String(item && item.status || '').trim();
  const status = ({
    'Returning Series': '连载中',
    'In Production': '制作中',
    'Planned': '已计划',
    'Ended': '已完结',
    'Canceled': '已取消',
    'Pilot': '试播',
  })[rawStatus] || rawStatus;
  if (status) parts.push(status);
  const lastAir = watchHistoryDateLabel(item && item.last_air_date);
  if (lastAir) parts.push(`最近更新 ${lastAir}`);
  const nextAir = watchHistoryDateLabel(item && item.next_air_date);
  const nextSeason = Number(item && item.next_season_number);
  const nextEpisode = Number(item && item.next_episode_number);
  if (nextAir) {
    let next = `下一集 ${nextAir}`;
    if (Number.isInteger(nextSeason) && nextSeason >= 0 && Number.isInteger(nextEpisode) && nextEpisode > 0) {
      next = `下一集 S${String(nextSeason).padStart(2, '0')}E${String(nextEpisode).padStart(2, '0')} · ${nextAir}`;
    }
    parts.push(next);
  }
  return parts.join(' · ');
}

function watchHistoryDetailsHTML(item) {
  const title = String((item && (item.title || item.name)) || '').trim() || '未知媒体';
  const mediaType = String((item && (item.media_type || item.item_type)) || '').toLowerCase();
  const episode = watchHistoryEpisodeLabel(item);
  const year = Number(item && (item.production_year || item.year));
  const secondary = episode || (Number.isInteger(year) && year > 1800 ? String(year) : '');
  const siteName = String((item && item.site_name) || '').trim() || '未知站点';
  const overview = String((item && item.overview) || '').trim();
  const progress = Math.round(watchHistoryProgress(item));
  const timeProgress = watchHistoryTimeProgress(item);
  const playMethod = String((item && item.play_method) || '').trim();
  const posterURL = watchHistoryPosterURL(item);
  const backgroundURLs = watchHistoryBackgroundURLs(item);
  const genres = watchHistoryGenres(item);
  const releaseDate = watchHistoryDateLabel(item && item.release_date) || (year > 1800 ? String(year) : '');
  const rating = Number(item && item.vote_average);
  const ratingText = Number.isFinite(rating) && rating > 0 ? `${Math.min(10, Math.max(0, rating)).toFixed(1)} / 10` : '';
  const episodeInfo = watchHistoryEpisodeInfo(item, mediaType);
  const updateInfo = watchHistoryUpdateInfo(item, mediaType);
  const viewerName = String((item && (item.user_name || item.user_id)) || '').trim();
  const clientName = String((item && item.client_name) || '').trim();
  const stillURLs = watchHistoryStillURLs(item);
  const cast = watchHistoryCast(item);
  const castHTML = cast.length
    ? `<ul class="watch-history-detail-cast">${cast.map((member, index) => `<li><div class="watch-history-cast-avatar"><span class="watch-history-cast-avatar-fallback" aria-hidden="true">${esc(watchHistoryCastInitial(member.name))}</span>${member.profilePath ? `<img src="${esc(watchHistoryCastURL(item, index))}" alt="" loading="lazy" decoding="async">` : ''}</div><div class="watch-history-cast-copy"><strong>${esc(member.name)}</strong>${member.character ? `<span>${esc(member.character)}</span>` : ''}</div></li>`).join('')}</ul>`
    : '<p class="watch-history-detail-muted">暂无演员资料，等待 TMDB 资料补全。</p>';
  const stillsHTML = stillURLs.length
    ? `<div class="watch-history-detail-stills">${stillURLs.map((url, index) => `<button type="button" class="watch-history-detail-still" data-watch-history-image="${esc(url)}" data-watch-history-image-alt="剧照 ${index + 1}" aria-label="放大查看剧照 ${index + 1}"><img src="${esc(url)}" alt="剧照 ${index + 1}" loading="lazy" decoding="async"></button>`).join('')}</div>`
    : posterURL
      ? `<div class="watch-history-detail-stills watch-history-detail-stills-fallback"><button type="button" class="watch-history-detail-still" data-watch-history-image="${esc(posterURL)}" data-watch-history-image-alt="海报" aria-label="放大查看海报"><img src="${esc(posterURL)}" alt="海报" loading="lazy" decoding="async"></button></div>`
      : '<p class="watch-history-detail-muted">暂无剧照或海报，等待 TMDB 资料补全。</p>';
  return `<div class="watch-history-detail-shell">
    ${backgroundURLs.length ? `<div class="watch-history-detail-background" data-watch-history-image="${esc(backgroundURLs[0])}" data-watch-history-image-alt="背景剧照" role="button" tabindex="0" aria-label="放大查看背景剧照"><img src="${esc(backgroundURLs[0])}" data-watch-history-background-fallbacks="${esc(JSON.stringify(backgroundURLs.slice(1)))}" alt=""></div>` : ''}
    <div class="watch-history-detail-layout">
      <div class="watch-history-detail-main">
      <div class="watch-history-detail-heading"><span class="watch-history-type">${esc(watchHistoryTypeLabel(mediaType))}</span><h2 id="watch-history-detail-title">${esc(title)}</h2></div>
      ${secondary ? `<p class="watch-history-detail-secondary">${esc(secondary)}</p>` : ''}
      <dl class="watch-history-detail-meta"><div><dt>站点</dt><dd>${esc(siteName)}</dd></div><div><dt>观看时间</dt><dd>${esc(watchHistoryFormatTime(item && (item.last_seen_at_ms || item.started_at_ms)))}</dd></div>${viewerName ? `<div><dt>观看用户</dt><dd>${esc(viewerName)}</dd></div>` : ''}${clientName ? `<div><dt>播放客户端</dt><dd>${esc(clientName)}</dd></div>` : ''}${releaseDate ? `<div><dt>上映时间</dt><dd>${esc(releaseDate)}</dd></div>` : ''}${genres.length ? `<div><dt>影片类型</dt><dd>${esc(genres.join(' · '))}</dd></div>` : ''}${ratingText ? `<div><dt>评分</dt><dd class="watch-history-detail-rating">★ ${esc(ratingText)}</dd></div>` : ''}${episodeInfo ? `<div><dt>集数信息</dt><dd>${esc(episodeInfo)}</dd></div>` : ''}<div><dt>观看进度</dt><dd>${timeProgress ? `${esc(timeProgress)} · ` : ''}${progress}%</dd></div>${playMethod ? `<div><dt>播放方式</dt><dd>${esc(playMethod)}</dd></div>` : ''}</dl>
      ${updateInfo ? `<section class="watch-history-detail-section watch-history-detail-update"><h3>更新信息</h3><p>${esc(updateInfo)}</p></section>` : ''}
      <section class="watch-history-detail-section"><h3>简介</h3><p>${overview ? esc(overview) : '暂无简介，等待 TMDB 资料补全。'}</p></section>
      <section class="watch-history-detail-section"><h3>演员表</h3>${castHTML}</section>
      <section class="watch-history-detail-section"><h3>剧照</h3>${stillsHTML}</section>
      </div>
    </div>
  </div>`;
}

function watchHistoryItemsForSource(source) {
  return source === 'live' ? watchHistoryState.liveItems : watchHistoryState.items;
}

function watchHistoryOpenDetails(index, source = 'history') {
  const modal = document.getElementById('watch-history-detail-modal');
  const content = document.getElementById('watch-history-detail-content');
  const normalizedSource = source === 'live' ? 'live' : 'history';
  const item = watchHistoryItemsForSource(normalizedSource)[Number(index)];
  if (!modal || !content || !item) return;
  watchHistoryCloseImageViewer();
  watchHistoryDetailItemID = String(item && item.id || '');
  watchHistoryDetailSource = normalizedSource;
  content.innerHTML = watchHistoryDetailsHTML(item);
  watchHistoryBindDetailImages(content);
  modal.hidden = false;
  document.body.classList.add('watch-history-dialog-open');
  const close = modal.querySelector('[data-watch-history-close]');
  if (close) close.focus();
}

function watchHistoryCloseDetails() {
  const modal = document.getElementById('watch-history-detail-modal');
  if (!modal) return;
  watchHistoryCloseImageViewer();
  modal.hidden = true;
  document.body.classList.remove('watch-history-dialog-open');
  watchHistoryDetailItemID = '';
  watchHistoryDetailSource = 'history';
}

function watchHistoryOpenImageViewer(source, alt) {
  const modal = document.getElementById('watch-history-image-modal');
  const image = document.getElementById('watch-history-image-viewer-image');
  if (!modal || !image || !String(source || '').trim()) return;
  image.src = String(source);
  image.alt = String(alt || '放大图片');
  modal.hidden = false;
  document.body.classList.add('watch-history-image-open');
  const close = modal.querySelector('[data-watch-history-image-close]');
  if (close) close.focus();
}

function watchHistoryCloseImageViewer() {
  const modal = document.getElementById('watch-history-image-modal');
  if (!modal || modal.hidden) return false;
  modal.hidden = true;
  document.body.classList.remove('watch-history-image-open');
  const image = document.getElementById('watch-history-image-viewer-image');
  if (image) {
    image.removeAttribute('src');
    image.alt = '';
  }
  return true;
}

function watchHistoryRefreshOpenDetails() {
  const modal = document.getElementById('watch-history-detail-modal');
  const content = document.getElementById('watch-history-detail-content');
  if (!modal || modal.hidden || !content || !watchHistoryDetailItemID) return;
  const item = watchHistoryItemsForSource(watchHistoryDetailSource).find(candidate => String(candidate && candidate.id || '') === watchHistoryDetailItemID);
  if (!item) {
    watchHistoryCloseImageViewer();
    watchHistoryCloseDetails();
    return;
  }
  content.innerHTML = watchHistoryDetailsHTML(item);
  watchHistoryBindDetailImages(content);
}

function watchHistoryBindDetailImages(root) {
  if (!root) return;
  root.querySelectorAll('[data-watch-history-background-fallbacks]').forEach(image => {
    image.addEventListener('error', () => {
      let fallbacks = [];
      try { fallbacks = JSON.parse(image.getAttribute('data-watch-history-background-fallbacks') || '[]'); } catch (_) { fallbacks = []; }
      const next = String(fallbacks.shift() || '');
      if (next) {
        image.setAttribute('data-watch-history-background-fallbacks', JSON.stringify(fallbacks));
        image.src = next;
      } else {
        image.remove();
      }
    });
  });
  root.querySelectorAll('[data-watch-history-image]').forEach(trigger => {
    const open = event => {
      if (event) event.stopPropagation();
      watchHistoryOpenImageViewer(trigger.getAttribute('data-watch-history-image'), trigger.getAttribute('data-watch-history-image-alt'));
    };
    trigger.addEventListener('click', open);
    trigger.addEventListener('keydown', event => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        open();
      }
    });
  });
  root.querySelectorAll('.watch-history-detail-cast img, .watch-history-detail-stills img').forEach(image => {
    image.addEventListener('error', () => {
      const button = image.closest('.watch-history-detail-still');
      if (button) button.hidden = true;
      else image.hidden = true;
    }, { once: true });
  });
}

let watchHistoryLongPressTimer = 0;
let watchHistoryLongPressTriggered = false;

function watchHistoryShowDeleteAction(card) {
  if (!card) return;
  document.querySelectorAll('.watch-history-card.is-long-pressed').forEach(other => {
    if (other !== card) {
      other.classList.remove('is-long-pressed');
      const button = other.querySelector('.watch-history-card-delete');
      if (button) button.hidden = true;
    }
  });
  card.classList.add('is-long-pressed');
  const button = card.querySelector('.watch-history-card-delete');
  if (button) button.hidden = false;
  watchHistoryLongPressTriggered = true;
}

async function watchHistoryDeleteItem(item) {
  const historyID = Number(item && item.id);
  if (!Number.isSafeInteger(historyID) || historyID <= 0) return;
  try {
    const result = typeof API !== 'undefined' && typeof API.deleteWatchHistory === 'function' ? await API.deleteWatchHistory(historyID) : null;
    if (!result) return;
    watchHistoryState.items = watchHistoryState.items.filter(candidate => Number(candidate && candidate.id) !== historyID);
    watchHistoryState.liveItems = watchHistoryState.liveItems.filter(candidate => Number(candidate && candidate.id) !== historyID);
    if (watchHistoryDetailItemID === String(historyID)) watchHistoryCloseDetails();
    watchHistoryRenderItems();
    Toast.success('观看记录已删除');
  } catch (error) {
    Toast.error(error.message || '删除观看记录失败');
  }
}

function watchHistoryStartLongPress(card) {
  window.clearTimeout(watchHistoryLongPressTimer);
  watchHistoryLongPressTriggered = false;
  watchHistoryLongPressTimer = window.setTimeout(() => {
    watchHistoryLongPressTimer = 0;
    watchHistoryShowDeleteAction(card);
  }, 680);
}

function watchHistoryCancelLongPress() {
  if (watchHistoryLongPressTimer) window.clearTimeout(watchHistoryLongPressTimer);
  watchHistoryLongPressTimer = 0;
}

function watchHistoryBindCards(grid) {
  grid = grid || document.getElementById('watch-history-grid');
  if (!grid) return;
  watchHistoryBindCardImages(grid);
  grid.querySelectorAll('.watch-history-card[data-history-index]').forEach(card => {
    const open = () => watchHistoryOpenDetails(card.dataset.historyIndex, card.dataset.historySource);
    const deleteButton = card.querySelector('.watch-history-card-delete');
    card.addEventListener('pointerdown', () => watchHistoryStartLongPress(card), { passive: true });
    card.addEventListener('pointerup', watchHistoryCancelLongPress, { passive: true });
    card.addEventListener('pointercancel', watchHistoryCancelLongPress, { passive: true });
    card.addEventListener('pointerleave', watchHistoryCancelLongPress, { passive: true });
    card.addEventListener('contextmenu', event => {
      event.preventDefault();
      watchHistoryCancelLongPress();
      watchHistoryShowDeleteAction(card);
    });
    if (deleteButton) {
      deleteButton.addEventListener('pointerdown', event => event.stopPropagation(), { passive: true });
      deleteButton.addEventListener('click', event => {
        event.preventDefault();
        event.stopPropagation();
        const index = Number(card.dataset.historyIndex);
        watchHistoryDeleteItem(watchHistoryState.items[index]);
      });
    }
    card.addEventListener('click', event => {
      if (watchHistoryLongPressTriggered) {
        watchHistoryLongPressTriggered = false;
        event.preventDefault();
        return;
      }
      if (card.classList.contains('is-long-pressed')) {
        card.classList.remove('is-long-pressed');
        if (deleteButton) deleteButton.hidden = true;
        event.preventDefault();
        return;
      }
      open();
    });
    card.addEventListener('keydown', event => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        open();
      }
    });
  });
}

function watchHistoryBindCardImages(root) {
  if (!root) return;
  root.querySelectorAll('.watch-history-poster img').forEach(image => {
    const markLoaded = () => image.closest('.watch-history-poster')?.classList.add('has-image');
    image.addEventListener('load', markLoaded, { once: true });
    image.addEventListener('error', () => {
      let fallbacks = [];
      try { fallbacks = JSON.parse(image.getAttribute('data-watch-history-card-fallbacks') || '[]'); } catch (_) { fallbacks = []; }
      const next = String(fallbacks.shift() || '');
      if (next) {
        image.setAttribute('data-watch-history-card-fallbacks', JSON.stringify(fallbacks));
        image.src = next;
        return;
      }
      image.remove();
    });
    if (image.complete && image.naturalWidth > 0) markLoaded();
  });
}

function watchHistoryNormalizeSites(value) {
  const sites = Array.isArray(value) ? value : (Array.isArray(value && value.sites) ? value.sites : []);
  const watchHistoryEnabled = value => value === true || value === 1 || (typeof value === 'string' && ['1', 'true', 'on', 'yes'].includes(value.trim().toLowerCase()));
  return sites.map(site => ({
    id: String(site && site.id !== undefined ? site.id : ''),
    name: String((site && site.name) || '').trim() || '未命名站点',
    // Older panel/API responses exposed SQLite booleans as 0/1. Accept both
    // representations so an enabled site is not incorrectly labelled off.
    enabled: watchHistoryEnabled(site && site.watch_history_enabled),
  })).filter(site => site.id);
}

function watchHistoryNormalizeItems(response) {
  if (Array.isArray(response && response.items)) return response.items;
  if (Array.isArray(response && response.sessions)) return response.sessions;
  if (Array.isArray(response && response.history)) return response.history;
  return [];
}

function watchHistoryItemMergeKey(item, index = 0) {
  const siteID = String((item && item.site_id) || '');
  const mediaType = String((item && item.media_type) || '').trim().toLowerCase();
  if (mediaType === 'episode') {
    const seriesName = String((item && item.series_name) || '').trim().toLowerCase();
    if (seriesName) return `series-name:${siteID}:${seriesName}`;
    const tmdbType = String((item && item.tmdb_type) || '').trim().toLowerCase();
    const tmdbID = Number(item && item.tmdb_id);
    if (tmdbType === 'tv' && Number.isSafeInteger(tmdbID) && tmdbID > 0) {
      return `series-tmdb:${siteID}:${tmdbID}`;
    }
  }
  const id = String((item && item.id) || '');
  return id ? `history:${id}` : `row:${index}`;
}

function watchHistoryMergeBackgroundPage(currentItems, incomingItems) {
  const current = Array.isArray(currentItems) ? currentItems : [];
  const incoming = Array.isArray(incomingItems) ? incomingItems : [];
  if (current.length <= WATCH_HISTORY_PAGE_SIZE) return incoming;
  const incomingKeys = new Set(incoming.map((item, index) => watchHistoryItemMergeKey(item, index)));
  return incoming.concat(current.filter((item, index) => !incomingKeys.has(watchHistoryItemMergeKey(item, index))));
}

function watchHistoryAuxiliaryError(error, fallback) {
  const message = String((error && error.message) || '').trim();
  return (message || fallback).slice(0, 200);
}

function watchHistoryTMDBAvailability(settings) {
  const enabled = settings && settings.enabled === true;
  const configured = settings && settings.configured === true;
  const invalid = !!(enabled && configured && String(settings.credential_state || '').trim().toLowerCase() === 'invalid');
  return {
    available: !!(enabled && configured && !invalid),
    invalid,
  };
}

function watchHistorySiteOptionsHTML() {
  const options = watchHistorySitesCache.map(site => `<option value="${esc(site.id)}" ${watchHistoryState.siteId === site.id ? 'selected' : ''}>${esc(site.name)}${site.enabled ? '' : '（未开启）'}</option>`);
  return `<option value="all" ${watchHistoryState.siteId === 'all' ? 'selected' : ''}>全部站点</option>${options.join('')}`;
}

function watchHistoryEmptyMessage() {
  const selected = watchHistorySitesCache.find(site => site.id === watchHistoryState.siteId);
  if (selected && !selected.enabled) return '该站点已关闭后续观看历史记录，当前没有已保存记录。';
  if (watchHistorySitesCache.length > 0 && !watchHistorySitesCache.some(site => site.enabled)) {
    return '尚未开启观看历史。请在站点编辑的高级设置中开启后再播放媒体。';
  }
  return '当前筛选条件下暂无观看记录。';
}

function watchHistoryRenderSiteOptions() {
  const select = document.getElementById('watch-history-site');
  if (!select) return;
  select.innerHTML = watchHistorySiteOptionsHTML();
  select.value = watchHistoryState.siteId;
}

function watchHistoryRenderNotice() {
  const notice = document.getElementById('watch-history-notice');
  if (!notice) return;
  const messages = [];
  if (watchHistoryState.sitesLoadError) {
    messages.push(`站点列表读取失败，筛选选项可能不完整：${watchHistoryState.sitesLoadError}`);
  } else {
    const selected = watchHistorySitesCache.find(site => site.id === watchHistoryState.siteId);
    if (selected && !selected.enabled) messages.push('该站点已关闭后续记录，已经保存的历史仍可查看。');
    else if (watchHistorySitesCache.length > 0 && !watchHistorySitesCache.some(site => site.enabled)) messages.push('请先在站点高级设置中开启观看历史记录。');
  }
  if (watchHistoryState.tmdbLoadError) {
    messages.push(`TMDB 设置读取失败，暂时无法确认资料补全状态：${watchHistoryState.tmdbLoadError}`);
  } else if (watchHistoryState.tmdbInvalid) {
    messages.push('TMDB Token 无效，请前往全局设置中的 TMDB 设置更换 Token 并重新测试。');
  } else if (watchHistoryState.tmdbConfigured === false) {
    messages.push('尚未启用 TMDB 资料补全；本地历史仍会正常记录，但不会自动补全海报和影片资料。');
  }
  notice.textContent = messages.join(' ');
  notice.hidden = messages.length === 0;
}

function watchHistoryItemsRenderSignature(items, context = '') {
  const source = Array.isArray(items) ? items : [];
  return JSON.stringify([context, source.map(item => [
    item && item.id, item && item.site_id, item && item.media_item_id, item && item.upstream_item_id,
    item && item.media_type, item && item.title, item && item.series_name,
    item && item.last_seen_at_ms, item && item.position_ticks, item && item.runtime_ticks,
    item && item.completed, item && item.poster_path, item && item.backdrop_path,
    item && item.match_status, item && item.user_name, item && item.client_name,
  ])]);
}

function watchHistoryRenderLive() {
  const section = document.getElementById('watch-history-live-section');
  const grid = document.getElementById('watch-history-live-grid');
  const count = document.getElementById('watch-history-live-count');
  const description = document.getElementById('watch-history-live-description');
  if (!section || !grid || !count || !description) return;
  const items = Array.isArray(watchHistoryState.liveItems) ? watchHistoryState.liveItems : [];
  count.textContent = String(items.length);
  if (watchHistoryState.liveLoadError) {
    section.hidden = false;
    description.textContent = `正在观看读取失败：${watchHistoryState.liveLoadError}`;
    const signature = `error:${watchHistoryState.liveLoadError}`;
    if (signature !== watchHistoryRenderedLiveSignature) grid.innerHTML = '';
    watchHistoryRenderedLiveSignature = signature;
    return;
  }
  section.hidden = items.length === 0;
  description.textContent = items.length === 1 ? '有 1 个播放会话正在同步。' : `有 ${items.length} 个播放会话正在同步。`;
  const signature = watchHistoryItemsRenderSignature(items, 'live');
  if (signature === watchHistoryRenderedLiveSignature) return;
  grid.innerHTML = items.map((item, index) => watchHistoryCardHTML(item, index, { source: 'live', live: true })).join('');
  watchHistoryRenderedLiveSignature = signature;
  watchHistoryBindCards(grid);
}

function watchHistoryRenderItems(errorMessage, options = {}) {
  const grid = document.getElementById('watch-history-grid');
  const status = document.getElementById('watch-history-status');
  const more = document.getElementById('watch-history-more');
  if (!grid || !status || !more) return;
  // A timer refresh must stay visually silent. In particular, replacing an
  // empty state with skeleton cards every five seconds is perceived as a page
  // flash even when the response has not changed.
  const showLoading = watchHistoryState.loading && options.background !== true;
  grid.setAttribute('aria-busy', String(showLoading));

  let signature = '';
  let html = '';
  let bindCards = false;
  if (errorMessage && watchHistoryState.items.length === 0) {
    signature = `error:${errorMessage}`;
    html = `<div class="watch-history-empty watch-history-error"><strong>观看历史读取失败</strong><span>${esc(errorMessage)}</span><button type="button" class="btn-ghost" id="watch-history-retry">重新加载</button></div>`;
  } else if (showLoading && watchHistoryState.items.length === 0) {
    signature = 'loading';
    html = Array.from({ length: 8 }, () => '<div class="watch-history-card watch-history-skeleton" aria-hidden="true"><div class="watch-history-poster"></div><div class="watch-history-card-body"><span></span><span></span><span></span></div></div>').join('');
  } else if (watchHistoryState.items.length === 0) {
    signature = `empty:${watchHistoryEmptyMessage()}`;
    html = `<div class="watch-history-empty"><strong>暂无观看历史</strong><span>${esc(watchHistoryEmptyMessage())}</span></div>`;
  } else {
    signature = watchHistoryItemsRenderSignature(watchHistoryState.items, `history:${watchHistoryState.tmdbConfigured}:${watchHistoryState.tmdbInvalid}`);
    html = watchHistoryState.items.map((item, index) => watchHistoryCardHTML(item, index)).join('');
    bindCards = true;
  }
  if (signature !== watchHistoryRenderedItemsSignature) {
    grid.innerHTML = html;
    watchHistoryRenderedItemsSignature = signature;
    if (errorMessage && watchHistoryState.items.length === 0) {
      const retry = document.getElementById('watch-history-retry');
      if (retry) retry.onclick = () => loadWatchHistory(true);
    } else if (bindCards) {
      watchHistoryBindCards(grid);
    }
  }

  more.hidden = !watchHistoryState.hasMore || watchHistoryState.items.length === 0;
  more.disabled = showLoading;
  more.textContent = showLoading && watchHistoryState.items.length > 0 ? '正在加载…' : '加载更多';
  status.textContent = errorMessage
    ? `读取失败：${errorMessage}`
    : (showLoading ? '正在读取观看历史…' : `当前显示 ${watchHistoryState.items.length} 条记录`);
  watchHistoryRenderNotice();
  watchHistoryRefreshOpenDetails();
}

function watchHistoryFilters(reset = false) {
  const filters = { limit: WATCH_HISTORY_PAGE_SIZE };
  if (watchHistoryState.siteId !== 'all') filters.site_id = watchHistoryState.siteId;
  if (watchHistoryState.mediaType !== 'all') filters.media_type = watchHistoryState.mediaType;
  const fromMS = watchHistoryRangeStart(watchHistoryState.range);
  if (fromMS > 0) filters.from_ms = Math.trunc(fromMS);
  if (!reset && watchHistoryState.nextCursor) filters.cursor = watchHistoryState.nextCursor;
  return filters;
}

function watchHistoryLiveFilters() {
  const filters = {};
  if (watchHistoryState.siteId !== 'all') filters.site_id = watchHistoryState.siteId;
  if (watchHistoryState.mediaType !== 'all') filters.media_type = watchHistoryState.mediaType;
  return filters;
}

function scheduleWatchHistoryRefresh() {
  if (watchHistoryRefreshTimer) clearTimeout(watchHistoryRefreshTimer);
  watchHistoryRefreshTimer = 0;
  if (typeof Router === 'undefined' || Router.current !== 'watch-history') return;
  watchHistoryRefreshTimer = setTimeout(() => {
    watchHistoryRefreshTimer = 0;
    if (typeof Router === 'undefined' || Router.current !== 'watch-history') return;
    if (watchHistoryState.loading) {
      scheduleWatchHistoryRefresh();
      return;
    }
    loadWatchHistory(true, { background: true });
  }, WATCH_HISTORY_REFRESH_INTERVAL_MS);
}

async function loadWatchHistory(reset, options = {}) {
  const page = document.getElementById('page-watch-history');
  if (!page) return;
  const background = options && options.background === true;
	const preserveLoadedPages = !!(background && reset && watchHistoryState.items.length > WATCH_HISTORY_PAGE_SIZE);
	const previousItems = preserveLoadedPages ? watchHistoryState.items.slice() : [];
	const previousCursor = preserveLoadedPages ? watchHistoryState.nextCursor : '';
	const previousHasMore = preserveLoadedPages ? watchHistoryState.hasMore : false;
  if (background && watchHistoryState.loading) {
    scheduleWatchHistoryRefresh();
    return;
  }
  const generation = ++watchHistoryLoadGeneration;
  if (reset && !background) {
    watchHistoryState.items = [];
    watchHistoryState.liveItems = [];
    watchHistoryState.nextCursor = '';
    watchHistoryState.hasMore = false;
  } else if (reset) {
    // Keep visible cards and pagination during a timer refresh. The request is
    // still forced to the first page; loaded later pages are merged back after
    // the response so an open detail card never disappears just because the
    // timer ticked.
  }
  watchHistoryState.loading = true;
  watchHistoryRenderItems(undefined, { background });

  try {
    const siteRequest = reset || watchHistorySitesCache.length === 0
      ? API.listSites()
        .then(value => ({ sites: watchHistoryNormalizeSites(value), error: '' }))
        .catch(error => ({ sites: watchHistorySitesCache, error: watchHistoryAuxiliaryError(error, '请求失败') }))
      : Promise.resolve({ sites: watchHistorySitesCache, error: watchHistoryState.sitesLoadError });
    const tmdbRequest = reset || watchHistoryState.tmdbConfigured === null
      ? API.getTMDBSettings()
        .then(settings => ({ ...watchHistoryTMDBAvailability(settings), error: '' }))
        .catch(error => ({
          available: watchHistoryState.tmdbConfigured,
          invalid: watchHistoryState.tmdbInvalid,
          error: watchHistoryAuxiliaryError(error, '请求失败'),
        }))
      : Promise.resolve({
        available: watchHistoryState.tmdbConfigured,
        invalid: watchHistoryState.tmdbInvalid,
        error: watchHistoryState.tmdbLoadError,
      });
    const liveRequest = API.getActiveWatchHistory(watchHistoryLiveFilters())
      .then(response => ({ response, error: '' }))
      .catch(error => ({ response: null, error: watchHistoryAuxiliaryError(error, '请求失败') }));
    const [siteResult, response, tmdbResult, liveResult] = await Promise.all([
      siteRequest,
      API.getWatchHistory(watchHistoryFilters(reset)),
      tmdbRequest,
      liveRequest,
    ]);
    if (generation !== watchHistoryLoadGeneration || Router.current !== 'watch-history' || !page.isConnected) return;
    // siteResult.sites is already normalized by the request promise. A second
    // pass would look for watch_history_enabled on the normalized {enabled}
    // shape and incorrectly turn every site off.
    watchHistorySitesCache = Array.isArray(siteResult.sites) ? siteResult.sites : [];
    watchHistoryState.sitesLoadError = siteResult.error;
    watchHistoryRenderSiteOptions();

    const incoming = watchHistoryNormalizeItems(response);
    if (reset) watchHistoryState.items = preserveLoadedPages
      ? watchHistoryMergeBackgroundPage(previousItems, incoming)
      : incoming;
    else {
      const existing = new Set(watchHistoryState.items.map(item => String(item && item.id || '')));
      watchHistoryState.items = watchHistoryState.items.concat(incoming.filter(item => {
        const id = String(item && item.id || '');
        return !id || !existing.has(id);
      }));
    }
    watchHistoryState.nextCursor = preserveLoadedPages
      ? previousCursor
      : String((response && response.next_cursor) || '');
    watchHistoryState.hasMore = preserveLoadedPages
      ? previousHasMore
      : !!(response && (response.has_more || response.next_cursor));
    if (typeof tmdbResult.available === 'boolean') watchHistoryState.tmdbConfigured = tmdbResult.available;
    watchHistoryState.tmdbInvalid = tmdbResult.invalid === true;
    watchHistoryState.tmdbLoadError = tmdbResult.error;
    watchHistoryState.liveLoadError = liveResult.error;
    if (liveResult.response) watchHistoryState.liveItems = watchHistoryNormalizeItems(liveResult.response);
    watchHistoryState.loading = false;
    watchHistoryRenderLive();
    watchHistoryRenderItems();
    scheduleWatchHistoryRefresh();
  } catch (error) {
    if (generation !== watchHistoryLoadGeneration || Router.current !== 'watch-history' || !page.isConnected) return;
    watchHistoryState.loading = false;
    watchHistoryRenderLive();
    watchHistoryRenderItems(error && error.message ? error.message : '请求失败');
    scheduleWatchHistoryRefresh();
  }
}

async function clearVisibleWatchHistory() {
  const selected = watchHistorySitesCache.find(site => site.id === watchHistoryState.siteId);
  const scope = watchHistoryState.siteId === 'all' ? '全部站点' : (selected ? selected.name : '当前站点');
  if (!confirm(`确认清空${scope}的观看历史？此操作无法撤销。`)) return;
  const button = document.getElementById('watch-history-clear');
  if (button) button.disabled = true;
  try {
    const result = await API.clearWatchHistory(watchHistoryState.siteId === 'all' ? '' : watchHistoryState.siteId);
    if (!result) return;
    Toast.success(`${scope}的观看历史已清空`);
    await loadWatchHistory(true);
  } catch (error) {
    Toast.error(error.message || '清空观看历史失败');
  } finally {
    if (button && button.isConnected) button.disabled = false;
  }
}

function stopWatchHistoryRefresh() {
  if (watchHistoryRefreshTimer) clearTimeout(watchHistoryRefreshTimer);
  watchHistoryRefreshTimer = 0;
  watchHistoryLoadGeneration += 1;
  watchHistoryState.loading = false;
  watchHistoryRenderedItemsSignature = '';
  watchHistoryRenderedLiveSignature = '';
  watchHistoryCancelLongPress();
  watchHistoryLongPressTriggered = false;
  watchHistoryCloseDetails();
  watchHistoryCloseImageViewer();
  if (watchHistoryKeydownHandler && typeof document !== 'undefined' && document.removeEventListener) {
    document.removeEventListener('keydown', watchHistoryKeydownHandler);
  }
  watchHistoryKeydownHandler = null;
}

function renderWatchHistory() {
  const page = document.getElementById('page-watch-history');
  if (!page) return;
  stopWatchHistoryRefresh();
  watchHistoryState.items = [];
  watchHistoryState.liveItems = [];
  watchHistoryState.nextCursor = '';
  watchHistoryState.hasMore = false;
  watchHistoryState.tmdbConfigured = null;
  watchHistoryState.tmdbInvalid = false;
  watchHistoryState.sitesLoadError = '';
  watchHistoryState.tmdbLoadError = '';
  watchHistoryState.liveLoadError = '';
  watchHistoryRenderedItemsSignature = '';
  watchHistoryRenderedLiveSignature = '';
  page.innerHTML = `<div class="watch-history-page">
    <header class="watch-history-hero fade-up">
      <div><p class="watch-history-eyebrow">PLAYBACK ARCHIVE</p><h1 class="section-title">观看历史</h1><p class="section-sub">把正在发生的播放与已看内容放在同一个清爽的观看空间。</p></div>
      <div class="watch-history-hero-signal" aria-label="自动刷新中"><span></span><b>自动刷新</b><small>${WATCH_HISTORY_REFRESH_INTERVAL_MS / 1000} 秒</small></div>
    </header>
    <section class="watch-history-toolbar fade-up" aria-label="观看历史筛选">
      <div class="watch-history-filters">
        <label><span>站点</span><select class="form-select" id="watch-history-site">${watchHistorySiteOptionsHTML()}</select></label>
        <label><span>媒体类型</span><select class="form-select" id="watch-history-type">
          <option value="all">全部类型</option><option value="movie">电影</option><option value="episode">剧集</option><option value="series">剧集合集</option>
        </select></label>
        <label><span>时间范围</span><select class="form-select" id="watch-history-range">
          <option value="all">全部时间</option><option value="today">今天</option><option value="7d">最近 7 天</option><option value="30d">最近 30 天</option>
        </select></label>
      </div>
      <div class="watch-history-toolbar-actions">
        <button type="button" class="request-log-action danger" id="watch-history-clear">清空记录</button>
        <button type="button" class="request-log-action" id="watch-history-refresh">刷新</button>
      </div>
      <p class="watch-history-status" id="watch-history-status" role="status" aria-live="polite">正在读取观看历史…</p>
    </section>
    <div class="watch-history-notice" id="watch-history-notice" role="status" aria-live="polite" aria-atomic="true" hidden></div>
    <section class="watch-history-live-section fade-up" id="watch-history-live-section" aria-labelledby="watch-history-live-title" hidden>
      <header class="watch-history-section-heading"><div><p class="watch-history-eyebrow">LIVE NOW</p><h2 id="watch-history-live-title">正在观看 <b id="watch-history-live-count">0</b></h2><p id="watch-history-live-description">正在读取播放会话…</p></div><span class="watch-history-live-mark" aria-hidden="true"><i></i>LIVE</span></header>
      <div class="watch-history-grid watch-history-live-grid" id="watch-history-live-grid" aria-label="正在观看列表"></div>
    </section>
    <section class="watch-history-library fade-up" aria-labelledby="watch-history-library-title"><header class="watch-history-section-heading"><div><p class="watch-history-eyebrow">YOUR LIBRARY</p><h2 id="watch-history-library-title">观看记录</h2></div></header><div class="watch-history-grid" id="watch-history-grid" aria-label="观看历史列表" aria-busy="true"></div></section>
    <div class="watch-history-more-wrap"><button type="button" class="btn-ghost" id="watch-history-more" hidden>加载更多</button></div>
    <div class="watch-history-detail-modal" id="watch-history-detail-modal" hidden>
      <div class="watch-history-detail-backdrop" data-watch-history-close></div>
      <div class="watch-history-detail-dialog" role="dialog" aria-modal="true" aria-labelledby="watch-history-detail-title" tabindex="-1">
        <button type="button" class="watch-history-detail-close" data-watch-history-close aria-label="关闭详情">×</button>
        <div id="watch-history-detail-content"></div>
      </div>
    </div>
    <div class="watch-history-image-modal" id="watch-history-image-modal" hidden>
      <div class="watch-history-image-backdrop" data-watch-history-image-close></div>
      <div class="watch-history-image-dialog" role="dialog" aria-modal="true" aria-labelledby="watch-history-image-viewer-title" tabindex="-1">
        <button type="button" class="watch-history-image-close" data-watch-history-image-close aria-label="关闭图片预览">×</button>
        <h2 id="watch-history-image-viewer-title" class="sr-only">图片预览</h2>
        <img id="watch-history-image-viewer-image" alt="" decoding="async">
      </div>
    </div>
    <footer class="watch-history-attribution">This product uses the TMDB API but is not endorsed or certified by TMDB. <a href="https://www.themoviedb.org/" target="_blank" rel="noopener noreferrer">了解 TMDB</a></footer>
  </div>`;

  const siteSelect = document.getElementById('watch-history-site');
  const typeSelect = document.getElementById('watch-history-type');
  const rangeSelect = document.getElementById('watch-history-range');
  siteSelect.value = watchHistoryState.siteId;
  typeSelect.value = watchHistoryState.mediaType;
  rangeSelect.value = watchHistoryState.range;
  siteSelect.onchange = () => {
    watchHistoryState.siteId = siteSelect.value;
    loadWatchHistory(true);
  };
  typeSelect.onchange = () => {
    watchHistoryState.mediaType = typeSelect.value;
    loadWatchHistory(true);
  };
  rangeSelect.onchange = () => {
    watchHistoryState.range = rangeSelect.value;
    loadWatchHistory(true);
  };
  document.getElementById('watch-history-refresh').onclick = () => loadWatchHistory(true);
  document.getElementById('watch-history-clear').onclick = clearVisibleWatchHistory;
  document.getElementById('watch-history-more').onclick = () => loadWatchHistory(false);
  document.querySelectorAll('[data-watch-history-close]').forEach(element => {
    element.addEventListener('click', watchHistoryCloseDetails);
  });
  document.querySelectorAll('[data-watch-history-image-close]').forEach(element => {
    element.addEventListener('click', watchHistoryCloseImageViewer);
  });
  watchHistoryKeydownHandler = event => {
    if (event.key === 'Escape' && !watchHistoryCloseImageViewer()) watchHistoryCloseDetails();
  };
  if (typeof document.addEventListener === 'function') document.addEventListener('keydown', watchHistoryKeydownHandler);
  loadWatchHistory(true);
}
