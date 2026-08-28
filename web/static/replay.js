// The replay viewer: a pure reducer over the trace's event stream.
//
// State at position N is the fold of events 0..N — the same bookkeeping rules
// the platform itself applies (solved: +payout −burned; failed: −burned;
// voided: refunded, a wash; dust burned at retirement). Scrubbing recomputes
// the fold from zero: at trace scale (hundreds of events) that is instant,
// and it keeps the reducer trivially correct instead of invertible.
//
// Live mode is the same reducer over a growing array: the server streams new
// trace events over SSE, each one is appended to EVENTS, and while "follow"
// is on the position tracks the tip. Scrubbing back through history while the
// episode continues is just turning follow off.
"use strict";

const EVENTS = window.EVENTS || [];
const LIVE = !!window.LIVE;

// Whether this episode ran -book open: the policy sits on the start line and
// nowhere else, so the feed can caption a book "sealed" only when it was.
// Scanned per call, not cached, because a live stream delivers the start
// event after the page loads. The scan short-circuits within the first few
// events on every real trace.
function bookIsOpen() {
  return EVENTS.some((e) => e.type === "episode" && e.action === "start" && e.book === "open");
}

function reduce(upto) {
  const s = {
    round: null, solved: 0, spent: 0, calls: 0, conservation: "",
    agents: new Map(), // id → {balance, grant, dead}
    board: new Map(),  // id → {tier, max, status, holder, failures}
  };
  for (let i = 0; i <= upto && i < EVENTS.length; i++) {
    const e = EVENTS[i];
    const b = s.board.get(e.id);
    const a = s.agents.get(e.agent);
    switch (e.type + "/" + (e.action || "")) {
      case "agent/spawned":
        s.agents.set(e.agent, { balance: e.grant, grant: e.grant, dead: false });
        break;
      case "agent/bankrupt":
        if (a) a.dead = true;
        break;
      case "episode/round":
        s.round = e.round;
        break;
      case "episode/end":
        s.conservation = e.conservation;
        break;
      case "bounty/posted":
        s.board.set(e.id, { tier: e.tier, max: e.max_payout, status: "open", holder: null, failures: e.failures || 0 });
        break;
      case "bounty/awarded":
        if (b) { b.status = "awarded"; b.holder = e.winner; }
        break;
      case "bounty/no_bids":
        if (b) { b.status = "open"; b.holder = null; }
        break;
      case "bounty/solved":
        if (b) b.status = "solved";
        if (a) a.balance += (e.payout || 0) - (e.burned || 0);
        s.solved++;
        break;
      case "bounty/failed":
        if (b) { b.status = "open"; b.holder = null; b.failures++; }
        if (a) a.balance -= e.burned || 0;
        break;
      case "bounty/voided":
        if (b) { b.status = "voided"; b.holder = null; }
        break;
      case "credit/dust_burn":
        if (a) a.balance -= e.amount || 0;
        break;
    }
    if (e.type === "model_call") {
      s.spent += e.cost || 0;
      s.calls++;
    }
  }
  return s;
}

// ---- rendering ----

const $ = (id) => document.getElementById(id);
const log = $("log"), scrub = $("scrub"), pos = $("pos"), play = $("play"), speed = $("speed");
const livebtn = $("livebtn"); // present only on a live page

let cur = 0;
let timer = null;
let follow = LIVE; // live pages start pinned to the tip

function esc(t) {
  const d = document.createElement("span");
  d.textContent = t;
  return d.innerHTML;
}

// describe words one event as a sentence. The trace carries a pre-worded
// label, but that label is a key=value dump — `action="solved" agent="gambler"
// burned=107 id="b0001" payout=36` — which is data, not information: the
// interesting number is buried in the middle of it and every row looks like
// every other row. Every field of the payload is already on the event object,
// so the log can say what happened instead of listing what was recorded.
//
// This is presentation only. The trace itself is untouched, and anything this
// function does not recognise falls back to the label it came with, so a new
// event kind degrades to the old behaviour rather than vanishing.
const who = (s) => `<span class="who">${esc(s)}</span>`;   // an actor: agent, bounty, suite
const fig = (n) => `<b>${esc(n)}</b>`;                      // the one number the row is about

// A metered call is booked to a wallet named att:<bounty>:r<round>. That is the
// right key for the ledger and the wrong thing to read, so unpack it.
function fromWallet(w) {
  const m = /^att:([^:]+):r(\d+)$/.exec(String(w || ""));
  return m ? `${who(m[1])} round ${esc(m[2])}` : esc(w || "—");
}

function describe(e) {
  switch (e.type + "/" + (e.action || "")) {
    case "agent/spawned":
      return `${who(e.agent)} enters with ${fig(e.grant)} credits`;
    case "agent/bankrupt":
      return `${who(e.agent)} is out of credits and retires`;

    case "episode/start":
      return `episode opens — ${fig(e.rounds)} rounds`;
    case "episode/round":
      return `round ${fig(e.round)}` + (e.postings ? ` — ${esc(e.postings)} bounties posted` : "");
    case "episode/end":
      return `episode closes — ${esc(e.conservation || "")}`;

    case "suite/registered":
      return `imported suite ${who(e.suite)} registered from ${esc(e.source || "an external source")}`;

    case "bounty/posted":
      return `${who(e.id)} posted at tier ${esc(e.tier)}, worth up to ${fig(e.max_payout)}` +
        (e.suite ? `, drawn from ${esc(e.suite)}` : ` — ${esc(e.generator)}`) +
        (e.failures ? ` (back on the board, ${esc(e.failures)}× failed)` : "");
    case "bounty/awarded":
      return `${who(e.winner)} wins ${who(e.id)} at ${fig(e.price)}` +
        (e.book && e.book.length > 1
          ? `, ${esc(e.book.length)} ${bookIsOpen() ? "bids in the open book" : "sealed bids"}`
          : "");
    case "bounty/no_bids":
      return `${who(e.id)} drew no bids`;
    case "bounty/solved":
      return `${who(e.agent)} solved ${who(e.id)} — paid ${fig(e.payout)}, ${esc(e.burned)} burned getting there`;
    case "bounty/failed":
      return `${who(e.agent)} failed ${who(e.id)} — ${esc(e.reason || "no reason given")}, ` +
        `${fig(e.burned)} burned for nothing`;
    case "bounty/judged":
      return `${who(e.agent)} graded ${e.pass ? "pass" : "fail"} on ${who(e.id)} by ` +
        `${esc(e.grader || "a model")} — ${esc(e.reason || "no reason given")}`;
    case "bounty/voided":
      return `${who(e.id)} voided, stake refunded — ${esc(e.reason || "no reason given")}`;

    case "credit/payout":
      return `${fig(e.amount)} paid to ${who(e.agent)} for ${esc(e.bounty)}`;
    case "credit/dust_burn":
      return `${fig(e.amount)} of dust burned from ${who(e.agent)} on retirement`;
  }
  if (e.type === "bid") {
    return `${who(e.agent)} asks ${fig(e.price)} for ${esc(e.bounty)}`;
  }
  if (e.type === "model_call") {
    const u = e.usage || {};
    return `${esc(e.model)} on ${fromWallet(e.wallet)} — ` +
      `${esc(u.input_tokens || 0)}→${esc(u.output_tokens || 0)} tokens, ${fig(e.cost)} metered` +
      (e.outcome && e.outcome !== "ok" ? `, ${esc(e.outcome)}` : "");
  }
  return esc(e.label || "");
}

function rowHTML(e, i) {
  // A round boundary is the only structural event in the stream, so it is the
  // only row that draws a rule above itself.
  const divide = e.type === "episode" && e.action === "round" ? " divide" : "";
  return `<div class="ev t-${esc(e.type)}${divide}" data-i="${i}">` +
    `<span class="seq">${e.seq}</span><span class="etype">${esc(e.type)}</span>` +
    `<span class="etext">${describe(e)}</span></div>`;
}

function buildLog() {
  log.innerHTML = EVENTS.map(rowHTML).join("");
  log.addEventListener("click", (ev) => {
    const row = ev.target.closest(".ev");
    if (row) { setFollow(false); setPos(Number(row.dataset.i)); }
  });
}

function renderState(s) {
  $("st-round").textContent = s.round === null ? "—" : s.round;
  $("st-solved").textContent = s.solved;
  $("st-spent").textContent = s.spent;
  $("st-conservation").textContent = s.conservation;

  let maxBal = 1;
  for (const [, a] of s.agents) maxBal = Math.max(maxBal, a.balance, a.grant);
  $("st-agents").innerHTML = [...s.agents].map(([id, a]) => {
    const pct = Math.max(0, Math.min(100, (a.balance / maxBal) * 100));
    return `<div class="ag ${a.dead ? "dead" : ""}">` +
      // Retirement is said in a word, not a dingbat: a skull is a different
      // glyph on every platform, and colour alone would carry the meaning.
      `<span class="ag-name">${esc(id)}${a.dead ? ` <span class="fate">retired</span>` : ""}</span>` +
      `<span class="ag-bal mono">${a.balance}</span>` +
      `<div class="bar"><div class="fill" style="width:${pct}%"></div></div></div>`;
  }).join("");

  const open = [...s.board].filter(([, b]) => b.status === "open" || b.status === "awarded");
  $("st-board").innerHTML = open.length ? open.map(([id, b]) =>
    `<div class="bo"><span class="mono">${esc(id)}</span> <span class="tier">t${b.tier}</span>` +
    `<span class="sub">max ${b.max}${b.failures ? " · " + b.failures + "× failed" : ""}` +
    `${b.holder ? " · with " + esc(b.holder) : ""}</span></div>`
  ).join("") : `<p class="sub">empty</p>`;
}

// WebKit has no way to style the played half of a range separately, so the
// track is a gradient and this is where its stop lives. Anything that moves
// the thumb — or changes what the far end means — has to repaint it.
function paintScrub() {
  const max = Number(scrub.max) || 0;
  scrub.style.setProperty("--p", `${max ? (cur / max) * 100 : 0}%`);
}

function setPos(i, scroll = true) {
  cur = Math.max(0, Math.min(EVENTS.length - 1, i));
  scrub.value = cur;
  paintScrub();
  pos.textContent = EVENTS.length ? `${cur + 1}/${EVENTS.length}` : "—";
  const prev = log.querySelector(".ev.current");
  if (prev) prev.classList.remove("current");
  const row = log.querySelector(`.ev[data-i="${cur}"]`);
  if (row) {
    row.classList.add("current");
    // Scroll the log pane only — scrollIntoView would drag the whole page.
    if (scroll) log.scrollTop = row.offsetTop - log.clientHeight / 2;
  }
  for (const r of log.children) {
    r.classList.toggle("future", Number(r.dataset.i) > cur);
  }
  renderState(reduce(cur));
}

function setPlaying(on) {
  if (timer) { clearInterval(timer); timer = null; }
  if (on) {
    setFollow(false);
    timer = setInterval(() => {
      if (cur >= EVENTS.length - 1) return setPlaying(false);
      setPos(cur + 1);
    }, Number(speed.value));
  }
  play.textContent = on ? "⏸" : "▶";
  // The glyph is the whole label, so the accessible name has to move with it.
  play.setAttribute("aria-label", on ? "pause" : "play");
}

function setFollow(on) {
  follow = LIVE && on;
  if (livebtn) livebtn.classList.toggle("off", !follow);
  if (livebtn) livebtn.setAttribute("aria-pressed", String(follow));
  if (follow) {
    setPlaying(false);
    setPos(EVENTS.length - 1);
  }
}

play.addEventListener("click", () => setPlaying(!timer));
speed.addEventListener("change", () => { if (timer) setPlaying(true); });
scrub.addEventListener("input", () => { setPlaying(false); setFollow(false); setPos(Number(scrub.value)); });
document.addEventListener("keydown", (e) => {
  // With the scrubber focused the arrows are already its own: the range steps
  // itself and its input event does the rest. Stepping again here would move
  // two events per press.
  if (e.target === scrub && (e.key === "ArrowRight" || e.key === "ArrowLeft")) return;
  if (e.key === "ArrowRight") { setPlaying(false); setFollow(false); setPos(cur + 1); }
  else if (e.key === "ArrowLeft") { setPlaying(false); setFollow(false); setPos(cur - 1); }
  else if (e.key === " " && e.target === document.body) { e.preventDefault(); setPlaying(!timer); }
});
if (livebtn) livebtn.addEventListener("click", () => setFollow(true));

scrub.max = Math.max(0, EVENTS.length - 1);
buildLog();
setPos(Math.max(0, LIVE ? EVENTS.length - 1 : 0));

// ---- the live feed ----

if (LIVE) {
  const last = EVENTS.length ? EVENTS[EVENTS.length - 1].seq : 0;
  const es = new EventSource(`/events?after=${last}`);
  es.onmessage = (m) => {
    const e = JSON.parse(m.data);
    EVENTS.push(e);
    log.insertAdjacentHTML("beforeend", rowHTML(e, EVENTS.length - 1));
    scrub.max = EVENTS.length - 1;
    if (follow) setPos(EVENTS.length - 1);
    else {
      paintScrub(); // the thumb held still, but the end of the track moved
      pos.textContent = `${cur + 1}/${EVENTS.length}`;
      log.lastElementChild.classList.add("future");
    }
  };
  // The server truncated under us: a new episode took the path. Start over.
  es.addEventListener("reset", () => location.reload());
}
