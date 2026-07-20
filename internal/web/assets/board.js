// board.js — the F12 board interactions: the SSE live hook (auto-refresh the
// panes off /v1/events) and the detail drawer that does NOT dim the board and
// supports click card -> card (build-list §G).
(function () {
  "use strict";

  var pollTimer = null;
  var refreshInFlight = false;

  // ── SSE live hook ── any lifecycle event reloads the live data (debounced).
  function wireSSE() {
    var live = document.querySelector("nav.fb-nav .live");
    try {
      // A project workspace requests only its exact project's lifecycle. Global
      // boards omit the parameter deliberately and therefore require an
      // explicit portfolio grant server-side. EventSource carries the existing
      // same-origin HttpOnly human-session cookie automatically.
      var workspace = document.querySelector("[data-conversation-workspace][data-project-id]");
      var eventsURL = "/v1/events";
      if (workspace) {
        eventsURL += "?project_id=" + encodeURIComponent(workspace.getAttribute("data-project-id"));
      }
      var es = new EventSource(eventsURL);
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

  // SSE is a low-latency nudge, not the source of truth. Some store folds can be
  // quiet and SSE itself is lossy, so poll the same read-only fragment every 30s.
  // The singleton guard prevents duplicate timers if this wiring is reused later;
  // hidden tabs wait until visible instead of doing background database reads.
  function wirePollingFallback() {
    if (pollTimer !== null) { return; }
    pollTimer = window.setInterval(function () {
      if (!document.hidden) { refresh(); }
    }, 30000);
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) { refresh(); }
    });
    window.addEventListener("beforeunload", function () {
      window.clearInterval(pollTimer);
      pollTimer = null;
    });
  }

  // refresh re-fetches the current view's pane fragment and swaps the board/fleet
  // body in place (so the open drawer is preserved — the board is never dimmed).
  function refresh() {
    var root = document.getElementById("fb-live");
    if (!root || refreshInFlight) { return; }
    // The Interactor workspace owns a durable conversation SSE cursor and its
    // own polling fallback. Replacing its DOM from the generic fleet wake-up
    // stream would destroy an in-progress draft and reset that cursor.
    if (root.querySelector("[data-conversation-workspace]")) { return; }
    refreshInFlight = true;
    // preserve the current query (e.g. the board's ?repo=<id> filter) so the live
    // refresh keeps the same view; just append partial=1 to get the body fragment.
    var params = new URLSearchParams(window.location.search);
    params.set("partial", "1");
    fetch(window.location.pathname + "?" + params.toString(), {
      headers: { "Accept": "text/html" },
      cache: "no-store"
    })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (html) {
        if (html === null) { return; }
        root.innerHTML = html;
        wireCards();
        wireTheme();
        wireDecisionInbox();
      })
      .catch(function () { /* transient: the next event or poll retries */ })
      .finally(function () { refreshInFlight = false; });
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

  // ── typed Needs You inbox ── the server remains authoritative. Every
  // response carries the exact request/artifact fence rendered on the card and
  // a session-persisted idempotency key, so a network-uncertain retry cannot
  // append a second human response.
  function decisionIdempotency(card, action) {
    var key = "flowbee-decision:" + card.dataset.projectId + ":" + card.dataset.decisionId + ":" +
      card.dataset.requestVersion + ":" + action;
    try {
      var existing = window.sessionStorage.getItem(key);
      if (existing) { return existing; }
      var value = "dashboard-" + (window.crypto && window.crypto.randomUUID ?
        window.crypto.randomUUID() : Date.now().toString(36) + "-" + Math.random().toString(36).slice(2));
      window.sessionStorage.setItem(key, value);
      return value;
    } catch (e) {
      return "dashboard-" + Date.now().toString(36) + "-" + Math.random().toString(36).slice(2);
    }
  }

  function clearDecisionIdempotency(card, action) {
    try {
      window.sessionStorage.removeItem("flowbee-decision:" + card.dataset.projectId + ":" +
        card.dataset.decisionId + ":" + card.dataset.requestVersion + ":" + action);
    } catch (e) { /* private mode */ }
  }

  function decisionStatus(card, message, kind) {
    var status = card.querySelector("[data-decision-status]");
    if (!status) { return; }
    status.textContent = message;
    status.classList.remove("error", "success");
    if (kind) { status.classList.add(kind); }
  }

  // The human session is HttpOnly and intentionally invisible here. The only
  // authentication-adjacent value JavaScript reads is the separate, readable
  // SameSite=Strict CSRF cookie, echoed on every state-changing request.
  function humanMutationHeaders(idempotency) {
    var csrf = "";
    var parts = document.cookie ? document.cookie.split(";") : [];
    for (var i = 0; i < parts.length; i++) {
      var pair = parts[i].trim();
      if (pair.indexOf("flowbee_csrf=") === 0) {
        try { csrf = decodeURIComponent(pair.slice("flowbee_csrf=".length)); } catch (e) { csrf = ""; }
        break;
      }
    }
    var headers = { "Accept": "application/json", "Content-Type": "application/json", "Idempotency-Key": idempotency };
    if (csrf) { headers["X-Flowbee-CSRF"] = csrf; }
    return headers;
  }

  function setDecisionBusy(card, busy) {
    var buttons = card.querySelectorAll("[data-decision-action], [data-decision-defer-toggle]");
    for (var i = 0; i < buttons.length; i++) { buttons[i].disabled = busy; }
    card.setAttribute("aria-busy", busy ? "true" : "false");
  }

  function decisionPayload(card, action) {
    var comment = card.querySelector("[data-decision-comment]");
    var body = {
      project_id: card.dataset.projectId,
      request_version: Number(card.dataset.requestVersion),
      subject_version: Number(card.dataset.subjectVersion),
      subject_sha256: card.dataset.subjectSha256,
      value: {},
      comment: comment ? comment.value.trim() : ""
    };
    if (action === "answer") {
      var picked = card.querySelector('input[name="answer-' + card.dataset.decisionId + '"]:checked');
      var free = card.querySelector("[data-decision-answer]");
      if (picked) {
        try { body.value = JSON.parse(picked.value); } catch (e) { throw new Error("The selected answer is invalid. Refresh and try again."); }
      } else if (free && free.value.trim()) {
        body.value = free.value.trim();
      } else {
        throw new Error("Choose or enter an answer first.");
      }
    }
    if (action === "request-changes" && !body.comment) {
      throw new Error("Describe the requested changes first.");
    }
    if (action === "defer") {
      var until = card.querySelector("[data-decision-defer-until]");
      var condition = card.querySelector("[data-decision-defer-condition]");
      body.defer_condition = condition ? condition.value.trim() : "";
      if (until && until.value) {
        var parsed = new Date(until.value);
        if (isNaN(parsed.getTime())) { throw new Error("Enter a valid resume time."); }
        body.defer_until = parsed.toISOString();
      }
      if (!body.defer_until && !body.defer_condition) {
        throw new Error("Set a resume time or durable condition.");
      }
    }
    if ((card.dataset.decisionKind === "authorization" || card.dataset.decisionKind === "exception") &&
        (action === "approve" || action === "answer")) {
      var scope = card.querySelector("[data-decision-authorization-scope]");
      var confirm = card.querySelector("[data-decision-confirm]");
      if (!scope || !scope.value.trim()) { throw new Error("Enter the exact authorization scope first."); }
      if (!confirm || !confirm.checked) { throw new Error("Confirm the exact scope and artifact fence first."); }
      body.authorization_scope = scope.value.trim();
    }
    return body;
  }

  function submitDecision(card, action) {
    var body;
    try {
      body = decisionPayload(card, action);
    } catch (e) {
      decisionStatus(card, e.message, "error");
      return;
    }
    var idempotency = decisionIdempotency(card, action);
    setDecisionBusy(card, true);
    decisionStatus(card, "Recording the exact response...", "");
    fetch("/v1/decisions/" + encodeURIComponent(card.dataset.decisionId) + "/" + action, {
      method: "POST",
      headers: humanMutationHeaders(idempotency),
      credentials: "same-origin",
      cache: "no-store",
      body: JSON.stringify(body)
    }).then(function (response) {
      if (response.ok) { return { ok: true, status: response.status, text: "" }; }
      return response.text().then(function (text) { return { ok: false, status: response.status, text: text.trim() }; });
    }).then(function (result) {
      if (!result.ok) {
        var stale = result.status === 409 || result.status === 412;
        decisionStatus(card, stale ? "This card changed. Refreshing durable state..." :
          (result.text || "The response was not accepted."), "error");
        if (stale) { window.setTimeout(refresh, 500); }
        return;
      }
      clearDecisionIdempotency(card, action);
      decisionStatus(card, "Recorded. Refreshing acknowledgement state...", "success");
      window.setTimeout(refresh, 250);
    }).catch(function () {
      decisionStatus(card, "Delivery is uncertain. Retry safely; this browser will reuse the same idempotency key.", "error");
    }).finally(function () { setDecisionBusy(card, false); });
  }

  function markDecisionViewed(card) {
    if (card.dataset.decisionState !== "open" || card.dataset.viewSent === "1") { return; }
    card.dataset.viewSent = "1";
    var action = "view";
    fetch("/v1/decisions/" + encodeURIComponent(card.dataset.decisionId) + "/view", {
      method: "POST",
      headers: humanMutationHeaders(decisionIdempotency(card, action)),
      credentials: "same-origin",
      cache: "no-store",
      body: JSON.stringify({ project_id: card.dataset.projectId, request_version: Number(card.dataset.requestVersion) })
    }).then(function (response) {
      if (response.ok) { clearDecisionIdempotency(card, action); card.dataset.decisionState = "viewed"; }
    }).catch(function () { card.dataset.viewSent = "0"; });
  }

  function wireDecisionInbox() {
    var cards = document.querySelectorAll("[data-decision-id]");
    for (var i = 0; i < cards.length; i++) {
      (function (card) {
        if (card.dataset.decisionWired === "1") { return; }
        card.dataset.decisionWired = "1";
        card.addEventListener("focusin", function () { markDecisionViewed(card); }, { once: true });
        var evidence = card.querySelector("[data-decision-evidence]");
        if (evidence) { evidence.addEventListener("toggle", function () { if (evidence.open) { markDecisionViewed(card); } }); }
        var toggle = card.querySelector("[data-decision-defer-toggle]");
        var deferForm = card.querySelector("[data-decision-defer-form]");
        if (toggle && deferForm) {
          toggle.addEventListener("click", function () {
            var show = deferForm.hidden;
            deferForm.hidden = !show;
            toggle.setAttribute("aria-expanded", show ? "true" : "false");
            if (show) {
              var first = deferForm.querySelector("input");
              if (first) { first.focus(); }
            }
          });
        }
        var actions = card.querySelectorAll("[data-decision-action]");
        for (var j = 0; j < actions.length; j++) {
          actions[j].addEventListener("click", function () { submitDecision(card, this.dataset.decisionAction); });
        }
      })(cards[i]);
    }
  }

  // ── project Interactor workspace ── conversation state comes exclusively
  // from the durable API. The Driver receipt/delivery projection remains
  // visually separate from agent-authored content, and SSE is only a wake-up:
  // every event causes an authoritative refetch from the database.
  var workspaceRuntime = null;

  function workspaceUUID() {
    if (window.crypto && window.crypto.randomUUID) { return window.crypto.randomUUID(); }
    return Date.now().toString(36) + "-" + Math.random().toString(36).slice(2);
  }

  function workspaceText(tag, className, value) {
    var node = document.createElement(tag);
    if (className) { node.className = className; }
    node.textContent = value || "";
    return node;
  }

  function workspaceStatus(root, message, kind) {
    var target = root.querySelector("[data-workspace-status]");
    if (!target) { return; }
    target.textContent = message;
    target.classList.remove("error", "success");
    if (kind) { target.classList.add(kind); }
  }

  function workspaceConnection(root, message, kind) {
    var target = root.querySelector("[data-workspace-connection]");
    if (!target) { return; }
    target.textContent = message;
    target.classList.remove("live", "error", "waiting");
    if (kind) { target.classList.add(kind); }
  }

  function workspaceFetchJSON(url, options) {
    options = options || {};
    options.credentials = "same-origin";
    options.cache = "no-store";
    options.headers = options.headers || { "Accept": "application/json" };
    return fetch(url, options).then(function (response) {
      if (response.ok) { return response.json(); }
      return response.text().then(function (body) {
        var error = new Error(body.trim() || "Flowbee request failed");
        error.status = response.status;
        throw error;
      });
    });
  }

  function workspaceThreadURL(runtime, threadID, suffix) {
    return "/v1/conversations/" + encodeURIComponent(threadID) + suffix +
      (suffix.indexOf("?") >= 0 ? "&" : "?") + "project_id=" + encodeURIComponent(runtime.projectID);
  }

  function workspaceSelectedThread(runtime, threads) {
    var requested = new URLSearchParams(window.location.search).get("thread") || "";
    if (!requested) {
      try { requested = window.localStorage.getItem("flowbee-workspace-thread:" + runtime.projectID) || ""; } catch (e) { /* private mode */ }
    }
    for (var i = 0; i < threads.length; i++) {
      if (threads[i].id === requested) { return threads[i]; }
    }
    return threads.length ? threads[0] : null;
  }

  function workspaceRenderThreads(runtime) {
    var root = runtime.root;
    var list = root.querySelector("[data-workspace-threads]");
    var count = root.querySelector("[data-workspace-thread-count]");
    if (count) { count.textContent = String(runtime.threads.length); }
    if (!list) { return; }
    list.replaceChildren();
    if (!runtime.threads.length) {
      var empty = workspaceText("p", "iw-empty", "No Interactor thread is registered for this project yet.");
      list.appendChild(empty);
      return;
    }
    runtime.threads.forEach(function (thread) {
      var button = document.createElement("button");
      button.type = "button";
      button.className = "iw-thread" + (runtime.thread && runtime.thread.id === thread.id ? " active" : "");
      button.dataset.threadId = thread.id;
      button.appendChild(workspaceText("strong", "", thread.title || thread.conversation_key));
      button.appendChild(workspaceText("span", "", thread.focus_kind + " · " + thread.focus_ref));
      button.appendChild(workspaceText("small", "", thread.last_message_seq + " messages · " + thread.state));
      button.addEventListener("click", function () { workspaceSelectThread(runtime, thread); });
      list.appendChild(button);
    });
  }

  function workspaceDeliveryLabel(message) {
    if (message.role !== "human") {
      return message.stream_state === "streaming" ? "Interactor is responding" : "Interactor response";
    }
    if ((message.delivery_state === "pending" || message.delivery_state === "failed") && message.delivery_error) {
      return "Driver route unavailable · durably held";
    }
    var labels = {
      pending: "Queued for Driver routing", routing: "Authorizing Driver route",
      submitted: "Inserted by Driver · awaiting Interactor evidence",
      acknowledged: "Acknowledged by Interactor", uncertain: "Driver delivery uncertain · not resent",
      failed: "Delivery failed · recovery pending", fenced: "Route fenced · awaiting replacement"
    };
    return labels[message.delivery_state] || message.delivery_state || "Delivery pending";
  }

  function workspaceRenderMessages(runtime) {
    var list = runtime.root.querySelector("[data-workspace-messages]");
    if (!list) { return; }
    list.replaceChildren();
    if (!runtime.messages.length) {
      var empty = document.createElement("div");
      empty.className = "iw-empty";
      empty.appendChild(workspaceText("strong", "", "Start the project conversation"));
      empty.appendChild(workspaceText("span", "", "Your message will be persisted before Driver-routed delivery."));
      list.appendChild(empty);
      return;
    }
    runtime.messages.forEach(function (message) {
      var article = document.createElement("article");
      var role = message.role === "human" ? "human" : (message.role === "interactor" ? "interactor" : "system");
      article.className = "iw-message " + role;
      article.dataset.messageId = message.id;
      var head = document.createElement("header");
      head.appendChild(workspaceText("strong", "", role === "human" ? "You" : (role === "interactor" ? "Interactor" : "Flowbee")));
      head.appendChild(workspaceText("time", "", message.created_at ? new Date(message.created_at).toLocaleString() : ""));
      article.appendChild(head);
      if (message.content_text) { article.appendChild(workspaceText("p", "iw-message-copy", message.content_text)); }
      if (message.content_artifact_ref) {
        var artifact = workspaceText("code", "iw-artifact", "artifact · " + message.content_artifact_ref);
        artifact.title = message.content_sha256 || "";
        article.appendChild(artifact);
      }
      var delivery = workspaceText("footer", "iw-delivery " + (message.delivery_state || ""), workspaceDeliveryLabel(message));
      if (message.delivery_error) { delivery.title = message.delivery_error; }
      article.appendChild(delivery);
      list.appendChild(article);
    });
    list.scrollTop = list.scrollHeight;
  }

  function workspaceRenderRoute(runtime) {
    var thread = runtime.thread;
    var title = runtime.root.querySelector("[data-workspace-thread-title]");
    var focus = runtime.root.querySelector("[data-workspace-thread-focus]");
    var route = runtime.root.querySelector("[data-workspace-focus]");
    var binding = runtime.root.querySelector("[data-workspace-binding]");
    if (!thread) {
      if (title) { title.textContent = "No thread selected"; }
      if (focus) { focus.textContent = "Project focus"; }
      if (route) { route.textContent = "No exact Interactor route registered"; }
      if (binding) { binding.textContent = "route unavailable"; }
      return;
    }
    if (title) { title.textContent = thread.title || thread.conversation_key; }
    if (focus) { focus.textContent = thread.focus_kind + " · " + thread.focus_ref +
      (thread.focus_artifact_sha256 ? " · " + thread.focus_artifact_sha256 : ""); }
    if (route) { route.textContent = thread.interactor_actor_id + " · incarnation " + thread.interactor_incarnation_id; }
    if (binding) { binding.textContent = "binding " + thread.interactor_binding_id; }
  }

  function workspaceLoadMessages(runtime) {
    if (!runtime.thread || runtime.loadingMessages) { return Promise.resolve(); }
    runtime.loadingMessages = true;
    var threadID = runtime.thread.id;
    var all = [];
    var digest = 0;
    function page(after) {
      return workspaceFetchJSON(workspaceThreadURL(runtime, threadID, "/messages?after=" + after + "&limit=200"))
        .then(function (body) {
          if (!runtime.thread || runtime.thread.id !== threadID) { return; }
          var rows = Array.isArray(body.messages) ? body.messages : [];
          all = all.concat(rows);
          digest = Number(body.digest_seq) || digest;
          if (rows.length === 200 && Number(body.next_after) > after) { return page(Number(body.next_after)); }
        });
    }
    return page(0).then(function () {
      if (!runtime.thread || runtime.thread.id !== threadID) { return; }
      runtime.messages = all;
      runtime.eventCursor = digest;
      workspaceRenderMessages(runtime);
      workspaceStatus(runtime.root, "Durable history is current.", "success");
    }).catch(function (error) {
      if (error.status === 401 || error.status === 403) {
        workspaceStatus(runtime.root, "Sign in with a Flowbee dashboard link to open this project conversation.", "error");
      } else {
        workspaceStatus(runtime.root, "Conversation reload failed; durable state is unchanged and polling will retry.", "error");
      }
    }).finally(function () { runtime.loadingMessages = false; });
  }

  function workspaceOpenStream(runtime) {
    if (runtime.stream) { runtime.stream.close(); runtime.stream = null; }
    if (!runtime.thread) { return; }
    var threadID = runtime.thread.id;
    try {
      var url = workspaceThreadURL(runtime, threadID, "/events?after=" + runtime.eventCursor);
      var stream = new EventSource(url);
      runtime.stream = stream;
      var wake = function (event) {
        if (!runtime.thread || runtime.thread.id !== threadID) { return; }
        var seq = Number(event.lastEventId);
        if (seq > runtime.eventCursor) { runtime.eventCursor = seq; }
        // Focus/route metadata and message bodies are separate durable rows.
        // Refresh the thread projection when its identity/focus may have moved;
        // delivery-only events can take the cheaper message path.
        if (event.type === "focus_changed" || event.type === "thread_created") {
          workspaceLoadThreads(runtime);
        } else {
          workspaceLoadMessages(runtime);
        }
      };
      stream.onopen = function () {
        workspaceConnection(runtime.root, runtime.driverControlAvailable ? "Live" : "History live · control held",
          runtime.driverControlAvailable ? "live" : "waiting");
      };
      stream.onerror = function () { workspaceConnection(runtime.root, "Reconnecting", "waiting"); };
      stream.onmessage = wake;
      ["thread_created", "message_appended", "delivery_changed", "focus_changed"].forEach(function (kind) {
        stream.addEventListener(kind, wake);
      });
    } catch (e) { workspaceConnection(runtime.root, "Polling", "waiting"); }
  }

  function workspaceDraftKey(runtime) {
    return "flowbee-conversation-draft:" + runtime.projectID + ":" + (runtime.thread ? runtime.thread.id : "none");
  }

  function workspaceReadDraft(runtime) {
    try {
      var raw = window.sessionStorage.getItem(workspaceDraftKey(runtime));
      if (!raw) { return null; }
      var parsed = JSON.parse(raw);
      return parsed && typeof parsed.content === "string" && typeof parsed.key === "string" ? parsed : null;
    } catch (e) { return null; }
  }

  function workspaceWriteDraft(runtime, content) {
    var previous = workspaceReadDraft(runtime);
    var draft = previous && previous.content === content ? previous : { content: content, key: "dashboard-message-" + workspaceUUID() };
    try { window.sessionStorage.setItem(workspaceDraftKey(runtime), JSON.stringify(draft)); } catch (e) { /* private mode */ }
    return draft;
  }

  function workspaceRestoreDraft(runtime) {
    var input = runtime.root.querySelector("[data-workspace-input]");
    var send = runtime.root.querySelector("[data-workspace-send]");
    var draft = workspaceReadDraft(runtime);
    if (input) { input.value = draft ? draft.content : ""; input.disabled = !runtime.thread || !runtime.driverControlAvailable; }
    if (send) { send.disabled = !runtime.driverControlAvailable || !runtime.thread || !input || !input.value.trim(); }
  }

  function workspaceSelectThread(runtime, thread) {
    runtime.thread = thread;
    runtime.messages = [];
    runtime.eventCursor = 0;
    try { window.localStorage.setItem("flowbee-workspace-thread:" + runtime.projectID, thread.id); } catch (e) { /* private mode */ }
    var url = new URL(window.location.href);
    url.searchParams.set("project", runtime.projectID);
    url.searchParams.set("thread", thread.id);
    window.history.replaceState(null, "", url.pathname + "?" + url.searchParams.toString());
    workspaceRenderThreads(runtime);
    workspaceRenderRoute(runtime);
    workspaceRestoreDraft(runtime);
    workspaceLoadMessages(runtime).then(function () { workspaceOpenStream(runtime); });
  }

  function workspaceLoadThreads(runtime) {
    return workspaceFetchJSON("/v1/projects/" + encodeURIComponent(runtime.projectID) + "/conversations")
      .then(function (body) {
        runtime.threads = Array.isArray(body.conversations) ? body.conversations : [];
        var selected = workspaceSelectedThread(runtime, runtime.threads);
        if (selected && runtime.thread && runtime.thread.id === selected.id) {
          runtime.thread = selected;
          workspaceRenderThreads(runtime);
          workspaceRenderRoute(runtime);
          workspaceRestoreDraft(runtime);
          return workspaceLoadMessages(runtime);
        }
        if (selected) { workspaceSelectThread(runtime, selected); }
        else {
          runtime.thread = null;
          workspaceRenderThreads(runtime);
          workspaceRenderRoute(runtime);
          workspaceRestoreDraft(runtime);
          workspaceConnection(runtime.root, "No route", "waiting");
        }
      }).catch(function (error) {
        runtime.threads = [];
        workspaceRenderThreads(runtime);
        workspaceRenderRoute(runtime);
        workspaceConnection(runtime.root, error.status === 401 ? "Sign in required" : "Offline", "error");
        workspaceStatus(runtime.root, error.status === 401 || error.status === 403 ?
          "Sign in with a Flowbee dashboard link to access this project." :
          "Project threads could not be loaded; retrying from durable state.", "error");
      });
  }

  function workspaceSend(runtime) {
    if (!runtime.thread || runtime.sending) { return; }
    if (!runtime.driverControlAvailable) {
      workspaceStatus(runtime.root, "Driver control route unavailable. Flowbee will not use a direct tmux fallback.", "error");
      return;
    }
    var input = runtime.root.querySelector("[data-workspace-input]");
    var send = runtime.root.querySelector("[data-workspace-send]");
    var content = input ? input.value.trim() : "";
    if (!content) { return; }
    var draft = workspaceWriteDraft(runtime, content);
    runtime.sending = true;
    if (input) { input.disabled = true; }
    if (send) { send.disabled = true; }
    workspaceStatus(runtime.root, "Persisting message before Driver-routed delivery…", "");
    workspaceFetchJSON("/v1/conversations/" + encodeURIComponent(runtime.thread.id) + "/messages", {
      method: "POST",
      headers: humanMutationHeaders(draft.key),
      body: JSON.stringify({ project_id: runtime.projectID, role: "human", content_text: content, stream_state: "complete" })
    }).then(function () {
      try { window.sessionStorage.removeItem(workspaceDraftKey(runtime)); } catch (e) { /* private mode */ }
      if (input) { input.value = ""; }
      workspaceStatus(runtime.root, "Persisted. Driver delivery status will update independently.", "success");
      return workspaceLoadMessages(runtime);
    }).catch(function (error) {
      workspaceStatus(runtime.root, error.status === 409 ?
        "The conversation changed. Your draft and idempotency key are preserved; reloading before retry." :
        "Delivery is uncertain. Retry safely; this draft retains the same idempotency key.", "error");
      if (error.status === 409) { workspaceLoadMessages(runtime); }
    }).finally(function () {
      runtime.sending = false;
      if (input) { input.disabled = !runtime.driverControlAvailable; }
      if (send) { send.disabled = !runtime.driverControlAvailable || !input || !input.value.trim(); }
      if (input) { input.focus(); }
    });
  }

  function wireConversationWorkspace() {
    var root = document.querySelector("[data-conversation-workspace]");
    if (!root || root.dataset.workspaceWired === "1") { return; }
    root.dataset.workspaceWired = "1";
    var runtime = { root: root, projectID: root.dataset.projectId, threads: [], thread: null,
      driverControlAvailable: root.dataset.driverControlAvailable !== "false",
      messages: [], eventCursor: 0, stream: null, poll: null, loadingMessages: false, sending: false };
    workspaceRuntime = runtime;
    var form = root.querySelector("[data-workspace-composer]");
    var input = root.querySelector("[data-workspace-input]");
    var send = root.querySelector("[data-workspace-send]");
    if (form) { form.addEventListener("submit", function (event) { event.preventDefault(); workspaceSend(runtime); }); }
    if (input) {
      input.addEventListener("input", function () {
        workspaceWriteDraft(runtime, input.value);
        if (send) { send.disabled = !runtime.driverControlAvailable || !runtime.thread || !input.value.trim() || runtime.sending; }
      });
      input.addEventListener("keydown", function (event) {
        if (event.key === "Enter" && !event.shiftKey) { event.preventDefault(); workspaceSend(runtime); }
      });
    }
    workspaceLoadThreads(runtime);
    runtime.poll = window.setInterval(function () {
      // Re-listing is intentional: a replacement Interactor may establish a new
      // thread while the old thread's SSE has no event to announce its sibling.
      if (!document.hidden) { workspaceLoadThreads(runtime); }
    }, 15000);
    window.addEventListener("beforeunload", function () {
      if (runtime.stream) { runtime.stream.close(); }
      if (runtime.poll) { window.clearInterval(runtime.poll); }
    });
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
    wirePollingFallback();
    wireCards();
    wireTheme();
    wireDecisionInbox();
    wireConversationWorkspace();
    var c = document.getElementById("fb-drawer-close");
    if (c) { c.addEventListener("click", closeDrawer); }
    document.addEventListener("keydown", function (e) { if (e.key === "Escape") { closeDrawer(); } });
  });
})();
