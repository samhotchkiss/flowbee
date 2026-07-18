// board.js — the F12 board interactions: the SSE live hook (auto-refresh the
// panes off /v1/events) and the detail drawer that does NOT dim the board and
// supports click card -> card (build-list §G).
(function () {
  "use strict";

  // ── SSE live hook ── any lifecycle event reloads the live data (debounced).
  function wireSSE() {
    var live = document.querySelector("nav.fb-nav .live");
    try {
      var es = new EventSource("/v1/events");
      var t = null;
      var scheduleRefresh = function () {
        clearTimeout(t);
        t = setTimeout(refresh, 350);
      };
      es.onopen = function () {
        document.documentElement.classList.add("sse-live");
        if (live) live.classList.add("on");
      };
      es.onerror = function () {
        document.documentElement.classList.remove("sse-live");
        if (live) live.classList.remove("on");
      };
      // The server emits named `lifecycle` and `epics` events. `onmessage` only
      // receives unnamed/default events, so listen to all three forms.
      es.onmessage = scheduleRefresh;
      es.addEventListener("lifecycle", scheduleRefresh);
      es.addEventListener("epics", scheduleRefresh);
      window.addEventListener("beforeunload", function () { es.close(); });
    } catch (e) { /* SSE unsupported: the page is still a valid static render */ }
  }

  // refresh re-fetches the current view's pane fragment and swaps the board/fleet
  // body in place (so the open drawer is preserved — the board is never dimmed).
  function refresh() {
    var root = document.getElementById("fb-live");
    if (!root) { return; }
    // preserve the current query (e.g. the board's ?repo=<id> filter) so the live
    // refresh keeps the same view; just append partial=1 to get the body fragment.
    var params = new URLSearchParams(window.location.search);
    params.set("partial", "1");
    fetch(window.location.pathname + "?" + params.toString(), { headers: { "Accept": "text/html" } })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html === null) { return; }
        root.innerHTML = html;
        wireCards();
        wireTheme();
      })
      .catch(function () { /* transient: the next event retries */ });
  }

  // ── detail drawer ── click a card to open the drawer; click another card to
  // swap its contents in place (no board dim, no navigation).
  function openDrawer(jobID) {
    var drawer = document.getElementById("fb-drawer");
    if (!drawer) { return; }
    drawer.classList.add("open");
    var body = drawer.querySelector(".drawer-body");
    body.innerHTML = '<p class="muted">Loading ' + jobID + '…</p>';
    fetch("/board/detail?job=" + encodeURIComponent(jobID), { headers: { "Accept": "text/html" } })
      .then(function (r) { return r.ok ? r.text() : "<p class='over'>not found</p>"; })
      .then(function (html) { body.innerHTML = html; wireDrawerLinks(); })
      .catch(function () { body.innerHTML = "<p class='over'>load error</p>"; });
  }

  function closeDrawer() {
    var drawer = document.getElementById("fb-drawer");
    if (drawer) { drawer.classList.remove("open"); }
  }

  function wireCards() {
    var cards = document.querySelectorAll(".card[data-job]");
    for (var i = 0; i < cards.length; i++) {
      cards[i].addEventListener("click", function () { openDrawer(this.getAttribute("data-job")); });
    }
  }

  // wireDrawerLinks lets the drawer click card -> card (the "card-to-card" jump):
  // a related-job link inside the drawer reloads the drawer in place.
  function wireDrawerLinks() {
    var links = document.querySelectorAll("#fb-drawer [data-job-link]");
    for (var i = 0; i < links.length; i++) {
      links[i].addEventListener("click", function (e) {
        e.preventDefault();
        openDrawer(this.getAttribute("data-job-link"));
      });
    }
  }

  // ── theme ── the epic fleet's target is dark, but the footer control is
  // a real persisted toggle rather than decorative chrome.
  function setTheme(theme) {
    if (theme === "light") {
      document.documentElement.setAttribute("data-theme", "light");
    } else {
      document.documentElement.removeAttribute("data-theme");
      theme = "dark";
    }
    var button = document.getElementById("theme-toggle");
    if (button) {
      button.setAttribute("aria-pressed", theme === "light" ? "true" : "false");
      button.setAttribute("title", "Switch to " + (theme === "light" ? "dark" : "light") + " theme");
    }
  }

  function wireTheme() {
    var button = document.getElementById("theme-toggle");
    if (!button || button.getAttribute("data-wired") === "1") { return; }
    button.setAttribute("data-wired", "1");
    button.addEventListener("click", function () {
      var next = document.documentElement.getAttribute("data-theme") === "light" ? "dark" : "light";
      setTheme(next);
      try { window.localStorage.setItem("flowbee-theme", next); } catch (e) { /* private mode */ }
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    var stored = "dark";
    try { stored = window.localStorage.getItem("flowbee-theme") || "dark"; } catch (e) { /* private mode */ }
    setTheme(stored);
    wireSSE();
    wireCards();
    wireTheme();
    var c = document.getElementById("fb-drawer-close");
    if (c) { c.addEventListener("click", closeDrawer); }
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") { closeDrawer(); } });
  });
})();
