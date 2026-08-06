(function(root) {
  'use strict';

  const STORAGE_KEY = 'meridian-theme';
  const DARK = 'dark';
  const LIGHT = 'light';

  function normalizeTheme(value) {
    return value === LIGHT ? LIGHT : DARK;
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
    return normalizeTheme(element && element.getAttribute('data-theme'));
  }

  function syncToggle(theme) {
    const button = root.document && root.document.getElementById('theme-toggle');
    if (!button) return;
    const light = theme === LIGHT;
    const label = light ? '切换到黑色背景' : '切换到白色背景';
    button.setAttribute('aria-label', label);
    button.setAttribute('aria-pressed', String(light));
    button.title = label;
  }

  function applyTheme(value, persist) {
    const theme = normalizeTheme(value);
    const element = root.document && root.document.documentElement;
    if (element) {
      element.setAttribute('data-theme', theme);
      element.style.colorScheme = theme;
    }
    if (persist) {
      try {
        if (root.localStorage) root.localStorage.setItem(STORAGE_KEY, theme);
      } catch (_) {}
    }
    syncToggle(theme);
    if (root.dispatchEvent && typeof root.CustomEvent === 'function') {
      root.dispatchEvent(new root.CustomEvent('meridian-theme-change', { detail: { theme } }));
    }
    return theme;
  }

  function toggleTheme() {
    return applyTheme(currentTheme() === LIGHT ? DARK : LIGHT, true);
  }

  function bindToggle() {
    const button = root.document && root.document.getElementById('theme-toggle');
    if (!button || button.dataset.themeBound === 'true') {
      syncToggle(currentTheme());
      return;
    }
    button.dataset.themeBound = 'true';
    button.addEventListener('click', toggleTheme);
    syncToggle(currentTheme());
  }

  applyTheme(storedTheme(), false);

  if (root.document && root.document.readyState === 'loading') {
    root.document.addEventListener('DOMContentLoaded', bindToggle, { once: true });
  } else {
    bindToggle();
  }

  root.MeridianTheme = {
    apply: applyTheme,
    current: currentTheme,
    toggle: toggleTheme,
    storageKey: STORAGE_KEY,
  };
})(window);
