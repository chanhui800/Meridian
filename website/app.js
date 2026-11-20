'use strict';

document.addEventListener('DOMContentLoaded', function () {
  var copyBtn = document.getElementById('copyBtn');
  var installCode = document.getElementById('installCode');
  var copyFeedback = document.getElementById('copyFeedback');
  var copyResetTimer;
  if (copyBtn && installCode) {
    copyBtn.addEventListener('click', async function () {
      if (copyBtn.disabled) return;
      clearTimeout(copyResetTimer);
      copyBtn.disabled = true;
      copyBtn.textContent = '正在复制…';
      if (copyFeedback) copyFeedback.hidden = true;
      try {
        if (!navigator.clipboard || !navigator.clipboard.writeText) throw new Error('Clipboard unavailable');
        await navigator.clipboard.writeText(installCode.textContent);
        copyBtn.textContent = '已复制';
        copyResetTimer = setTimeout(function () { copyBtn.textContent = '复制'; }, 1600);
      } catch (_) {
        copyBtn.textContent = '手动复制';
        if (copyFeedback) {
          copyFeedback.textContent = '无法自动复制，请选中下方安装配置并手动复制。';
          copyFeedback.hidden = false;
        }
        try {
          installCode.focus({ preventScroll: true });
          var range = document.createRange();
          range.selectNodeContents(installCode);
          var selection = window.getSelection();
          if (selection) {
            selection.removeAllRanges();
            selection.addRange(range);
          }
        } catch (_) {
          // Keep the manual-copy instructions visible if selection is blocked.
        }
      } finally {
        copyBtn.disabled = false;
      }
    });
  }

  var lightbox = document.getElementById('lightbox');
  var lightboxImg = document.getElementById('lightboxImg');
  var closeBtn = document.getElementById('lightboxClose');

  document.querySelectorAll('.shot').forEach(function (shot) {
    shot.addEventListener('click', function () {
      lightboxImg.src = shot.getAttribute('data-full');
      lightboxImg.alt = shot.querySelector('img').alt;
      lightbox.hidden = false;
      document.body.style.overflow = 'hidden';
    });
  });

  function closeLightbox() {
    lightbox.hidden = true;
    document.body.style.overflow = '';
  }
  if (closeBtn) {
    closeBtn.addEventListener('click', closeLightbox);
    lightbox.addEventListener('click', function (e) {
      if (e.target === lightbox) closeLightbox();
    });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape') closeLightbox();
    });
  }
});
