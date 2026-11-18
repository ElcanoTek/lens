// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
(function () {
  'use strict';
  function normalize(value) {
    const base = (value || '').trim().toLowerCase().split('-')[0];
    return ['en', 'es', 'pt'].includes(base) ? base : '';
  }
  const root = document.documentElement;
  const language = normalize(root.lang) || 'en';
  const locale = { en: 'en-US', es: 'es-ES', pt: 'pt-PT' }[language];
  const url = new URL(window.location.href);
  const explicit = normalize(url.searchParams.get('lang'));
  let saved = '';
  try { saved = normalize(localStorage.getItem('elcano.language')); } catch (_) { /* Storage is optional. */ }
  if ((explicit || saved) && (explicit || saved) !== language) {
    url.searchParams.set('lang', explicit || saved);
    window.location.replace(url.href);
    return;
  }
  if (explicit) {
    try { localStorage.setItem('elcano.language', explicit); } catch (_) { /* The server cookie also remembers it. */ }
  }
  const catalog = JSON.parse(document.querySelector('meta[name="elcano-translations"]')?.content || '{}');
  function number(value, options) { return new Intl.NumberFormat(locale, options).format(value); }
  function text(source, ...values) {
    const message = Object.prototype.hasOwnProperty.call(catalog, source) ? catalog[source] : source;
    return message.replace(/\{(\d+)\}/g, (slot, index) => index < values.length ? (typeof values[index] === 'number' ? number(values[index]) : String(values[index])) : slot);
  }
  function html(source, ...values) {
    return text(source, ...values).replace(/[&<>"']/g, c => ({'&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;'}[c]));
  }
  window.LensI18n = { text, html, number, language, locale };
  document.addEventListener('DOMContentLoaded', () => {
    // Supply translated accessible labels without changing vendored Flag code.
    const toggles = document.querySelectorAll('[data-flag-theme-toggle]');
    function themeLabels() {
      toggles.forEach(toggle => toggle.setAttribute('aria-label', text(root.dataset.theme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode')));
    }
    if (toggles.length) {
      new MutationObserver(themeLabels).observe(root, { attributes: true, attributeFilter: ['data-theme'] });
      themeLabels();
    }
    document.querySelector('[data-language-choice]')?.addEventListener('submit', event => {
      event.preventDefault();
      const selected = event.currentTarget.elements.lang.value;
      try { localStorage.setItem('elcano.language', selected); } catch (_) { /* The query still works. */ }
      const target = new URL(window.location.href);
      target.searchParams.set('lang', selected);
      // Flash notices describe the action just completed; changing language
      // starts a fresh view and does not replay that action or stale notice.
      ['error', 'message', 'files_error', 'files_message'].forEach(key => target.searchParams.delete(key));
      window.location.assign(target.href);
    });
  });
}());
