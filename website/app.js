'use strict';

document.addEventListener('DOMContentLoaded', function () {
  var copyBtn = document.getElementById('copyBtn');
  var installCode = document.getElementById('installCode');
  if (copyBtn && installCode) {
    copyBtn.addEventListener('click', function () {
      var text = installCode.textContent;
      function done() {
        copyBtn.textContent = '已复制';
        setTimeout(function () { copyBtn.textContent = '复制'; }, 1600);
      }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, done);
      } else {
        done();
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
