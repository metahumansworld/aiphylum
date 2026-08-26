// The replay viewer: a pure reducer over the trace's event stream.
//
// State at position N is the fold of events 0..N — the same bookkeeping rules
// the platform itself applies (solved: +payout −burned; failed: −burned;
// voided: refunded, a wash; dust burned at retirement). Scrubbing recomputes
// the fold from zero: at trace scale (hundreds of events) that is instant,
// and it keeps the reducer trivially correct instead of invertible. This is
// the component the live spectator view grows from — swap the static EVENTS
// array for a stream and the reducer does not change.
"use strict";

const EVENTS = window.EVENTS || [];

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

let cur = 0;
let timer = null;

function esc(t) {
  const d = document.createElement("span");
  d.textContent = t;
  return d.innerHTML;
}

function buildLog() {
  log.innerHTML = EVENTS.map((e, i) =>
    `<div class="ev t-${esc(e.type)}" data-i="${i}">` +
    `<span class="seq">#${e.seq}</span><span class="etype">${esc(e.type)}</span>` +
    `<span class="elabel">${esc(e.label)}</span></div>`
  ).join("");
  log.addEventListener("click", (ev) => {
    const row = ev.target.closest(".ev");
    if (row) setPos(Number(row.dataset.i));
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
      `<span class="ag-name">${a.dead ? "☠ " : ""}${esc(id)}</span>` +
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

function setPos(i, scroll = true) {
  cur = Math.max(0, Math.min(EVENTS.length - 1, i));
  scrub.value = cur;
  pos.textContent = `${cur + 1}/${EVENTS.length}`;
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
    timer = setInterval(() => {
      if (cur >= EVENTS.length - 1) return setPlaying(false);
      setPos(cur + 1);
    }, Number(speed.value));
  }
  play.textContent = on ? "⏸" : "▶";
}

play.addEventListener("click", () => setPlaying(!timer));
speed.addEventListener("change", () => { if (timer) setPlaying(true); });
scrub.addEventListener("input", () => { setPlaying(false); setPos(Number(scrub.value)); });
document.addEventListener("keydown", (e) => {
  if (e.key === "ArrowRight") { setPlaying(false); setPos(cur + 1); }
  else if (e.key === "ArrowLeft") { setPlaying(false); setPos(cur - 1); }
  else if (e.key === " " && e.target === document.body) { e.preventDefault(); setPlaying(!timer); }
});

scrub.max = Math.max(0, EVENTS.length - 1);
buildLog();
setPos(0);
