(function(root) {
  'use strict';

  const STORAGE_KEY = 'meridian-theme';
  const SYSTEM = 'system';
  const DARK = 'dark';
  const LIGHT = 'light';
  const systemPreference = typeof root.matchMedia === 'function'
    ? root.matchMedia('(prefers-color-scheme: light)')
    : null;

  function normalizeMode(value) {
    return value === LIGHT || value === DARK ? value : SYSTEM;
  }

  function resolvedTheme(mode) {
    if (mode === LIGHT || mode === DARK) return mode;
    return systemPreference && systemPreference.matches ? LIGHT : DARK;
  }

  function storedTheme() {
    try {
      return root.localStorage ? root.localStorage.getItem(STORAGE_KEY) : null;
    } catch (_) {
      return null;
    }
  }

  function currentTheme() {
    const element = root.document && root.document.documentElement;
    return element && element.getAttribute('data-theme') === LIGHT ? LIGHT : DARK;
  }

  function currentMode() {
    const element = root.document && root.document.documentElement;
    return normalizeMode(element && element.getAttribute('data-theme-mode'));
  }

  function nextMode(mode) {
    const deviceTheme = resolvedTheme(SYSTEM);
    if (mode === SYSTEM) return deviceTheme === LIGHT ? DARK : LIGHT;
    if (mode !== deviceTheme) return deviceTheme;
    return SYSTEM;
  }

  function syncToggle(mode, theme) {
    const button = root.document && root.document.getElementById('theme-toggle');
    if (!button) return;
    const themeName = theme === LIGHT ? '浅色' : '深色';
    const next = nextMode(mode);
    const currentName = mode === SYSTEM ? `跟随设备（当前${themeName}）` : themeName;
    const nextName = next === SYSTEM ? '跟随设备' : (next === LIGHT ? '浅色主题' : '深色主题');
    const label = `主题：${currentName}；点击切换到${nextName}`;
    button.setAttribute('aria-label', label);
    button.setAttribute('aria-pressed', mode === SYSTEM ? 'mixed' : String(mode === LIGHT));
    button.title = label;
  }

  function applyThemeMode(value, persist) {
    const mode = normalizeMode(value);
    const theme = resolvedTheme(mode);
    const element = root.document && root.document.documentElement;
    if (element) {
      element.setAttribute('data-theme', theme);
      element.setAttribute('data-theme-mode', mode);
      element.style.colorScheme = theme;
    }
    if (persist) {
      try {
        if (root.localStorage) root.localStorage.setItem(STORAGE_KEY, mode);
      } catch (_) {}
    }
    syncToggle(mode, theme);
    if (root.dispatchEvent && typeof root.CustomEvent === 'function') {
      root.dispatchEvent(new root.CustomEvent('meridian-theme-change', { detail: { theme, mode } }));
    }
    return theme;
  }

  function toggleTheme() {
    const mode = currentMode();
    return applyThemeMode(nextMode(mode), true);
  }

  function handleSystemPreferenceChange() {
    if (currentMode() === SYSTEM) applyThemeMode(SYSTEM, false);
  }

  function bindToggle() {
    const button = root.document && root.document.getElementById('theme-toggle');
    if (!button || button.dataset.themeBound === 'true') {
      syncToggle(currentMode(), currentTheme());
      return;
    }
    button.dataset.themeBound = 'true';
    button.addEventListener('click', toggleTheme);
    syncToggle(currentMode(), currentTheme());
  }

  applyThemeMode(storedTheme(), false);

  if (systemPreference) {
    if (typeof systemPreference.addEventListener === 'function') {
      systemPreference.addEventListener('change', handleSystemPreferenceChange);
    } else if (typeof systemPreference.addListener === 'function') {
      systemPreference.addListener(handleSystemPreferenceChange);
    }
  }

  if (root.document && root.document.readyState === 'loading') {
    root.document.addEventListener('DOMContentLoaded', bindToggle, { once: true });
  } else {
    bindToggle();
  }

  root.MeridianTheme = {
    apply: applyThemeMode,
    current: currentTheme,
    mode: currentMode,
    toggle: toggleTheme,
    storageKey: STORAGE_KEY,
  };
})(window);
