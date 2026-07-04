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
      es.onopen = function () { if (live) live.classList.add("on"); };
      es.onerror = function () { if (live) live.classList.remove("on"); };
      es.onmessage = function () {
        clearTimeout(t);
        t = setTimeout(refresh, 350);
      };
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
      })
      .catch(function () { /* transient: the next event retries */ });
  }

  // ── detail drawer ── click a card to open the drawer; click another card to
  // swap its contents in place (no board dim, no navigation).
  function openDrawer(jobID) {
    openDrawerFrom(jobID, "/board/detail");
  }

  function openTraceDrawer(jobID) {
    openDrawerFrom(jobID, "/board/trace");
  }

  function openDrawerFrom(jobID, path) {
    var drawer = document.getElementById("fb-drawer");
    if (!drawer) { return; }
    drawer.classList.add("open");
    var body = drawer.querySelector(".drawer-body");
    body.innerHTML = '<p class="muted">Loading ' + jobID + '…</p>';
    fetch(path + "?job=" + encodeURIComponent(jobID), { headers: { "Accept": "text/html" } })
      .then(function (r) {
        if (r.ok) { return r.text(); }
        return r.status === 403 ? "<p class='over'>forbidden</p>" : "<p class='over'>not found</p>";
      })
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
    wireCardMenus();
  }

  function closeMenus(except) {
    var menus = document.querySelectorAll("[data-card-menu].open");
    for (var i = 0; i < menus.length; i++) {
      if (menus[i] !== except) {
        menus[i].classList.remove("open");
        var b = menus[i].querySelector(".card-menu-button");
        if (b) { b.setAttribute("aria-expanded", "false"); }
      }
    }
  }

  function wireCardMenus() {
    var menus = document.querySelectorAll("[data-card-menu]");
    for (var i = 0; i < menus.length; i++) {
      (function (menu) {
        var button = menu.querySelector(".card-menu-button");
        if (button) {
          button.addEventListener("click", function (e) {
            e.preventDefault();
            e.stopPropagation();
            var open = menu.classList.toggle("open");
            button.setAttribute("aria-expanded", open ? "true" : "false");
            closeMenus(open ? menu : null);
          });
        }
      })(menus[i]);
    }
    var traceItems = document.querySelectorAll("[data-trace-job]");
    for (var j = 0; j < traceItems.length; j++) {
      traceItems[j].addEventListener("click", function (e) {
        e.preventDefault();
        e.stopPropagation();
        closeMenus(null);
        openTraceDrawer(this.getAttribute("data-trace-job"));
      });
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

  document.addEventListener("DOMContentLoaded", function () {
    wireSSE();
    wireCards();
    var c = document.getElementById("fb-drawer-close");
    if (c) { c.addEventListener("click", closeDrawer); }
    document.addEventListener("click", function () { closeMenus(null); });
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") {
        closeMenus(null);
        closeDrawer();
      }
    });
  });
})();
