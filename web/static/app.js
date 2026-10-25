// feedreader — failed htmx requests, the entry drawer and its tabs, the
// theme switch, and broken feed icons.
// Plain script, no build step; loaded from 'self' as the
// Content-Security-Policy requires, and not deferred (see the end of the
// file).
//
// The app answers a failed action with a message that it retargets to
// #notice (HX-Retarget). Any other error response, such as an empty 502 from
// the reverse proxy while the service restarts or a plain-text 403, must not
// replace the card or the grid the request was aimed at, and must not look
// like a success either: it is reported in #notice instead.
(function () {
  "use strict";

  function fromApp(xhr) {
    return Boolean(xhr && xhr.getResponseHeader("HX-Retarget"));
  }

  function showError(text) {
    var area = document.getElementById("notice");
    if (!area) {
      return;
    }
    var p = document.createElement("p");
    p.className = "notice notice-error";
    p.textContent = text;
    area.replaceChildren(p);
  }

  document.addEventListener("htmx:beforeSwap", function (evt) {
    if (evt.detail.isError && !fromApp(evt.detail.xhr)) {
      evt.detail.shouldSwap = false;
    }
  });

  document.addEventListener("htmx:responseError", function (evt) {
    var xhr = evt.detail.xhr;
    if (!fromApp(xhr)) {
      showError("The server answered with an error (HTTP " + (xhr ? xhr.status : "?") +
        "). Reload the page and try again.");
    }
  });

  document.addEventListener("htmx:sendError", function () {
    showError("Could not reach the server. Reload the page and try again.");
  });

  // A card's Details button loads the entry into the layout's #entry-drawer
  // (hx-target), a modal dialog docked to the right edge; it opens once the
  // content is in. Esc, its Close buttons (method="dialog" forms) and a
  // click on the backdrop close it, and focus goes back to the button that
  // opened it. Errors are retargeted to #notice and leave the drawer
  // closed. Without JavaScript, or with a modifier key held, the button
  // opens a page instead.
  var opener = null;

  function isEntryDrawer(el) {
    return el instanceof HTMLDialogElement && el.id === "entry-drawer";
  }

  // The button is remembered only when the drawer opens: a swap into the
  // drawer while it is open keeps the element focus goes back to.
  document.addEventListener("htmx:afterSwap", function (evt) {
    var drawer = evt.detail.target;
    if (!isEntryDrawer(drawer) || drawer.open) {
      return;
    }
    opener = evt.detail.requestConfig ? evt.detail.requestConfig.elt : null;
    drawer.showModal();
  });

  // htmx cancels every click on the Details button to make its request, a
  // Ctrl/Cmd-, Shift- or Alt-click too. Those belong to the browser (open
  // the entry page in a new tab or window, …): they are stopped on the
  // document in the capture phase, before they reach htmx's listener on
  // the button, and their default action, following the href, is left
  // alone.
  document.addEventListener("click", function (evt) {
    var link = evt.target instanceof Element ? evt.target.closest("a.card-details") : null;
    if (link && (evt.ctrlKey || evt.metaKey || evt.shiftKey || evt.altKey)) {
      evt.stopPropagation();
    }
  }, true);

  // The drawer's content fills it, so a click whose target is the dialog
  // element itself landed on its backdrop.
  document.addEventListener("click", function (evt) {
    if (isEntryDrawer(evt.target)) {
      evt.target.close();
    }
  });

  // "close" does not bubble: listen in the capture phase.
  document.addEventListener("close", function (evt) {
    if (!isEntryDrawer(evt.target)) {
      return;
    }
    if (opener && opener.isConnected) {
      opener.focus();
    }
    opener = null;
  }, true);

  // A video's transcript status line replaces itself (hx-swap="outerHTML")
  // when its button is pressed, taking the focused button with it: focus
  // goes to the new line's button, or to the line itself, instead of
  // falling back to the start of the page.
  document.addEventListener("htmx:afterSettle", function (evt) {
    var line = evt.target;
    var lost = !document.activeElement || document.activeElement === document.body;
    if (lost && line instanceof Element && line.classList.contains("transcript-status") && line.isConnected) {
      (line.querySelector("button") || line).focus();
    }
  });

  // Tabs (role="tab" in a role="tablist"): the entry's Summary and
  // Transcription. Each tab is a link to the page with that tab selected,
  // which is what it does without JavaScript; here a click selects it in
  // place, showing its panel (aria-controls) and hiding the others, and the
  // panel gets a "tab-shown" event, on which htmx loads the transcript the
  // first time. The arrow keys, Home and End move between the enabled tabs
  // (roving tabindex). A disabled tab (aria-disabled) does nothing.
  function enabledTabs(list) {
    return Array.prototype.filter.call(list.querySelectorAll('[role="tab"]'), function (t) {
      return t.getAttribute("aria-disabled") !== "true";
    });
  }

  function selectTab(tab) {
    var list = tab.closest('[role="tablist"]');
    if (!list) {
      return;
    }
    enabledTabs(list).forEach(function (t) {
      var on = t === tab;
      t.setAttribute("aria-selected", on ? "true" : "false");
      t.tabIndex = on ? 0 : -1;
      var panel = document.getElementById(t.getAttribute("aria-controls"));
      if (panel) {
        panel.hidden = !on;
        if (on) {
          panel.dispatchEvent(new CustomEvent("tab-shown"));
        }
      }
    });
  }

  document.addEventListener("click", function (evt) {
    var tab = evt.target instanceof Element ? evt.target.closest('[role="tab"]') : null;
    if (!tab || evt.ctrlKey || evt.metaKey || evt.shiftKey || evt.altKey) {
      return;
    }
    evt.preventDefault();
    if (tab.getAttribute("aria-disabled") !== "true") {
      selectTab(tab);
    }
  });

  document.addEventListener("keydown", function (evt) {
    var tab = evt.target instanceof Element ? evt.target.closest('[role="tab"]') : null;
    var list = tab ? tab.closest('[role="tablist"]') : null;
    if (!list) {
      return;
    }
    var tabs = enabledTabs(list);
    var i = tabs.indexOf(tab);
    var next = null;
    switch (evt.key) {
    case "ArrowRight":
      next = tabs[(i + 1) % tabs.length];
      break;
    case "ArrowLeft":
      next = tabs[(i - 1 + tabs.length) % tabs.length];
      break;
    case "Home":
      next = tabs[0];
      break;
    case "End":
      next = tabs[tabs.length - 1];
      break;
    case " ":
    case "Enter":
      next = tab;
      break;
    default:
      return;
    }
    evt.preventDefault();
    if (next) {
      selectTab(next);
      next.focus();
    }
  });

  // The theme switch in each top bar: static/theme.js has already set
  // data-theme on <html> before the first paint. A click flips it and keeps
  // the choice in localStorage. The switch is rendered hidden (it does
  // nothing without JavaScript) and its state is filled in here, also for
  // top bars that htmx swaps in later.
  var THEME_KEY = "feedreader-theme";

  function syncThemeSwitches() {
    var dark = document.documentElement.getAttribute("data-theme") !== "light";
    document.querySelectorAll(".theme-switch").forEach(function (btn) {
      btn.setAttribute("aria-checked", dark ? "true" : "false");
      btn.title = dark ? "Switch to light theme" : "Switch to dark theme";
      btn.hidden = false;
    });
  }

  document.addEventListener("click", function (evt) {
    var btn = evt.target instanceof Element ? evt.target.closest(".theme-switch") : null;
    if (!btn) {
      return;
    }
    var next = btn.getAttribute("aria-checked") === "true" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    try {
      window.localStorage.setItem(THEME_KEY, next);
    } catch (e) {
      // Private mode or storage blocked: the choice lasts until the page
      // is left.
    }
    syncThemeSwitches();
  });

  document.addEventListener("DOMContentLoaded", syncThemeSwitches);
  document.addEventListener("htmx:load", syncThemeSwitches);
  document.addEventListener("theme-changed", syncThemeSwitches);

  // Feed icons and channel avatars are hot-linked and can stop loading (a
  // site drops its favicon, a channel changes its avatar). The feed's
  // initials follow every icon, hidden by the stylesheet while the icon is
  // there, so removing a broken icon shows them. Load errors do not bubble:
  // the listener captures them on the document, and it is in place before
  // any icon can fail because this script is not deferred.
  document.addEventListener("error", function (evt) {
    var img = evt.target;
    if (img instanceof HTMLImageElement && img.matches(".avatar > img, .thumb-icon")) {
      img.remove();
    }
  }, true);
})();
