(() => {
  const root = document.documentElement;
  const themeButton = document.querySelector('.theme-toggle');
  const themeMeta = document.querySelector('meta[name="theme-color"]');
  const navButton = document.querySelector('.nav-toggle');
  const nav = document.getElementById('site-nav');
  const themePreference = window.matchMedia('(prefers-color-scheme: light)');

  function normalizeThemeMode(value) {
    return value === 'light' || value === 'dark' ? value : 'system';
  }

  function resolveTheme(mode) {
    return mode === 'system' ? (themePreference.matches ? 'light' : 'dark') : mode;
  }

  function nextThemeMode(mode) {
    const deviceTheme = resolveTheme('system');
    if (mode === 'system') return deviceTheme === 'light' ? 'dark' : 'light';
    if (mode !== deviceTheme) return deviceTheme;
    return 'system';
  }

  function setTheme(mode, persist = true) {
    mode = normalizeThemeMode(mode);
    const theme = resolveTheme(mode);
    root.dataset.themeMode = mode;
    root.dataset.theme = theme;
    root.style.colorScheme = theme;
    const themeName = theme === 'dark' ? '深色' : '浅色';
    const nextMode = nextThemeMode(mode);
    const currentName = mode === 'system' ? `跟随设备（当前${themeName}）` : themeName;
    const nextName = nextMode === 'system' ? '跟随设备' : (nextMode === 'light' ? '浅色主题' : '深色主题');
    const label = `主题：${currentName}；点击切换到${nextName}`;
    themeButton?.setAttribute('aria-label', label);
    themeButton?.setAttribute('aria-pressed', mode === 'system' ? 'mixed' : String(mode === 'light'));
    if (themeButton) themeButton.title = label;
    if (themeMeta) themeMeta.content = theme === 'dark' ? '#07101d' : '#f5f7fb';
    if (persist) {
      try { localStorage.setItem('meridian-site-theme', mode); } catch (_) {}
    }
  }

  setTheme(root.dataset.themeMode || 'system', false);
  themeButton?.addEventListener('click', () => {
    const mode = normalizeThemeMode(root.dataset.themeMode);
    setTheme(nextThemeMode(mode));
  });
  const handleThemePreferenceChange = () => {
    if (root.dataset.themeMode === 'system') setTheme('system', false);
  };
  if (typeof themePreference.addEventListener === 'function') {
    themePreference.addEventListener('change', handleThemePreferenceChange);
  } else if (typeof themePreference.addListener === 'function') {
    themePreference.addListener(handleThemePreferenceChange);
  }

  function closeNav() {
    nav?.classList.remove('is-open');
    navButton?.setAttribute('aria-expanded', 'false');
    navButton?.setAttribute('aria-label', '打开导航菜单');
  }

  navButton?.addEventListener('click', () => {
    const open = !nav?.classList.contains('is-open');
    nav?.classList.toggle('is-open', open);
    navButton.setAttribute('aria-expanded', String(open));
    navButton.setAttribute('aria-label', open ? '关闭导航菜单' : '打开导航菜单');
  });

  nav?.querySelectorAll('a').forEach(link => link.addEventListener('click', closeNav));
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape') closeNav();
  });

  const tabs = [...document.querySelectorAll('.gallery-tabs [role="tab"]')];
  const galleryImage = document.getElementById('gallery-image');
  const galleryTitle = document.getElementById('gallery-title');
  const galleryCaption = document.getElementById('gallery-caption');

  function selectGalleryTab(tab) {
    if (!tab || !galleryImage) return;
    tabs.forEach(item => {
      const selected = item === tab;
      item.setAttribute('aria-selected', String(selected));
      item.tabIndex = selected ? 0 : -1;
    });
    document.getElementById('gallery-panel')?.setAttribute('aria-labelledby', tab.id);
    galleryImage.classList.add('is-switching');
    const nextImage = new Image();
    nextImage.onload = () => {
      galleryImage.src = tab.dataset.src;
      galleryImage.alt = tab.dataset.alt;
      galleryTitle.textContent = tab.dataset.title;
      galleryCaption.textContent = tab.dataset.caption;
      requestAnimationFrame(() => galleryImage.classList.remove('is-switching'));
    };
    nextImage.onerror = () => galleryImage.classList.remove('is-switching');
    nextImage.src = tab.dataset.src;
  }

  tabs.forEach((tab, index) => {
    tab.addEventListener('click', () => selectGalleryTab(tab));
    tab.addEventListener('keydown', event => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      let targetIndex = index;
      if (event.key === 'ArrowLeft') targetIndex = (index - 1 + tabs.length) % tabs.length;
      if (event.key === 'ArrowRight') targetIndex = (index + 1) % tabs.length;
      if (event.key === 'Home') targetIndex = 0;
      if (event.key === 'End') targetIndex = tabs.length - 1;
      tabs[targetIndex].focus();
      selectGalleryTab(tabs[targetIndex]);
    });
  });

  const deployTabs = [...document.querySelectorAll('.deploy-tabs [role="tab"]')];
  const terminalLanguage = document.getElementById('terminal-language');

  function selectDeployTab(tab) {
    deployTabs.forEach(item => {
      const selected = item === tab;
      item.setAttribute('aria-selected', String(selected));
      item.tabIndex = selected ? 0 : -1;
      const panel = document.getElementById(item.getAttribute('aria-controls'));
      if (panel) panel.hidden = !selected;
    });
    if (terminalLanguage) terminalLanguage.textContent = tab.id === 'deploy-tab-docker' ? 'yaml + shell' : 'bash';
  }

  deployTabs.forEach((tab, index) => {
    tab.addEventListener('click', () => selectDeployTab(tab));
    tab.addEventListener('keydown', event => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
      event.preventDefault();
      let targetIndex = index;
      if (event.key === 'ArrowLeft') targetIndex = (index - 1 + deployTabs.length) % deployTabs.length;
      if (event.key === 'ArrowRight') targetIndex = (index + 1) % deployTabs.length;
      if (event.key === 'Home') targetIndex = 0;
      if (event.key === 'End') targetIndex = deployTabs.length - 1;
      deployTabs[targetIndex].focus();
      selectDeployTab(deployTabs[targetIndex]);
    });
  });

  document.querySelectorAll('[data-copy]').forEach(button => {
    button.addEventListener('click', async () => {
      const target = document.querySelector(button.dataset.copy);
      if (!target) return;
      const label = button.querySelector('span');
      try {
        await navigator.clipboard.writeText(target.textContent.trim());
        if (label) label.textContent = '已复制';
      } catch (_) {
        const range = document.createRange();
        range.selectNodeContents(target);
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        if (label) label.textContent = '已选中';
      }
      window.setTimeout(() => { if (label) label.textContent = '复制'; }, 1800);
    });
  });

  const year = document.getElementById('year');
  if (year) year.textContent = String(new Date().getFullYear());
})();
