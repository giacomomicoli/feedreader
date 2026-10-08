// feedreader — failed htmx requests. Plain script, no build step; loaded
// from 'self' as the Content-Security-Policy requires.
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
})();
