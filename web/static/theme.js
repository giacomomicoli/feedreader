// feedreader — light or dark theme, decided before the first paint.
// Loaded in <head>, before app.css and not deferred, so the page never
// flashes in the wrong theme. The choice made with the theme switch (see
// static/app.js) is kept in localStorage; without one the page follows the
// system setting, live. Without JavaScript, app.css follows the system
// setting on its own.
(function () {
  "use strict";

  var KEY = "feedreader-theme";
  var root = document.documentElement;
  var system = window.matchMedia ? window.matchMedia("(prefers-color-scheme: light)") : null;

  function stored() {
    try {
      var v = window.localStorage.getItem(KEY);
      return v === "light" || v === "dark" ? v : null;
    } catch (e) {
      return null;
    }
  }

  function apply() {
    root.setAttribute("data-theme", stored() || (system && system.matches ? "light" : "dark"));
  }

  apply();
  if (system && system.addEventListener) {
    system.addEventListener("change", function () {
      if (!stored()) {
        apply();
        document.dispatchEvent(new CustomEvent("theme-changed"));
      }
    });
  }
})();
