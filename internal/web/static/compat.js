// sos-vpdive: a browser older than the supported ones (spec §9.6: Chrome
// 111, Safari 16.4, Firefox 128) gets a short notice instead of a broken
// page. Together, color-mix() and CSS.registerProperty draw that line. Plain
// ES5 on purpose: this must run where app.js cannot.
(function () {
  var css = window.CSS;
  if (css && css.supports && css.supports("color", "color-mix(in lab, red, red)") && css.registerProperty) return;
  var notice = document.querySelector("[data-old-browser]");
  var main = document.querySelector("main");
  if (notice) notice.removeAttribute("hidden");
  if (main) main.setAttribute("hidden", "");
})();
