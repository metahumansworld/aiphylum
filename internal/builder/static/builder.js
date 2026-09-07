// The builder page. One spec document, edited two ways: by hand on the
// canvas, where each field is a node, and by talking, where each draft comes
// back from the builder model and lands on the same nodes. Everything the
// page shows is put in as text; nothing from a spec or a reply becomes
// markup.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const SESSION_KEY = "soscitea.session";
  const LAYOUT_KEY = (id) => "soscitea.layout." + (id || "new");
  const DEFAULT_CEILING = 256;

  // ---- state ---------------------------------------------------------
  const state = {
    session: null,
    me: null,
    models: { offered: [], locked: [] },
    billing: null, // {open, min, max} when the recharge is open
    agents: [],
    current: null, // {id, spec} or null for a new one
    spec: blankSpec(),
    dirty: false,
    undo: null, // the spec before the last draft
    conv: "",
  };

  function blankSpec() {
    return { version: 1, name: "", model: "", persona: "", greeting: "", rules: [], max_reply_tokens: DEFAULT_CEILING, tools: [], webhook: "" };
  }

  // ---- storage, which may be absent ----------------------------------
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* fine */ } },
    del(k) { try { localStorage.removeItem(k); } catch { /* fine */ } },
  };

  // ---- http ----------------------------------------------------------
  async function api(method, path, body, opts = {}) {
    const headers = { "Content-Type": "application/json" };
    if (state.session && !opts.public) headers.Authorization = "Bearer " + state.session;
    const resp = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    let data = null;
    const text = await resp.text();
    if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
    if (resp.status === 401 && !opts.public) { signOut("Your session has ended. Sign in again."); }
    return { ok: resp.ok, status: resp.status, data: data || {}, headers: resp.headers };
  }

  // The balance is credits, never dollars: the ledger's integer as-is, so
  // there is no published exchange rate for a topup's fees to show through.
  function credits(n) {
    return Number(n).toLocaleString("en-US");
  }

  let toastTimer = 0;
  function toast(text, bad) {
    const t = $("toast");
    t.textContent = text;
    t.classList.toggle("bad", !!bad);
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 5000);
  }

  // ---- sign in -------------------------------------------------------
  async function boot() {
    const url = new URL(location.href);
    const token = url.searchParams.get("token");
    const topup = url.searchParams.get("topup");
    if (topup) history.replaceState({}, "", "/");
    if (token) {
      history.replaceState({}, "", "/");
      const r = await api("POST", "/auth/verify", { token });
      if (r.ok && r.data.session) {
        state.session = r.data.session;
        store.set(SESSION_KEY, state.session);
      } else {
        showSignIn(r.data.error || "That link did not work. Ask for another.");
        return;
      }
    } else {
      state.session = store.get(SESSION_KEY);
    }
    if (!state.session) { showSignIn(""); return; }
    const me = await api("GET", "/auth/me");
    if (!me.ok) { showSignIn(""); return; }
    state.me = me.data;
    await enterApp();
    if (topup === "ok") toast("Thank you. Your credits land the moment Stripe confirms the payment.");
  }

  function showSignIn(status) {
    $("signin").hidden = false;
    $("app").hidden = true;
    $("who").hidden = true;
    $("signin-status").textContent = status;
  }

  function signOut(reason) {
    state.session = null;
    state.me = null;
    store.del(SESSION_KEY);
    showSignIn(reason || "");
  }

  $("signin-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const email = $("signin-email").value.trim();
    if (!email) return;
    $("signin-status").textContent = "Sending…";
    const r = await api("POST", "/auth/request", { email });
    $("signin-status").textContent = r.ok
      ? "A link is on its way to " + email + ". Offline, it is printed in the server's log."
      : (r.data.error || "Could not send a link.");
  });

  $("signout").addEventListener("click", async () => {
    await api("POST", "/auth/signout");
    signOut("Signed out.");
  });

  // ---- app -----------------------------------------------------------
  async function enterApp() {
    $("signin").hidden = true;
    $("app").hidden = false;
    $("who").hidden = false;
    renderWho();
    const m = await api("GET", "/v1/models");
    if (m.ok) state.models = m.data;
    const b = await api("GET", "/billing", undefined, { public: true });
    state.billing = b.ok && b.data.open ? b.data : null;
    $("topup").hidden = !state.billing;
    await loadAgents();
    if (state.agents.length) selectAgent(state.agents[0]); else selectNew();
  }

  function renderWho() {
    $("email").textContent = state.me.email;
    $("balance").textContent = credits(state.me.balance) + (state.me.paid ? " credits left" : " credits of your grant left");
  }

  // ---- recharge ------------------------------------------------------
  function showTopup(open) {
    $("topup").hidden = !state.billing || open;
    $("topup-menu").hidden = !state.billing || !open;
  }
  $("topup").addEventListener("click", () => showTopup(true));
  $("topup-menu").addEventListener("click", async (e) => {
    const cents = Number(e.target.dataset.cents);
    if (!cents) { showTopup(false); return; }
    e.target.disabled = true;
    const r = await api("POST", "/billing/checkout", { cents });
    e.target.disabled = false;
    if (r.ok && r.data.url) location.href = r.data.url;
    else toast(r.data.error || "Could not open a checkout.", true);
  });

  async function refreshMe() {
    const me = await api("GET", "/auth/me");
    if (me.ok) { state.me = me.data; renderWho(); }
  }

  async function loadAgents() {
    const r = await api("GET", "/v1/agents");
    state.agents = r.ok ? (r.data.agents || []) : [];
    renderAgentList();
  }

  function renderAgentList() {
    const ul = $("agent-list");
    ul.replaceChildren();
    for (const a of state.agents) {
      const li = document.createElement("li");
      if (state.current && state.current.id === a.id) li.classList.add("current");
      const b = document.createElement("button");
      b.type = "button";
      const name = document.createElement("span");
      name.className = "name";
      name.textContent = a.spec.name || "(unnamed)";
      const meta = document.createElement("span");
      meta.className = "meta";
      meta.textContent = a.spec.model + " · " + (a.spec.rules || []).length + " rules";
      b.append(name, meta);
      b.addEventListener("click", () => { if (confirmDiscard()) selectAgent(a); });
      li.append(b);
      ul.append(li);
    }
    $("agents-empty").hidden = state.agents.length > 0;
  }

  function confirmDiscard() {
    return !state.dirty || confirm("Leave this agent without saving its changes?");
  }

  function selectAgent(a) {
    state.current = { id: a.id, spec: a.spec };
    state.spec = normalise(structuredClone(a.spec));
    state.dirty = false;
    state.undo = null;
    state.conv = "";
    $("try-log").replaceChildren();
    $("chat-log").replaceChildren();
    $("undo").hidden = true;
    renderAll();
  }

  function selectNew() {
    state.current = null;
    state.spec = blankSpec();
    state.spec.model = state.models.offered[0] || "";
    state.dirty = false;
    state.undo = null;
    state.conv = "";
    $("try-log").replaceChildren();
    $("chat-log").replaceChildren();
    $("undo").hidden = true;
    renderAll();
  }

  function normalise(s) {
    return {
      version: 1,
      name: s.name || "",
      model: s.model || "",
      persona: s.persona || "",
      greeting: s.greeting || "",
      rules: Array.isArray(s.rules) ? s.rules.slice() : [],
      max_reply_tokens: s.max_reply_tokens || DEFAULT_CEILING,
      tools: Array.isArray(s.tools) ? s.tools.map((t) => ({
        name: t.name || "", description: t.description || "", url: t.url || "", method: t.method === "POST" ? "POST" : "GET",
        params: Array.isArray(t.params) ? t.params.map((p) => ({ name: p.name || "", description: p.description || "" })) : [],
      })) : [],
      // Kept as the instruction alone: an empty one is no webhook.
      webhook: s.webhook && s.webhook.instruction ? s.webhook.instruction : "",
    };
  }

  $("new-agent").addEventListener("click", () => { if (confirmDiscard()) selectNew(); });

  // ---- canvas --------------------------------------------------------
  const NODES = [
    { key: "hub", title: "Agent", hub: true },
    { key: "persona", title: "Persona" },
    { key: "greeting", title: "Greeting" },
    { key: "rules", title: "Rules" },
    { key: "ceiling", title: "Reply ceiling" },
    { key: "tools", title: "Tools" },
    { key: "webhook", title: "Webhook" },
  ];
  const nodeEls = {};
  let positions = {};

  // Three columns: the fields on either side, the agent between them, the
  // tools under it and the webhook beside them. The widths are the stylesheet's node widths; the canvas
  // scrolls if it is narrower than the three of them.
  function defaultPositions() {
    const c = $("canvas");
    const side = 232, hub = 256, pad = 24;
    const w = Math.max(c.clientWidth, side * 2 + hub + pad * 4), h = Math.max(c.clientHeight, 620);
    const ceilingY = Math.max(340, h - 200);
    return {
      hub: { x: Math.round((w - hub) / 2), y: 40 },
      persona: { x: pad, y: 40 },
      greeting: { x: pad, y: Math.max(340, h - 230) },
      rules: { x: w - pad - side, y: 40 },
      ceiling: { x: w - pad - side, y: ceilingY },
      tools: { x: Math.round((w - hub) / 2), y: Math.max(500, h - 140) },
      webhook: { x: w - pad - side, y: ceilingY + 220 },
    };
  }

  function loadLayout() {
    const raw = store.get(LAYOUT_KEY(state.current && state.current.id));
    let saved = null;
    if (raw) { try { saved = JSON.parse(raw); } catch { saved = null; } }
    positions = Object.assign(defaultPositions(), saved || {});
  }

  function saveLayout() {
    store.set(LAYOUT_KEY(state.current && state.current.id), JSON.stringify(positions));
  }

  function renderAll() {
    $("canvas-title").textContent = state.current ? (state.spec.name || "(unnamed)") : "New agent";
    $("delete").hidden = !state.current;
    $("delete").textContent = "Delete";
    $("try").hidden = !state.current;
    renderEndpoint();
    renderDirty();
    renderAgentList();
    loadLayout();
    buildNodes();
    drawEdges();
  }

  function renderEndpoint() {
    const p = $("endpoint");
    if (!state.current) { p.hidden = true; return; }
    p.hidden = false;
    p.replaceChildren();
    p.append("Reachable at ");
    const code = document.createElement("code");
    code.textContent = location.origin + "/a/" + state.current.id;
    p.append(code, " — a POST of {\"text\": …} to /messages on it gets a reply. ");
    // The widget: one line for a page, and the chat page it opens.
    const snippet = `<script src="${location.origin}/widget.js" data-agent="${state.current.id}" async></script>`;
    const copy = document.createElement("button");
    copy.type = "button";
    copy.className = "quiet";
    copy.textContent = "Copy the embed snippet";
    copy.addEventListener("click", async () => {
      try { await navigator.clipboard.writeText(snippet); toast("Copied. Paste it into any page's HTML."); }
      catch { window.prompt("Copy this into any page's HTML:", snippet); }
    });
    const open = document.createElement("a");
    open.href = "/a/" + state.current.id + "/embed";
    open.target = "_blank";
    open.rel = "noopener";
    open.textContent = "Open the chat page";
    p.append(copy, " · ", open);
  }

  function renderDirty() {
    $("dirty").hidden = !state.dirty;
    $("save").disabled = !state.dirty && !!state.current;
  }

  function markDirty() {
    if (!state.dirty) { state.dirty = true; renderDirty(); }
    if (state.current) $("canvas-title").textContent = state.spec.name || "(unnamed)";
  }

  function buildNodes() {
    const canvas = $("canvas");
    for (const k of Object.keys(nodeEls)) { nodeEls[k].remove(); delete nodeEls[k]; }
    for (const n of NODES) {
      const el = document.createElement("section");
      el.className = "node" + (n.hub ? " hub" : "");
      el.dataset.key = n.key;
      const head = document.createElement("div");
      head.className = "node-head";
      const h3 = document.createElement("h3");
      h3.textContent = n.title;
      head.append(h3);
      const count = document.createElement("span");
      count.className = "count";
      head.append(count);
      const body = document.createElement("div");
      body.className = "node-body";
      el.append(head, body);
      fillNode(n.key, body, count);
      const pos = positions[n.key];
      el.style.left = pos.x + "px";
      el.style.top = pos.y + "px";
      makeDraggable(el, head, n.key);
      canvas.append(el);
      nodeEls[n.key] = el;
    }
  }

  function field(labelText, control) {
    const l = document.createElement("label");
    l.textContent = labelText;
    return [l, control];
  }

  function fillNode(key, body, count) {
    const s = state.spec;
    if (key === "hub") {
      const name = document.createElement("input");
      name.type = "text";
      name.maxLength = 64;
      name.value = s.name;
      name.placeholder = "What it is called";
      name.addEventListener("input", () => { s.name = name.value; markDirty(); });
      body.append(...field("Name", name));
      const ml = document.createElement("label");
      ml.textContent = "Model";
      body.append(ml, renderModels());
      return;
    }
    if (key === "persona" || key === "greeting") {
      const t = document.createElement("textarea");
      t.rows = key === "persona" ? 6 : 3;
      t.maxLength = key === "persona" ? 4096 : 1024;
      t.value = s[key];
      t.placeholder = key === "persona"
        ? "Who it is, in the second person: “You are the front-of-house voice of…”"
        : "The first thing it says";
      t.addEventListener("input", () => { s[key] = t.value; markDirty(); });
      body.append(t);
      const hint = document.createElement("p");
      hint.className = "hint";
      hint.textContent = key === "persona"
        ? "Sent with every message, so every byte here is paid for on every reply."
        : "Shown before anyone has said anything. Costs nothing.";
      body.append(hint);
      return;
    }
    if (key === "rules") {
      const ul = document.createElement("ul");
      ul.className = "rules";
      const redraw = () => {
        ul.replaceChildren();
        s.rules.forEach((rule, i) => {
          const li = document.createElement("li");
          const inp = document.createElement("input");
          inp.type = "text";
          inp.maxLength = 512;
          inp.value = rule;
          inp.addEventListener("input", () => { s.rules[i] = inp.value; markDirty(); });
          const rm = document.createElement("button");
          rm.type = "button";
          rm.className = "remove";
          rm.textContent = "×";
          rm.setAttribute("aria-label", "Remove rule " + (i + 1));
          rm.addEventListener("click", () => { s.rules.splice(i, 1); markDirty(); redraw(); });
          li.append(inp, rm);
          ul.append(li);
        });
        count.textContent = s.rules.length + " of 32";
        add.disabled = s.rules.length >= 32;
        drawEdges();
      };
      const add = document.createElement("button");
      add.type = "button";
      add.className = "add-rule";
      add.textContent = "Add a rule";
      add.addEventListener("click", () => {
        s.rules.push("");
        markDirty();
        redraw();
        const last = ul.querySelector("li:last-child input");
        if (last) last.focus();
      });
      body.append(ul, add);
      redraw();
      return;
    }
    if (key === "ceiling") {
      const n = document.createElement("input");
      n.type = "number";
      n.min = 1; n.max = 4096; n.step = 1;
      n.value = s.max_reply_tokens;
      n.addEventListener("input", () => {
        const v = parseInt(n.value, 10);
        if (v >= 1 && v <= 4096) { s.max_reply_tokens = v; markDirty(); }
      });
      body.append(...field("Tokens per reply", n));
      const hint = document.createElement("p");
      hint.className = "hint";
      hint.textContent = "The most it may say in one reply, and what is held before each one. 256 is a short paragraph.";
      body.append(hint);
      return;
    }
    if (key === "tools") {
      const ul = document.createElement("ul");
      ul.className = "tools";
      const redraw = () => {
        ul.replaceChildren();
        s.tools.forEach((tool, i) => ul.append(toolItem(tool, i, () => { s.tools.splice(i, 1); markDirty(); redraw(); })));
        count.textContent = s.tools.length + " of 8";
        add.disabled = s.tools.length >= 8;
        drawEdges();
      };
      const add = document.createElement("button");
      add.type = "button";
      add.className = "add-rule";
      add.textContent = "Add a tool";
      add.addEventListener("click", () => {
        s.tools.push({ name: "", description: "", url: "", method: "GET", params: [] });
        markDirty();
        redraw();
        const last = ul.querySelector("li:last-child input");
        if (last) last.focus();
      });
      const hint = document.createElement("p");
      hint.className = "hint";
      hint.textContent = "Your own HTTP endpoints, https to a public host. The model decides when to call one; the platform makes the call and hands back what came out, up to 8 KiB. No keys: put what the endpoint needs in the URL.";
      body.append(ul, add, hint);
      redraw();
      return;
    }
    if (key === "webhook") {
      const t = document.createElement("textarea");
      t.rows = 3;
      t.maxLength = 1024;
      t.value = s.webhook;
      t.placeholder = "What an event is and what to do with one: “An order came in. Thank the customer by name.”";
      const where = document.createElement("p");
      where.className = "hint";
      const show = () => {
        count.textContent = s.webhook.trim() ? "on" : "off";
        where.replaceChildren();
        if (!s.webhook.trim()) { where.textContent = "Off. Write an instruction and the agent gets an endpoint your own systems can POST to."; return; }
        if (!state.current) { where.textContent = "Save, and the endpoint appears here."; return; }
        const code = document.createElement("code");
        code.textContent = location.origin + "/a/" + state.current.id + "/events";
        where.append("POST any body up to 4 KiB to ", code, " and the reply comes back. No secret: it is as public as a message, and rate-limited the same.");
      };
      t.addEventListener("input", () => { s.webhook = t.value; markDirty(); show(); });
      body.append(t, where);
      show();
    }
  }

  // One tool's fields. Params are edited as lines of "name: what to write"
  // and kept as the spec's list; a line with no name is dropped on Save.
  function toolItem(tool, i, remove) {
    const li = document.createElement("li");
    const head = document.createElement("div");
    head.className = "tool-head";
    const name = document.createElement("input");
    name.type = "text";
    name.maxLength = 32;
    name.pattern = "[a-z][a-z0-9_]*";
    name.placeholder = "name, like check_stock";
    name.value = tool.name;
    name.addEventListener("input", () => { tool.name = name.value; markDirty(); });
    const method = document.createElement("select");
    for (const m of ["GET", "POST"]) {
      const o = document.createElement("option");
      o.value = m; o.textContent = m; o.selected = tool.method === m;
      method.append(o);
    }
    method.addEventListener("change", () => { tool.method = method.value; markDirty(); });
    const rm = document.createElement("button");
    rm.type = "button";
    rm.className = "remove";
    rm.textContent = "×";
    rm.setAttribute("aria-label", "Remove tool " + (i + 1));
    rm.addEventListener("click", remove);
    head.append(name, method, rm);
    const url = document.createElement("input");
    url.type = "url";
    url.maxLength = 1024;
    url.placeholder = "https://your.site/api/stock";
    url.value = tool.url;
    url.addEventListener("input", () => { tool.url = url.value; markDirty(); });
    const desc = document.createElement("input");
    desc.type = "text";
    desc.maxLength = 256;
    desc.placeholder = "When to call it: “How many of a tea are on the shelf.”";
    desc.value = tool.description;
    desc.addEventListener("input", () => { tool.description = desc.value; markDirty(); });
    const params = document.createElement("textarea");
    params.rows = 2;
    params.placeholder = "params, one per line: tea: the tea, by name";
    params.value = tool.params.map((p) => p.description ? p.name + ": " + p.description : p.name).join("\n");
    params.addEventListener("input", () => {
      tool.params = params.value.split("\n").map((line) => {
        const [n, ...rest] = line.split(":");
        return { name: n.trim(), description: rest.join(":").trim() };
      }).filter((p) => p.name);
      markDirty();
    });
    li.append(head, url, desc, params);
    return li;
  }

  function renderModels() {
    const wrap = document.createElement("div");
    wrap.className = "models";
    const waiting = new Set((state.me && state.me.waiting) || []);
    const paid = !!(state.me && state.me.paid);
    const locked = state.models.locked || [];
    const redraw = () => {
      wrap.replaceChildren();
      // A person who has added credits picks from every model; the grant
      // covers only the offered one, and the tag says which is which.
      for (const id of [...(state.models.offered || []), ...(paid ? locked : [])]) {
        const b = document.createElement("button");
        b.type = "button";
        b.className = state.spec.model === id ? "chosen" : "";
        const code = document.createElement("code");
        code.textContent = id;
        b.append(code);
        if (state.spec.model === id) {
          const tag = document.createElement("span");
          tag.className = "lock";
          tag.textContent = locked.includes(id) ? "on your credits" : "on your grant";
          b.append(tag);
        }
        b.addEventListener("click", () => { state.spec.model = id; markDirty(); redraw(); });
        wrap.append(b);
      }
      for (const id of paid ? [] : locked) {
        const b = document.createElement("button");
        b.type = "button";
        b.className = "locked";
        const code = document.createElement("code");
        code.textContent = id;
        const tag = document.createElement("span");
        tag.className = "lock";
        tag.textContent = state.billing ? "locked · add credits to unlock"
          : waiting.has("model:" + id) ? "on the waitlist" : "locked · join the waitlist";
        b.append(code, tag);
        b.addEventListener("click", async () => {
          if (state.billing) { showTopup(true); return; }
          const r = await api("POST", "/waitlist", { reason: "model:" + id });
          if (r.ok) {
            waiting.add("model:" + id);
            if (state.me) state.me.waiting = [...waiting];
            toast("You are on the waitlist for " + id + ".");
            redraw();
          }
          else toast(r.data.error || "Could not join the waitlist.", true);
        });
        wrap.append(b);
      }
    };
    redraw();
    return wrap;
  }

  function makeDraggable(el, handle, key) {
    let start = null;
    handle.addEventListener("pointerdown", (e) => {
      if (e.button !== 0) return;
      start = { x: e.clientX, y: e.clientY, left: el.offsetLeft, top: el.offsetTop };
      el.classList.add("dragging");
      handle.setPointerCapture(e.pointerId);
      e.preventDefault();
    });
    handle.addEventListener("pointermove", (e) => {
      if (!start) return;
      const x = Math.max(0, start.left + e.clientX - start.x);
      const y = Math.max(0, start.top + e.clientY - start.y);
      el.style.left = x + "px";
      el.style.top = y + "px";
      positions[key] = { x, y };
      drawEdges();
    });
    const end = () => {
      if (!start) return;
      start = null;
      el.classList.remove("dragging");
      saveLayout();
    };
    handle.addEventListener("pointerup", end);
    handle.addEventListener("pointercancel", end);
  }

  function centre(el) {
    return { x: el.offsetLeft + el.offsetWidth / 2, y: el.offsetTop + el.offsetHeight / 2 };
  }

  function drawEdges() {
    const svg = $("edges");
    const hub = nodeEls.hub;
    if (!hub) return;
    const canvas = $("canvas");
    svg.setAttribute("width", canvas.scrollWidth);
    svg.setAttribute("height", canvas.scrollHeight);
    svg.replaceChildren();
    const h = centre(hub);
    for (const n of NODES) {
      if (n.hub || !nodeEls[n.key]) continue;
      const c = centre(nodeEls[n.key]);
      const line = document.createElementNS("http://www.w3.org/2000/svg", "line");
      line.setAttribute("x1", h.x); line.setAttribute("y1", h.y);
      line.setAttribute("x2", c.x); line.setAttribute("y2", c.y);
      svg.append(line);
    }
  }
  window.addEventListener("resize", drawEdges);

  // ---- save and delete ----------------------------------------------
  function cleanSpec() {
    const s = state.spec;
    return {
      version: 1,
      name: s.name.trim(),
      model: s.model,
      persona: s.persona,
      greeting: s.greeting,
      rules: s.rules.map((r) => r.trim()).filter(Boolean),
      max_reply_tokens: s.max_reply_tokens,
      tools: s.tools.filter((t) => t.name.trim() || t.url.trim()).map((t) => ({
        name: t.name.trim(), description: t.description.trim(), url: t.url.trim(), method: t.method,
        params: t.params.filter((p) => p.name).map((p) => ({ name: p.name, description: p.description })),
      })),
      ...(s.webhook.trim() ? { webhook: { instruction: s.webhook.trim() } } : {}),
    };
  }

  $("save").addEventListener("click", async () => {
    const spec = cleanSpec();
    if (!spec.name) { toast("Give it a name first.", true); return; }
    $("save").disabled = true;
    const r = state.current
      ? await api("PUT", "/v1/agents/" + state.current.id, spec)
      : await api("POST", "/v1/agents", spec);
    if (!r.ok) {
      toast(r.data.error || "Could not save.", true);
      $("save").disabled = false;
      return;
    }
    const wasNew = !state.current;
    state.current = { id: r.data.id, spec: r.data.spec };
    state.spec = normalise(structuredClone(r.data.spec));
    state.dirty = false;
    state.conv = "";
    $("try-log").replaceChildren();
    await loadAgents();
    renderAll();
    toast(wasNew ? "Saved. It has an endpoint now: try it on the right." : "Saved. Its conversations start afresh.");
  });

  $("delete").addEventListener("click", async () => {
    const b = $("delete");
    if (!state.current) return;
    if (b.textContent !== "Really delete") { b.textContent = "Really delete"; b.classList.add("danger"); return; }
    const r = await api("DELETE", "/v1/agents/" + state.current.id);
    b.classList.remove("danger");
    if (!r.ok) { toast(r.data.error || "Could not delete.", true); b.textContent = "Delete"; return; }
    store.del(LAYOUT_KEY(state.current.id));
    await loadAgents();
    toast("Deleted. Its endpoint is gone.");
    if (state.agents.length) selectAgent(state.agents[0]); else selectNew();
  });

  // ---- chat to spec --------------------------------------------------
  function logChat(list, cls, text, cost) {
    const li = document.createElement("li");
    li.className = cls;
    li.textContent = text;
    if (cost !== undefined) {
      const c = document.createElement("span");
      c.className = "cost";
      c.textContent = cost;
      li.append(c);
    }
    list.append(li);
    list.scrollTop = list.scrollHeight;
  }

  $("chat-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const request = $("chat-input").value.trim();
    if (!request) return;
    const log = $("chat-log");
    logChat(log, "you", request);
    $("chat-input").value = "";
    $("draft").disabled = true;
    const before = cleanSpec();
    const r = await api("POST", "/v1/draft", { spec: before, request });
    $("draft").disabled = false;
    if (!r.ok) {
      const why = r.status === 402
        ? "Not enough of the grant is left to hold a draft. Messages are smaller; those may still go through."
        : (r.data.error || "The builder could not answer.");
      logChat(log, "refused", why);
      return;
    }
    state.undo = before;
    $("undo").hidden = false;
    state.spec = normalise(r.data.spec);
    markDirty();
    logChat(log, "note", r.data.note || "Revised.", "cost " + credits(r.data.cost) + " · " + credits(r.data.balance) + " credits left");
    state.me.balance = r.data.balance;
    renderWho();
    buildNodes();
    drawEdges();
  });

  $("undo").addEventListener("click", () => {
    if (!state.undo) return;
    state.spec = normalise(state.undo);
    state.undo = null;
    $("undo").hidden = true;
    markDirty();
    buildNodes();
    drawEdges();
    logChat($("chat-log"), "note", "Undid the last draft.");
  });

  // ---- try it ----------------------------------------------------------
  $("try-reset").addEventListener("click", () => { state.conv = ""; $("try-log").replaceChildren(); $("try-status").textContent = ""; });

  $("try-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (!state.current) return;
    const text = $("try-input").value.trim();
    if (!text) return;
    const log = $("try-log");
    logChat(log, "you", text);
    $("try-input").value = "";
    $("try-status").textContent = state.dirty ? "Answering as the saved version — the canvas has unsaved changes." : "";
    const r = await api("POST", "/a/" + state.current.id + "/messages", { conversation: state.conv, text }, { public: true });
    if (!r.ok) {
      let why = r.data.error || "No reply.";
      if (r.status === 429) why = "Its public endpoint is being messaged faster than it answers — you share that bucket with strangers. Try again in " + (r.headers.get("Retry-After") || "a few") + " s.";
      if (r.status === 402) why = "Your grant is spent. The agent stays reachable and answers nobody until it is topped up.";
      logChat(log, "refused", why);
      return;
    }
    state.conv = r.data.conversation;
    logChat(log, "agent", r.data.reply);
    refreshMe();
  });

  boot();
})();
