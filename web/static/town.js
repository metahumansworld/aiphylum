// The town viewer: the same pure-reducer discipline as the replay page, drawn
// as a chunky isometric village-city. State at position N is the fold of town
// events 0..N; scrubbing recomputes from zero. The map, the roster and the
// palette all come from the founding event — the first line of every town
// trace — so this file knows nothing about any particular town: it renders by
// place kind (home, shop, plaza, market, park, street) and would draw any map
// that speaks that vocabulary.
//
// The art direction is the mobile-village idiom: heavy dark outlines, warm
// saturated colour, stone footings under timbered walls, tiled roofs with deep
// eaves, gold trim, and props scattered on the ground. It is original vector
// work in that style, not anybody's assets.
"use strict";

const EVENTS = window.EVENTS || [];
const LIVE = !!window.LIVE;

// ---- the fold ----

function reduce(upto) {
  const s = {
    founded: null, day: null, clock: null,
    tick: 0,              // how many ticks have been folded — the animator's clock
    residents: new Map(), // id → {name, x, y, place, activity, path}
    feed: [],             // arrivals, meetings, speech and reflection, newest last
    meetings: 0,
    bubble: null,         // {who, text} — the line currently over someone's head
    bubbleAge: 0,         // ticks folded since it was said; two and it is stale
  };
  for (let i = 0; i <= upto && i < EVENTS.length; i++) {
    const e = EVENTS[i];
    // The fair's economy events ride the same stream as the town's own. They
    // carry no clock — they are stamped with the tick they happened in, which
    // is exact because the seam that produces them fires after the tick frame.
    // On a plain town trace none of these types occur and this block is inert.
    const money = (kind, who, extra) => {
      s.feed.push(Object.assign({ clock: s.clock, day: s.day, kind, who }, extra || {}));
    };
    if (e.type === "bounty") {
      switch (e.action) {
        case "posted": money("posted", [], { bounty: e.id, tier: e.tier, generator: e.generator, max: e.max_payout }); break;
        case "awarded": money("awarded", [e.winner], { bounty: e.id, price: e.price }); break;
        case "no_bids": money("nobids", [], { bounty: e.id }); break;
        case "solved": money("solved", [e.agent], { bounty: e.id, payout: e.payout }); break;
        case "failed": money("failed", [e.agent], { bounty: e.id, burned: e.burned }); break;
        case "voided": money("voided", e.agent ? [e.agent] : [], { bounty: e.id, reason: e.reason }); break;
      }
      continue;
    }
    if (e.type === "bid") { money("bid", [e.agent], { bounty: e.bounty, price: e.price }); continue; }
    if (e.type === "note" && e.note === "bounty shelved") { money("shelved", [], { bounty: e.bounty, windows: e.windows }); continue; }
    if (e.type === "agent" && e.action === "bankrupt") { money("bankrupt", [e.agent]); continue; }
    if (e.type === "credit" && e.action === "stayed") { money("stayed", [e.agent], { place: e.place, ticks: e.ticks, amount: e.amount }); continue; }
    if (e.type === "agent" && e.action === "memo") { money("memo", [e.agent], { text: e.memo }); continue; }
    if (e.type !== "town") continue;
    switch (e.action) {
      case "founded":
        s.founded = e;
        for (const r of e.residents) {
          s.residents.set(r.id, { name: r.name, x: r.x, y: r.y, place: r.place, activity: "", path: null });
        }
        break;
      case "tick":
        s.tick++;
        if (s.bubble) s.bubbleAge++;
        s.day = e.day; s.clock = e.clock;
        for (const f of e.residents) {
          const r = s.residents.get(f.id);
          if (!r) continue;
          r.x = f.x; r.y = f.y; r.place = f.place; r.activity = f.activity;
          r.path = f.path || null; // absent in traces written before routes existed
        }
        break;
      case "arrive":
        s.feed.push({ clock: e.clock, day: e.day, kind: "arrive", who: [e.resident], place: e.place, activity: e.activity });
        break;
      case "met":
        s.meetings++;
        s.feed.push({ clock: e.clock, day: e.day, kind: "met", who: [e.a, e.b], place: e.place });
        break;
      case "said":
        s.feed.push({ clock: e.clock, day: e.day, kind: "said", who: [e.resident, e.to], place: e.place, text: e.text });
        s.bubble = { who: e.resident, text: e.text };
        s.bubbleAge = 0;
        break;
      case "reflected":
        s.feed.push({ clock: e.clock, day: e.day, kind: "reflected", who: [e.resident], text: e.text });
        break;
    }
  }
  if (s.feed.length > 40) s.feed = s.feed.slice(s.feed.length - 40);
  return s;
}

// ---- the projection ----
//
// Classic 2:1 isometric: one grid cell is a 64x32 diamond. The camera sits to
// the south-east, so of a building's four walls it is the south and east faces
// that are drawn, with a pitched roof over them. When somebody is inside, that
// shell fades to a ghost and you see them through it.

const HW = 32, HH = 16;         // half a tile, in svg units
const SKEW = 26.565;            // atan(HH/HW) — the slope of every wall base
const CELLPX = Math.hypot(HW, HH); // one step, in svg units: every step is equal
const BASE = 9;                 // the stone footing every building stands on
const WALL = { home: 40, shop: 52 };
const ROOF = { home: 30, shop: 34 };
const EAVE = 0.26;              // how far the roof oversails the walls, in cells

let OX = 0;
const OY = 96;                  // headroom: the tallest roof in the corner still fits
const C = (gx, gy) => [OX + (gx - gy) * HW, OY + (gx + gy) * HH];

const P = (list) => list.map((p) => p[0].toFixed(1) + "," + p[1].toFixed(1)).join(" ");
const up = (p, h) => [p[0], p[1] - h];
const mid = (a, b, t) => [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t];
const poly = (list, fill, cls) => `<polygon class="${cls || "ol"}" points="${P(list)}" fill="${fill}"/>`;

// hash is the town's only randomness: a fixed function of the cell, so every
// reload grows the same trees. Nothing on this page is allowed to wobble.
const hash = (x, y) => (((x + 7) * 2654435761) ^ ((y + 13) * 97531)) >>> 0;

function shade(hex, f) {
  const n = parseInt(hex.slice(1), 16);
  const c = (v) => Math.round(Math.min(255, v * f));
  return `rgb(${c(n >> 16 & 255)},${c(n >> 8 & 255)},${c(n & 255)})`;
}
function tint(hex, f) {
  const n = parseInt(hex.slice(1), 16);
  const c = (v) => Math.round(v + (255 - v) * f);
  return `rgb(${c(n >> 16 & 255)},${c(n >> 8 & 255)},${c(n & 255)})`;
}

// ---- the palette ----

const GRASS = ["#69b246", "#61a840"], GRASS_DK = "#4e8f33";
const PAVE = ["#ded0aa", "#d6c69e"];
const COBBLE = ["#efe3c2", "#e5d7b3", "#d8c8a1"];
const PARKG = ["#7cc04f", "#72b647"];
const STONE = "#cfc7b6", STONE_DK = "#a89e8b";
const WOOD = "#8b5a33", WOOD_DK = "#6d4526";
const GOLD = "#f2c245";
// Six roofs, and no two towns' worth of hue between them. Blue and violet are
// not available to this palette, so the two slots they held are now a bone
// tile and a slate one — which pays for itself twice, because a town roofed
// partly in the same whites as the page reads as one design rather than as a
// picture pasted onto it.
const ROOFPAL = [
  ["#d1503f", "#a83a2c"], ["#e6dcc4", "#c2b394"], ["#4fa07c", "#397a5d"],
  ["#c9803a", "#a05f26"], ["#7d6f63", "#584e44"], ["#c94f7c", "#9e3a5e"],
];
const WALLPAL = ["#f0e0c1", "#e9d4b1", "#f4e6ca", "#e4d3b7"];
const STRIPES = [["#e05a4e", "#fbf1dc"], ["#3f9e7a", "#fbf1dc"], ["#e6a02e", "#fbf1dc"], ["#c9527a", "#fbf1dc"]];
const FLOWERS = ["#ff5d8f", "#ffd23f", "#ff8b5e", "#f7cfe0", "#fff6e8"];

// LOOKS are the costumes, handed out by roster order. Hat, prop and build all
// vary together, because a hat alone stops working the moment two residents
// are small on the far side of the square — the outline has to differ too.
const LOOKS = [
  { skin: "#ffd2ab", hair: "#7a4a25", tunic: "#e8637c", trim: "#fdf3e3", hat: "chef", prop: "loaf", build: "apron" },
  { skin: "#e8b98c", hair: "#a8a49a", tunic: "#54493c", trim: "#e8dcc4", hat: "scholar", prop: "book", build: "robe" },
  { skin: "#c98d5e", hair: "#2f2117", tunic: "#3fa08a", trim: "#f2c245", hat: "scarf", prop: "satchel", build: "wrap" },
  { skin: "#f0c39a", hair: "#8a5a2a", tunic: "#a4632f", trim: "#f5e3c0", hat: "beard", prop: "tankard", build: "stout" },
  { skin: "#ffd2ab", hair: "#3b2a1d", tunic: "#7d3346", trim: "#e8d3d8", hat: "hood", prop: "", build: "robe" },
  { skin: "#e0a878", hair: "#c9a227", tunic: "#6f9e3f", trim: "#f4ead0", hat: "straw", prop: "basket", build: "apron" },
  { skin: "#d9a06b", hair: "#14100c", tunic: "#c2802e", trim: "#33271a", hat: "scarf", prop: "tankard", build: "plain" },
];

// BUILDS are the outlines. sh is the half-width at the shoulder, hp at the hip,
// and hem is where the garment stops — a robe that reaches the ankles hides the
// legs drawn under it, which is the whole difference between Osric and Pell at
// twenty pixels tall.
const BUILDS = {
  apron: { sh: 6.5, hp: 8, hem: -10 },
  robe: { sh: 6.8, hp: 10.5, hem: -2 },
  wrap: { sh: 6.2, hp: 7.4, hem: -12 },
  stout: { sh: 8.4, hp: 9.6, hem: -9 },
  plain: { sh: 7, hp: 8.5, hem: -10 },
};

// ---- rendering ----

const $ = (id) => document.getElementById(id);
const mapEl = $("map"), roster = $("roster"), feed = $("feed"), clockEl = $("clock");
const scrub = $("scrub"), pos = $("pos"), play = $("play"), speed = $("speed");
const livebtn = $("livebtn");

let cur = 0, timer = null, follow = LIVE;
let names = new Map(), placeNames = new Map(), colorOf = new Map();
let insideOf = new Map(); // "x,y" → the id of the building that cell is inside
let frontRow = new Map(); // building id → the depth row its solid art is drawn in

function esc(t) {
  const d = document.createElement("span");
  d.textContent = t == null ? "" : t;
  return d.innerHTML;
}

// ---- things that stand on the ground ----

function tree(x, y, big) {
  const [px, py] = C(x + 0.5, y + 0.5);
  const r = big ? 13 : 10, h = big ? 22 : 17;
  const dk = "#2f7d3a", lt = "#48a04a", hi = "#63bb59";
  return `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="${r}" ry="${r / 2.4}"/>` +
    `<rect class="ol" x="${px - 3}" y="${py - h}" width="6" height="${h}" rx="2" fill="${WOOD}"/>` +
    `<circle class="ol" cx="${px - r * 0.5}" cy="${py - h - r * 0.5}" r="${r * 0.72}" fill="${dk}"/>` +
    `<circle class="ol" cx="${px + r * 0.5}" cy="${py - h - r * 0.4}" r="${r * 0.68}" fill="${dk}"/>` +
    `<circle class="ol" cx="${px}" cy="${py - h - r}" r="${r}" fill="${lt}"/>` +
    `<circle cx="${px - r * 0.3}" cy="${py - h - r * 1.3}" r="${r * 0.4}" fill="${hi}"/></g>`;
}

function bush(x, y, h) {
  const [px, py] = C(x + 0.4 + (h % 5) / 12, y + 0.45 + ((h >>> 4) % 5) / 12);
  return `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="9" ry="4"/>` +
    `<circle class="ol" cx="${px - 4}" cy="${py - 4}" r="5.5" fill="#3d8f3c"/>` +
    `<circle class="ol" cx="${px + 4}" cy="${py - 4}" r="5" fill="#3d8f3c"/>` +
    `<circle class="ol" cx="${px}" cy="${py - 7}" r="6.5" fill="#4aa348"/></g>`;
}

function rock(x, y, h) {
  const [px, py] = C(x + 0.45 + (h % 7) / 16, y + 0.5);
  const r = 5 + (h % 3) * 1.6;
  return `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="${r + 2}" ry="${r / 2}"/>` +
    poly([[px - r, py], [px - r * 0.6, py - r * 0.9], [px + r * 0.5, py - r], [px + r, py - r * 0.2], [px + r * 0.5, py]], STONE_DK) +
    poly([[px - r * 0.6, py - r * 0.9], [px + r * 0.5, py - r], [px + r * 0.1, py - r * 0.5], [px - r * 0.3, py - r * 0.5]], STONE, "") +
    `</g>`;
}

function barrel(x, y) {
  const [px, py] = C(x, y);
  return `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="7" ry="3.5"/>` +
    `<rect class="ol" x="${px - 6}" y="${py - 14}" width="12" height="14" rx="3" fill="${WOOD}"/>` +
    `<rect x="${px - 6}" y="${py - 10}" width="12" height="2.4" fill="${WOOD_DK}"/>` +
    `<rect x="${px - 6}" y="${py - 5}" width="12" height="2.4" fill="${WOOD_DK}"/>` +
    `<ellipse class="ol" cx="${px}" cy="${py - 14}" rx="6" ry="2.6" fill="${tint(WOOD, 0.3)}"/></g>`;
}

function crate(x, y) {
  const [px, py] = C(x, y);
  return `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="8" ry="4"/>` +
    poly([[px - 8, py - 4], [px, py], [px + 8, py - 4], [px, py - 8]], tint(WOOD, 0.34)) +
    poly([[px - 8, py - 4], [px, py], [px, py - 10], [px - 8, py - 14]], shade(WOOD, 0.8)) +
    poly([[px + 8, py - 4], [px, py], [px, py - 10], [px + 8, py - 14]], WOOD) +
    poly([[px - 8, py - 14], [px, py - 10], [px + 8, py - 14], [px, py - 18]], tint(WOOD, 0.34)) + `</g>`;
}

// torch is the street light: a post, a gold brazier, and a flame that only the
// evening switches on.
function torch(gx, gy) {
  const [px, py] = C(gx, gy);
  return {
    solid: `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="6" ry="3"/>` +
      `<rect class="ol" x="${px - 2.5}" y="${py - 30}" width="5" height="30" rx="2" fill="${WOOD_DK}"/>` +
      `<path class="ol" d="M${px - 6},${py - 30} L${px + 6},${py - 30} L${px + 4},${py - 38} L${px - 4},${py - 38} Z" fill="${GOLD}"/>` +
      `<rect x="${px - 5}" y="${py - 32}" width="10" height="2.5" fill="${shade(GOLD, 0.7)}"/></g>`,
    lit: `<g><circle class="lamp-glow" cx="${px}" cy="${py - 40}" r="16"/>` +
      `<path class="flame" d="M${px},${py - 50} C${px + 6},${py - 42} ${px + 5},${py - 37} ${px},${py - 37} C${px - 5},${py - 37} ${px - 6},${py - 42} ${px},${py - 50} Z"/>` +
      `<circle class="lamp-core" cx="${px}" cy="${py - 40}" r="3"/></g>`,
  };
}

function stall(x, y) {
  const stripe = STRIPES[hash(x, y) % STRIPES.length];
  const lift = 26, inset = 0.1;
  const t = up(C(x + 0.5, y + inset), lift), r = up(C(x + 1 - inset, y + 0.5), lift);
  const b = up(C(x + 0.5, y + 1 - inset), lift), l = up(C(x + inset, y + 0.5), lift);
  const [px, py] = C(x + 0.5, y + 0.5);
  return `<g><ellipse class="shadow" cx="${px}" cy="${py}" rx="17" ry="8"/>` +
    `<rect class="ol" x="${l[0] - 2}" y="${l[1]}" width="4" height="${lift}" fill="${WOOD_DK}"/>` +
    `<rect class="ol" x="${r[0] - 2}" y="${r[1]}" width="4" height="${lift}" fill="${WOOD_DK}"/>` +
    poly([b, l, t], stripe[1]) + poly([b, r, t], stripe[0]) +
    poly([b, l, up(l, -5), up(b, -5)], shade(stripe[1], 0.86)) +
    poly([b, r, up(r, -5), up(b, -5)], shade(stripe[0], 0.86)) +
    crate(x + 0.72, y + 0.78) + `</g>`;
}

function fountain(p) {
  const [cx, cy] = C(p.x + p.w / 2, p.y + p.h / 2);
  const dm = (r, h) => P([[cx, cy - r * HH - h], [cx + r * HW, cy - h], [cx, cy + r * HH - h], [cx - r * HW, cy - h]]);
  return `<g><polygon class="ol" points="${dm(1.25, 0)}" fill="${STONE_DK}"/>` +
    `<polygon class="ol" points="${dm(1.25, 8)}" fill="${STONE}"/>` +
    `<polygon points="${dm(0.92, 8)}" fill="#cfe3d5"/>` +
    `<polygon points="${dm(0.6, 8)}" fill="#eef7f0"/>` +
    `<rect class="ol" x="${cx - 5}" y="${cy - 30}" width="10" height="22" rx="3" fill="${STONE}"/>` +
    `<polygon class="ol" points="${P([[cx - 9, cy - 30], [cx + 9, cy - 30], [cx, cy - 38]])}" fill="${GOLD}"/>` +
    `<circle class="spray" cx="${cx - 10}" cy="${cy - 20}" r="2.6"/>` +
    `<circle class="spray" cx="${cx + 10}" cy="${cy - 19}" r="2.6"/></g>`;
}

// ---- buildings ----
//
// A building is a stone footing, two visible plastered walls under a pitched
// tiled roof, and whatever trim its kind earns. Walls and roof go in one
// "shell" group: when a resident is inside, the shell fades and the interior
// floor, and the person standing on it, show through.

// doorSide works out which wall the emitted door cell belongs to, by asking the
// same questions the router asked in the same order: south if there is open
// ground below it, then east. A corner cell answers both, and this is what
// keeps the painted doorway on the wall people actually come out of. North and
// west doors face away from the camera and are simply not drawn.
function doorSide(p, open) {
  const d = p.door;
  if (!d) return null;
  if (d.y === p.y + p.h - 1 && open(d.x, d.y + 1)) return "south";
  if (d.x === p.x + p.w - 1 && open(d.x + 1, d.y)) return "east";
  return null;
}

function building(p, hue, roofPal, side) {
  const wh = WALL[p.kind] || 42, rh = ROOF[p.kind] || 28;
  const A = C(p.x, p.y), B = C(p.x + p.w, p.y);
  const D = C(p.x, p.y + p.h), E = C(p.x + p.w, p.y + p.h);
  let s = "", lit = "";

  // The footing: the whole footprint diamond raised a little, so the walls
  // stand on stone rather than straight out of the grass.
  s += poly([[D[0], D[1] + 3], [E[0], E[1] + 3], [B[0], B[1] + 3], [A[0], A[1] + 3]], STONE_DK, "ol2");
  s += poly([D, E, up(E, -3), up(D, -3)], STONE_DK);
  s += poly([E, B, up(B, -3), up(E, -3)], shade(STONE_DK, 0.85));
  s += poly([up(D, BASE), up(E, BASE), E, D], shade(STONE, 0.92));
  s += poly([up(E, BASE), up(B, BASE), B, E], shade(STONE, 0.78));
  // The floor: boarded, because the shell above it turns to glass whenever
  // somebody is home and this is the room you then look into.
  s += poly([up(A, BASE), up(B, BASE), up(E, BASE), up(D, BASE)], "#a97c4e");
  for (let i = 1; i < p.w; i++) {
    const a = up(C(p.x + i, p.y), BASE), b = up(C(p.x + i, p.y + p.h), BASE);
    s += `<line class="plank" x1="${a[0]}" y1="${a[1]}" x2="${b[0]}" y2="${b[1]}"/>`;
  }

  // The two walls the camera can see, raised off the footing.
  const bD = up(D, BASE), bE = up(E, BASE), bB = up(B, BASE), bA = up(A, BASE);
  const faceS = tint(hue, 0.06), faceE = shade(hue, 0.8);
  let shell = "";
  shell += poly([bD, bE, up(bE, wh), up(bD, wh)], faceS, "ol2");
  shell += poly([bE, bB, up(bB, wh), up(bE, wh)], faceE, "ol2");

  // Corner timbers: four leaning posts down the arrises, which is most of what
  // sells a wall as built rather than extruded.
  shell += `<g class="beams">` +
    `<polygon points="${P([bD, up(bD, wh), [up(bD, wh)[0] + 5, up(bD, wh)[1] + 2.5], [bD[0] + 5, bD[1] + 2.5]])}" fill="${WOOD_DK}"/>` +
    `<polygon points="${P([bE, up(bE, wh), [up(bE, wh)[0] - 5, up(bE, wh)[1] + 2.5], [bE[0] - 5, bE[1] + 2.5]])}" fill="${WOOD_DK}"/>` +
    `<polygon points="${P([bE, up(bE, wh), [up(bE, wh)[0] + 5, up(bE, wh)[1] + 2.5], [bE[0] + 5, bE[1] + 2.5]])}" fill="${WOOD_DK}"/>` +
    `<polygon points="${P([bB, up(bB, wh), [up(bB, wh)[0] - 5, up(bB, wh)[1] + 2.5], [bB[0] - 5, bB[1] + 2.5]])}" fill="${WOOD_DK}"/>` +
    `</g>`;

  // Openings live in skewed groups so their rectangles lean with the wall.
  // The south wall runs from D along +x; the east wall from B along +y, which
  // on screen is local x going negative.
  const shop = p.kind === "shop";
  const south = { t: `translate(${bD[0]} ${bD[1]}) skewY(${SKEW})`, s: "", lit: "" };
  const east = { t: `translate(${bB[0]} ${bB[1]}) skewY(-${SKEW})`, s: "", lit: "" };
  const win = (o, lx, top, h2) => {
    o.s += `<rect class="ol" x="${lx - 9}" y="${-top}" width="18" height="${h2}" rx="2" fill="#4a3d2e"/>` +
      `<rect x="${lx - 9}" y="${-top}" width="18" height="${h2 / 2}" rx="2" fill="#5e4f3c"/>` +
      `<rect class="ol" x="${lx - 13}" y="${-top - 1}" width="4.5" height="${h2 + 2}" rx="1.5" fill="${WOOD_DK}"/>` +
      `<rect class="ol" x="${lx + 8.5}" y="${-top - 1}" width="4.5" height="${h2 + 2}" rx="1.5" fill="${WOOD_DK}"/>`;
    o.lit += `<rect class="winlit" x="${lx - 9}" y="${-top}" width="18" height="${h2}" rx="2"/>`;
  };
  const door = (o, lx) => {
    o.s += `<rect class="ol" x="${lx - 11}" y="-30" width="22" height="30" rx="3" fill="${WOOD}"/>` +
      `<rect x="${lx - 7}" y="-26" width="14" height="26" rx="2" fill="${WOOD_DK}"/>` +
      `<circle cx="${lx + 4}" cy="-14" r="1.8" fill="${GOLD}"/>` +
      `<path class="ol" d="M${lx - 15},-32 h30 l-4,-6 h-22 Z" fill="${GOLD}"/>`;
    o.lit += `<rect class="winlit" x="${lx - 7}" y="-26" width="14" height="26" rx="2" opacity=".55"/>`;
  };

  // Timber framing, drawn into the same skewed groups as the windows so it
  // leans with the wall: a stone plinth course at the foot, a top plate under
  // the eaves, a rail between them and a stud on every cell line. This is the
  // difference between a wall and a coloured rectangle.
  const frame = (o, span, dir) => {
    const w2 = span * HW * dir, x = Math.min(0, w2), abs = Math.abs(w2);
    o.s = `<rect x="${x}" y="-13" width="${abs}" height="13" fill="${STONE}"/>` +
      `<rect x="${x}" y="-13" width="${abs}" height="2.5" fill="${shade(STONE, 0.82)}"/>` +
      `<rect x="${x}" y="${-wh}" width="${abs}" height="7" fill="${WOOD_DK}"/>` +
      `<rect x="${x}" y="${-wh * 0.52}" width="${abs}" height="4.5" fill="${shade(WOOD, 0.9)}"/>` +
      Array.from({ length: span - 1 }, (_, k) =>
        `<rect x="${(k + 1) * HW * dir - 2.2}" y="${-wh}" width="4.4" height="${wh - 13}" fill="${shade(WOOD, 0.9)}"/>`
      ).join("") + o.s;
  };

  const dr = p.door;
  const onSouth = side === "south", onEast = side === "east";
  for (let i = 0; i < p.w; i++) {
    const lx = (i + 0.5) * HW;
    if (onSouth && dr.x === p.x + i) door(south, lx);
    else win(south, lx, shop ? 34 : 30, shop ? 22 : 16);
    if (shop && !(onSouth && dr.x === p.x + i)) win(south, lx, 52, 12);
  }
  for (let j = 0; j < p.h; j++) {
    const lx = -(j + 0.5) * HW;
    if (onEast && dr.y === p.y + j) door(east, lx);
    else win(east, lx, shop ? 34 : 30, shop ? 22 : 16);
  }
  frame(south, p.w, 1);
  frame(east, p.h, -1);
  shell += `<g transform="${south.t}">${south.s}</g><g transform="${east.t}">${east.s}</g>`;
  lit += `<g transform="${south.t}">${south.lit}</g><g transform="${east.t}">${east.lit}</g>`;

  shell += roof(p, wh, rh, roofPal, shop);

  // A shop hangs its name on a board over the street.
  if (shop && p.name) {
    const label = p.name.toUpperCase();
    const anchor = onEast ? bB : bD;
    const flip = onEast ? -1 : 1;
    const off = onEast ? -((dr.y - p.y) + 0.5) * HW : ((dr.x - p.x) + 0.5) * HW;
    const bw = label.length * 6.6 + 20;
    shell += `<g class="sign" transform="translate(${anchor[0]} ${anchor[1]}) skewY(${flip * SKEW})">` +
      `<rect class="ol" x="${off - bw / 2}" y="-58" width="${bw}" height="19" rx="4" fill="${WOOD}"/>` +
      `<rect x="${off - bw / 2 + 2.5}" y="-55.5" width="${bw - 5}" height="14" rx="3" fill="${shade(WOOD, 0.62)}"/>` +
      `<text x="${off}" y="-44">${esc(label)}</text></g>`;
  }

  s += `<g class="shell" id="shell-${esc(p.id)}">${shell}</g>`;
  if (p.name) s = `<g><title>${esc(p.name)}</title>${s}</g>`;
  return { solid: s, lit };
}

// roof lays a gabled, tiled roof over a footprint, ridge along the long axis,
// oversailing the walls on every side. Only the two faces the camera can see
// get detail; the far ones are painted first so a shallow pitch still reads.
function roof(p, wh, rh, pal, shop) {
  const o = EAVE;
  const x0 = p.x - o, x1 = p.x + p.w + o, y0 = p.y - o, y1 = p.y + p.h + o;
  const alongX = p.w >= p.h;
  const nw = up(C(x0, y0), wh), ne = up(C(x1, y0), wh);
  const sw = up(C(x0, y1), wh), se = up(C(x1, y1), wh);
  const [ra, rb] = alongX
    ? [up(C(x0, (y0 + y1) / 2), wh + rh), up(C(x1, (y0 + y1) / 2), wh + rh)]
    : [up(C((x0 + x1) / 2, y0), wh + rh), up(C((x0 + x1) / 2, y1), wh + rh)];
  // ra is the far end of the ridge, rb the near end; the lit slope is the one
  // between the ridge and the eave nearest the camera.
  const far = alongX ? [nw, ne] : [nw, sw];
  const near = alongX ? [sw, se] : [ne, se];
  const gableFar = alongX ? [nw, sw, ra] : [nw, ne, ra];
  const gableNear = alongX ? [ne, se, rb] : [sw, se, rb];

  let s = poly([far[0], far[1], rb, ra], pal[1], "ol2");
  s += poly(gableFar, shade(pal[1], 0.9), "ol2");
  s += poly(gableNear, tint(pal[0], 0.12), "ol2");
  // The near slope in tile courses: strips interpolated from ridge to eave.
  s += `<polygon class="ol2" points="${P([near[0], near[1], rb, ra])}" fill="${pal[0]}"/>`;
  const rows = 4;
  for (let i = 0; i < rows; i++) {
    const t0 = i / rows, t1 = (i + 1) / rows;
    const a0 = mid(ra, near[0], t0), b0 = mid(rb, near[1], t0);
    const a1 = mid(ra, near[0], t1), b1 = mid(rb, near[1], t1);
    if (i % 2) s += poly([a0, b0, b1, a1], shade(pal[0], 0.9), "");
    // Each course sits on the one below, so its lower edge gets a shadow and a
    // highlight — that pair of lines is what reads as tile rather than paint.
    s += `<line class="tileshade" x1="${a1[0]}" y1="${a1[1]}" x2="${b1[0]}" y2="${b1[1]}"/>`;
    s += `<line class="tilelip" x1="${a1[0]}" y1="${a1[1] + 1.6}" x2="${b1[0]}" y2="${b1[1] + 1.6}"/>`;
  }
  // The eave has a thickness. Without this board the roof ends in a knife edge
  // and the whole building goes flat, which is the one thing the style is not.
  const FAS = 6;
  const drop = (a, b) => poly([a, b, up(b, -FAS), up(a, -FAS)], shade(pal[0], 0.62), "ol2");
  s += drop(near[0], near[1]);
  // Barge boards down the two visible gable rakes, mitred into the fascia.
  const rake = (eaveEnd, ridgeEnd) =>
    poly([eaveEnd, ridgeEnd, up(ridgeEnd, -FAS * 0.7), up(eaveEnd, -FAS)], shade(pal[1], 0.62), "ol2");
  s += rake(near[0], ra) + rake(near[1], rb);
  // Ridge cap, and a gold finial with a pennant on the shops.
  s += `<line class="ridge" x1="${ra[0]}" y1="${ra[1]}" x2="${rb[0]}" y2="${rb[1]}"/>`;
  if (shop) {
    const tip = up(rb, 4);
    s += `<rect class="ol" x="${tip[0] - 1.6}" y="${tip[1] - 22}" width="3.2" height="22" fill="${WOOD_DK}"/>` +
      `<path class="ol" d="M${tip[0] + 1.6},${tip[1] - 22} l16,5 l-16,5 Z" fill="${GOLD}"/>` +
      `<circle class="ol" cx="${tip[0]}" cy="${tip[1] - 24}" r="2.6" fill="${GOLD}"/>`;
  }
  return `<g class="roof">${s}</g>`;
}

// ---- the people ----
//
// Legs, arms and a head, plus a costume from LOOKS. Everything hangs off a
// group whose origin is between the feet, so the animator only ever sets a
// transform.
function person(r, look) {
  const sk = look.skin, tn = look.tunic, tr = look.trim;
  let hat = "", prop = "";
  switch (look.hat) {
    case "chef":
      hat = `<path class="ol" d="M-7,-30 a7,7 0 0 1 14,0 z" fill="${look.hair}"/>` +
        `<ellipse class="ol" cx="0" cy="-38" rx="8" ry="5.5" fill="#fdf6ea"/>` +
        `<rect class="ol" x="-7" y="-36" width="14" height="5" rx="2" fill="#fdf6ea"/>`;
      break;
    case "scholar":
      hat = `<path class="ol" d="M-7,-30 a7,7 0 0 1 14,0 z" fill="${look.hair}"/>` +
        `<rect class="ol" x="-9" y="-37" width="18" height="4.5" rx="2" fill="#4a4136"/>` +
        `<rect class="ol" x="-6" y="-40" width="12" height="4" rx="1.5" fill="#5c5245"/>` +
        `<circle cx="-2.7" cy="-28.6" r="3" fill="none" stroke="#e9e2cf" stroke-width="1.1"/>` +
        `<circle cx="2.7" cy="-28.6" r="3" fill="none" stroke="#e9e2cf" stroke-width="1.1"/>`;
      break;
    case "scarf":
      hat = `<path class="ol" d="M-7.5,-29 a7.5,7.5 0 0 1 15,0 q-3,-4 -7.5,-4 t-7.5,4 z" fill="${tr}"/>` +
        `<path class="ol" d="M-7.5,-29 q-4,6 -1,11 l5,-3 q-3,-4 -1,-8 z" fill="${tr}"/>`;
      break;
    case "beard":
      hat = `<path class="ol" d="M-7,-30 a7,7 0 0 1 14,0 z" fill="${look.hair}"/>` +
        `<path class="ol" d="M-5,-25 q-0.5,8 5,9 q5.5,-1 5,-9 q-5,2.5 -10,0 z" fill="#d9cfc0"/>`;
      break;
    case "hood":
      hat = `<path class="ol" d="M-8,-24 q0,-12 8,-12 t8,12 q-8,-4 -16,0 z" fill="${tr}"/>`;
      break;
    default:
      hat = `<ellipse class="ol" cx="0" cy="-32" rx="11" ry="4" fill="#e3c377"/>` +
        `<ellipse class="ol" cx="0" cy="-35" rx="6" ry="4" fill="#edd08a"/>`;
  }
  switch (look.prop) {
    case "loaf": prop = `<ellipse class="ol" cx="9" cy="-16" rx="5" ry="3.4" fill="#d9a55c" transform="rotate(-20 9 -16)"/>`; break;
    case "book": prop = `<rect class="ol" x="5" y="-19" width="8" height="10" rx="1" fill="#b8503f"/><rect x="6.5" y="-18" width="5" height="8" fill="#f0e6d2"/>`; break;
    case "satchel": prop = `<rect class="ol" x="-13" y="-17" width="9" height="8" rx="2" fill="${WOOD}"/><path d="M-11,-17 l4,-7" stroke="${WOOD_DK}" stroke-width="2"/>`; break;
    case "tankard": prop = `<rect class="ol" x="6" y="-18" width="7" height="8" rx="1.5" fill="${STONE}"/><rect x="6.5" y="-19" width="6" height="2" fill="#f3e7c8"/>`; break;
    case "basket": prop = `<path class="ol" d="M5,-16 h10 l-1.5,8 h-7 z" fill="${WOOD}"/>`; break;
  }
  const b = BUILDS[look.build] || BUILDS.plain;
  // The garment: shoulders at -25, flaring to the hip and stopping at the hem.
  // A belt sits a third of the way down whatever length that turns out to be.
  const belt = (-25 + b.hem) / 2;
  const body =
    `<path class="ol" d="M${-b.sh},-25 h${b.sh * 2} L${b.hp},${b.hem} h${-b.hp * 2} z" fill="${tn}"/>` +
    `<path d="M-4,-25 h8 L${b.hp * 0.6},${b.hem} h${-b.hp * 1.2} z" fill="${tr}" opacity=".85"/>` +
    `<rect x="${-b.hp * 0.94}" y="${belt}" width="${b.hp * 1.88}" height="3" fill="${shade(tn, 0.7)}"/>` +
    // A shoulder wrap reads from behind and above, where a hat does not.
    (look.build === "wrap"
      ? `<path class="ol" d="M${-b.sh - 1.5},-24 q${b.sh + 1.5},4 ${b.sh * 2 + 3},0 l-2,7 q${-b.sh},3 ${-b.sh * 2 + 2},0 z" fill="${tr}"/>`
      : "");
  return `<g class="res" id="res-${esc(r.id)}">` +
    `<ellipse class="shadow" cx="0" cy="0" rx="8" ry="3.6"/>` +
    `<g class="flip">` +
    `<g class="limb" data-dir="-1" data-pivot="-11"><rect x="-4.8" y="-11" width="4" height="12" rx="1.6" fill="#4a3b2c"/></g>` +
    `<g class="limb" data-dir="1" data-pivot="-11"><rect x="0.8" y="-11" width="4" height="12" rx="1.6" fill="#5b4a37"/></g>` +
    `<g class="limb" data-dir="1" data-pivot="-24"><rect x="${-b.sh - 1.7}" y="-24" width="3.6" height="11" rx="1.7" fill="${shade(tn, 0.82)}"/></g>` +
    body +
    `<g class="limb" data-dir="-1" data-pivot="-24"><rect x="${b.sh - 1.9}" y="-24" width="3.6" height="11" rx="1.7" fill="${tn}"/></g>` +
    prop +
    `<circle class="ol" cx="0" cy="-30" r="6.6" fill="${sk}"/>` +
    hat +
    // Two eyes, painted last so no hat, hood or hairline can swallow them.
    // Without them a resident is a pawn; with them you can tell at a glance
    // which way they are facing.
    `<circle cx="-2.7" cy="-28.6" r="1.25" fill="#3b2a1d"/>` +
    `<circle cx="2.7" cy="-28.6" r="1.25" fill="#3b2a1d"/>` +
    `</g>` +
    `<g class="tag"><rect x="-22" y="-58" width="44" height="14" rx="7"/>` +
    `<text y="-47.5">${esc(r.name)}</text></g></g>`;
}

// ---- the standing town ----
//
// buildMap draws everything that does not move, once, into depth rows: one
// group per (x+y), painted back to front. Residents are appended to the row
// their cell falls in, so a walker passes behind a house on the far side and
// in front of it on the near side without anyone sorting anything per frame.

function buildMap(f) {
  names = new Map(f.residents.map((r) => [r.id, r.name]));
  placeNames = new Map(f.places.map((p) => [p.id, p.name]));
  colorOf = new Map(f.residents.map((r, i) => [r.id, LOOKS[i % LOOKS.length].tunic]));

  OX = f.height * HW + 24;
  const CW = (f.width + f.height) * HW + 48;
  const CH = OY + (f.width + f.height) * HH + 52;
  const DMAX = f.width + f.height;

  const placeAt = (x, y) => f.places.find((p) => x >= p.x && x < p.x + p.w && y >= p.y && y < p.y + p.h);
  const paved = (x, y) => {
    const p = placeAt(x, y);
    return !!p && (p.kind === "street" || p.kind === "plaza" || p.kind === "market");
  };
  insideOf = new Map();
  frontRow = new Map();
  const hue = new Map(), roofOf = new Map();
  let homes = 0, shops = 0;
  for (const p of f.places) {
    if (p.kind === "home" || p.kind === "shop") {
      const n = p.kind === "home" ? homes++ : shops++;
      hue.set(p.id, WALLPAL[n % WALLPAL.length]);
      roofOf.set(p.id, ROOFPAL[(p.kind === "home" ? n : n + 3) % ROOFPAL.length]);
      for (let y = p.y; y < p.y + p.h; y++)
        for (let x = p.x; x < p.x + p.w; x++) insideOf.set(x + "," + y, p.id);
      // The whole building is painted in one row — its front corner's — so a
      // resident standing in a back cell would be covered by their own floor.
      // Indoors, they join the building's row instead of their cell's.
      frontRow.set(p.id, p.x + p.w - 1 + (p.y + p.h - 1));
    }
  }

  let ground = "", lights = "", labels = "";
  const rows = new Map(); // depth → html, painted back to front
  const at = (d, s) => rows.set(d, (rows.get(d) || "") + s);

  // The island: a rounded earth plate with a rock edge, so the town sits on
  // something rather than floating on the page background.
  const rim = [C(0, 0), C(f.width, 0), C(f.width, f.height), C(0, f.height)];
  ground += `<polygon class="plinth-shadow" points="${P(rim.map((p) => [p[0], p[1] + 22]))}"/>`;
  ground += `<polygon class="plinth" points="${P(rim.map((p) => [p[0], p[1] + 16]))}"/>`;
  ground += `<polygon class="plinth-top" points="${P(rim)}"/>`;

  for (let y = 0; y < f.height; y++) {
    for (let x = 0; x < f.width; x++) {
      const p = placeAt(x, y), alt = (x + y) % 2, h = hash(x, y);
      const d = [C(x, y), C(x + 1, y), C(x + 1, y + 1), C(x, y + 1)];
      const kind = p ? p.kind : "grass";

      if (kind === "grass" || kind === "park") {
        const pal = kind === "park" ? PARKG : GRASS;
        ground += `<polygon points="${P(d)}" fill="${pal[alt]}"/>`;
        if (h % 5 === 0) {
          const [gx, gy] = C(x + 0.3 + (h % 7) / 14, y + 0.3 + ((h >>> 3) % 7) / 14);
          ground += `<ellipse cx="${gx}" cy="${gy}" rx="${8 + (h % 5)}" ry="${4 + (h % 3)}" fill="${GRASS_DK}" opacity=".45"/>`;
        }
        if (h % 3 === 0) {
          const [tx, ty] = C(x + 0.55, y + 0.6);
          ground += `<path class="tuft" d="M${tx - 3},${ty} q1.5,-4 3,0 M${tx + 2},${ty + 1} q1.5,-4 3,0"/>`;
        }
      } else if (kind === "street" || kind === "plaza" || kind === "market") {
        ground += `<polygon points="${P(d)}" fill="${PAVE[alt]}"/>`;
        // Cobbles: four rounded stones a cell, offset off the hash so no two
        // cells repeat and no grid shows through.
        for (let i = 0; i < 4; i++) {
          const fx = x + 0.2 + ((h >>> (i * 3)) % 11) / 18, fy = y + 0.2 + ((h >>> (i * 3 + 2)) % 11) / 18;
          const [cx2, cy2] = C(fx, fy);
          ground += `<ellipse cx="${cx2}" cy="${cy2}" rx="${5 + (h >>> i) % 3}" ry="${2.6 + (h >>> i) % 2}" fill="${COBBLE[(h >>> i) % 3]}"/>`;
        }
        // A curb wherever paving meets something that is not paving.
        if (!paved(x, y + 1)) ground += `<line class="curb" x1="${d[3][0]}" y1="${d[3][1]}" x2="${d[2][0]}" y2="${d[2][1]}"/>`;
        if (!paved(x + 1, y)) ground += `<line class="curb" x1="${d[1][0]}" y1="${d[1][1]}" x2="${d[2][0]}" y2="${d[2][1]}"/>`;
        if (!paved(x, y - 1)) ground += `<line class="curb" x1="${d[0][0]}" y1="${d[0][1]}" x2="${d[1][0]}" y2="${d[1][1]}"/>`;
        if (!paved(x - 1, y)) ground += `<line class="curb" x1="${d[0][0]}" y1="${d[0][1]}" x2="${d[3][0]}" y2="${d[3][1]}"/>`;
      } else {
        // Under a building. The floor proper is the top of the footing, nine
        // pixels up; all that shows here is the sliver behind the back walls,
        // which wants to read as more footing.
        ground += `<polygon points="${P(d)}" fill="${STONE_DK}"/>`;
      }

      // What stands on the cell.
      if (kind === "grass") {
        if (h % 9 === 0) at(x + y, tree(x, y, h % 3 === 0));
        else if (h % 13 === 4) at(x + y, bush(x, y, h));
        else if (h % 17 === 7) at(x + y, rock(x, y, h));
      } else if (kind === "park") {
        if (h % 4 === 0) at(x + y, tree(x, y, false));
        else if (h % 4 === 1) at(x + y, bush(x, y, h));
        else {
          for (let i = 0; i < 3; i++) {
            const fx = x + 0.25 + ((h >>> (i * 5)) % 11) / 20, fy = y + 0.25 + ((h >>> (i * 5 + 3)) % 11) / 20;
            const [px, py] = C(fx, fy);
            ground += `<circle cx="${px}" cy="${py}" r="2.4" fill="${FLOWERS[(h >>> i) % FLOWERS.length]}"/>`;
          }
        }
      } else if (kind === "street" && h % 11 === 0) {
        const t = torch(x + 0.16, y + 0.16);
        at(x + y, t.solid);
        lights += t.lit;
      }
    }
  }

  for (const p of f.places) {
    if (p.kind === "home" || p.kind === "shop") {
      const open = (x, y) => x >= 0 && y >= 0 && x < f.width && y < f.height && !insideOf.has(x + "," + y);
      const b = building(p, hue.get(p.id), roofOf.get(p.id), doorSide(p, open));
      at(p.x + p.w - 1 + p.y + p.h - 1, b.solid);
      lights += b.lit;
    } else if (p.kind === "plaza") {
      at(Math.floor(p.x + p.w / 2) + Math.floor(p.y + p.h / 2), fountain(p));
    } else if (p.kind === "market") {
      for (let y = p.y; y < p.y + p.h; y++)
        for (let x = p.x; x < p.x + p.w; x++)
          if ((x + y) % 2 === 0) at(x + y, stall(x, y));
      at(p.x + p.y, barrel(p.x + 0.2, p.y + 0.8));
    }
    if (p.name && p.kind !== "home" && p.kind !== "shop") {
      const [lx, ly] = C(p.x + p.w / 2, p.y);
      labels += `<text class="arealabel" x="${lx}" y="${ly - 8}">${esc(p.name.toUpperCase())}</text>`;
    }
  }

  let body = "";
  for (let d = 0; d <= DMAX; d++) body += `<g class="row" id="row-${d}">${rows.get(d) || ""}</g>`;

  mapEl.innerHTML =
    `<svg viewBox="0 0 ${CW} ${CH}" role="img" aria-label="map of ${esc(f.town)}">` +
    `<g>${ground}</g>${body}` +
    `<rect class="veil veil-warm" x="0" y="0" width="${CW}" height="${CH}"/>` +
    `<rect class="veil veil-cool" x="0" y="0" width="${CW}" height="${CH}"/>` +
    `<g class="lights">${lights}</g>` +
    `<g>${labels}</g>` +
    `</svg>`;

  const svg = mapEl.firstChild;
  walkers.clear();
  f.residents.forEach((r, i) => {
    svg.insertAdjacentHTML("beforeend", `<g class="walker" id="w-${esc(r.id)}">${person(r, LOOKS[i % LOOKS.length])}</g>`);
    walkers.set(r.id, { gx: r.x + 0.5, gy: r.y + 0.5, q: [], face: 1, phase: 0, row: -1 });
  });
}

// ---- the walk ----
//
// The simulation says where everyone is once a tick and hands over the cells
// they crossed to get there. This is what turns that into walking: each
// resident carries a queue of waypoints and moves along it at a human pace,
// only hurrying when the playback speed would otherwise leave them behind.

const walkers = new Map();
const NATURAL = 58 / CELLPX; // cells per second — an unhurried walk at this scale
// Seconds between ticks, for the catch-up. Measured as the clock runs; pressing
// play reseeds it from the selector, which knows the gap exactly. This opening
// value therefore only ever stands in for the first tick of a live feed, whose
// pace is the simulation's and is not knowable here until it has happened once.
let tickDur = 0.7;
let lastTickAt = 0, lastTick = -1, lastFrame = 0;

function feedWalkers(s, mode) {
  for (const [id, r] of s.residents) {
    const w = walkers.get(id);
    if (!w) continue;
    if (mode === "walk" && r.path && r.path.length) {
      for (const c of r.path) w.q.push([c[0] + 0.5, c[1] + 0.5]);
    } else if (mode !== "hold") {
      w.q.length = 0;
      w.gx = r.x + 0.5; w.gy = r.y + 0.5;
    }
  }
}

function stepWalkers(dt) {
  for (const w of walkers.values()) {
    if (!w.q.length) { w.moving = false; continue; }
    let left = 0, px = w.gx, py = w.gy;
    for (const c of w.q) { left += Math.abs(c[0] - px) + Math.abs(c[1] - py); px = c[0]; py = c[1]; }
    // Walk at a human pace, unless the clock is running faster than a human
    // walks — then move exactly fast enough to arrive with the next tick.
    let move = Math.max(NATURAL, left / Math.max(tickDur, 0.05)) * dt;
    w.moving = true;
    while (move > 0 && w.q.length) {
      const [tx, ty] = w.q[0];
      const dx = tx - w.gx, dy = ty - w.gy;
      const d = Math.hypot(dx, dy);
      if (d <= move || d < 1e-6) {
        w.gx = tx; w.gy = ty; w.q.shift(); move -= d; w.phase += d;
        if (Math.abs(dx) > 1e-6 || Math.abs(dy) > 1e-6) w.face = (dx - dy) >= 0 ? 1 : -1;
      } else {
        w.gx += dx / d * move; w.gy += dy / d * move; w.phase += move;
        w.face = (dx - dy) >= 0 ? 1 : -1;
        move = 0;
      }
    }
  }
}

function drawWalkers() {
  // Anyone sharing a cell would stand inside somebody else; fan them out. The
  // step of that fan is narrower than a name plate, though, so the plates alone
  // would still land on top of one another: they climb instead, one above the
  // next, each left over the head it names.
  const byCell = new Map();
  for (const [id, w] of walkers) {
    const k = Math.round(w.gx - 0.5) + "," + Math.round(w.gy - 0.5);
    if (!byCell.has(k)) byCell.set(k, []);
    byCell.get(k).push(id);
  }
  const open = new Set();
  for (const [, ids] of byCell) {
    ids.forEach((id, i) => {
      const w = walkers.get(id);
      if (!w.node) {
        w.node = document.getElementById("w-" + id);
        if (!w.node) return;
        w.flip = w.node.querySelector(".flip");
        w.tag = w.node.querySelector(".tag");
        w.limbs = [...w.node.querySelectorAll(".limb")];
      }
      const off = ids.length > 1 ? (i - (ids.length - 1) / 2) * 12 : 0;
      const tagY = ids.length > 1 ? -i * 16 : 0;
      const [px, py] = C(w.gx, w.gy);
      // Indoors the ground is the floorboards on top of the footing, so a
      // resident who steps through a door steps up onto it.
      const inside = insideOf.get(Math.round(w.gx - 0.5) + "," + Math.round(w.gy - 0.5));
      const lift = inside ? BASE : 0;
      // The walk cycle: limbs swing about the hip and shoulder, and the body
      // rises on each step. Standing still, everything squares up.
      const swing = w.moving ? Math.sin(w.phase * 7.2) * 22 : 0;
      const bob = w.moving ? Math.abs(Math.sin(w.phase * 7.2)) * 1.5 : 0;
      w.node.setAttribute("transform", `translate(${(px + off).toFixed(1)},${(py - bob - lift).toFixed(1)})`);
      w.flip.setAttribute("transform", `scale(${w.face},1)`);
      for (const l of w.limbs) {
        const s = (swing * Number(l.dataset.dir)).toFixed(1);
        l.setAttribute("transform", `rotate(${s},0,${l.dataset.pivot})`);
      }
      if (tagY !== w.tagY) { w.tag.setAttribute("transform", `translate(0,${tagY})`); w.tagY = tagY; }
      const d = inside ? frontRow.get(inside) : Math.round(w.gx - 0.5) + Math.round(w.gy - 0.5);
      if (d !== w.row) {
        const row = document.getElementById("row-" + d);
        if (row) { row.appendChild(w.node); w.row = d; }
      }
      if (inside) open.add(inside);
    });
  }
  // A roof you can see through, for as long as somebody is under it.
  for (const id of new Set(insideOf.values())) {
    const sh = document.getElementById("shell-" + id);
    if (sh) sh.classList.toggle("open", open.has(id));
  }
}

function frame(now) {
  const dt = lastFrame ? Math.min(0.05, (now - lastFrame) / 1000) : 0;
  lastFrame = now;
  stepWalkers(dt);
  drawWalkers();
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);

// ---- the speech bubble ----
//
// One bubble on screen at a time, over whoever spoke last: conversation is
// turn-taking, so the current speaker is the whole story, and a second bubble
// would only ever collide with the first in a doorway. It is authored as a
// child of the walker's own <g>, so position, depth row and building occlusion
// all arrive for free from drawWalkers — the renderer never learns that
// bubbles exist. It rides above .flip, not inside it, or the words would
// mirror whenever the speaker faced left.
let bubbleNode = null;
function showBubble(s) {
  if (bubbleNode) { bubbleNode.remove(); bubbleNode = null; }
  if (!s.bubble || s.bubbleAge >= 2) return;
  const holder = document.getElementById("w-" + s.bubble.who);
  if (!holder) return;
  // SVG text does not wrap; break on words, three lines, then an ellipsis.
  const lines = [""];
  for (const w of s.bubble.text.split(/\s+/)) {
    const cur = lines[lines.length - 1];
    if (cur && (cur + " " + w).length > 24) lines.push(w);
    else lines[lines.length - 1] = cur ? cur + " " + w : w;
  }
  if (lines.length > 3) { lines.length = 3; lines[2] += "…"; }
  const bw = Math.max(...lines.map((l) => l.length)) * 5.2 + 14;
  const bh = lines.length * 11 + 9;
  const top = -48 - bh;
  const text = lines.map((l, i) =>
    `<text x="0" y="${top + 13 + i * 11}" text-anchor="middle">${esc(l)}</text>`).join("");
  // The tail is an open path drawn after the rect: its fill patches over the
  // rect's bottom edge and its stroke covers only the two slanting sides, so
  // the seam needs no third shape to hide it.
  holder.insertAdjacentHTML("beforeend",
    `<g class="bubble">` +
    `<rect x="${(-bw / 2).toFixed(1)}" y="${top}" width="${bw.toFixed(1)}" height="${bh}" rx="7"/>` +
    `<path d="M-5,${top + bh - 1} L0,${top + bh + 5} L5,${top + bh - 1}"/>` +
    `${text}</g>`);
  bubbleNode = holder.lastElementChild;
}

// ---- the page ----

function render(s) {
  if (!s.founded) return;
  if (!mapEl.firstChild) buildMap(s.founded);

  clockEl.textContent = s.clock ? `day ${s.day} · ${s.clock}` : "day 1 · 07:00";

  // One tick on from the last render is a walk; anything else — a scrub, a
  // reload, the first frame — is a jump, and the walkers snap.
  if (s.tick === lastTick) {
    feedWalkers(s, "hold");
  } else if (s.tick === lastTick + 1) {
    const now = performance.now();
    if (lastTickAt) tickDur = Math.min(3, Math.max(0.08, (now - lastTickAt) / 1000));
    lastTickAt = now;
    feedWalkers(s, "walk");
  } else {
    lastTickAt = 0;
    feedWalkers(s, "snap");
  }
  lastTick = s.tick;

  // The sky: parse the hour straight off the clock and let CSS fade the town
  // through dusk into torchlight and lit windows.
  const hr = s.clock ? parseInt(s.clock, 10) : 7;
  const night = hr >= 21 || hr < 5, dusk = !night && (hr >= 18 || hr < 7);
  mapEl.classList.toggle("night", night);
  mapEl.classList.toggle("dusk", dusk);

  // Name over place, both hung off the one swatch that identifies the walker on
  // the map. Two lines rather than one long one: the name is what you scan the
  // list for, and the place is what you check once you have found it.
  roster.innerHTML = [...s.residents].map(([id, r]) => {
    const where = r.place ? placeNames.get(r.place) || r.place : "out on the street";
    return `<div class="townres"><span class="dot" style="background:${colorOf.get(id)}"></span>` +
      `<span class="townres-name">${esc(r.name)}</span>` +
      `<span class="where">${esc(where)}${r.activity ? ", " + esc(r.activity) : ""}</span></div>`;
  }).join("");

  // The clock sits in its own quiet column so the eye reads straight down the
  // sentences and only glances left for the time.
  const feedLine = {
    met: (f, place) => `<b>${esc(names.get(f.who[0]))}</b> ran into <b>${esc(names.get(f.who[1]))}</b> at ${esc(place)}`,
    arrive: (f, place) => `<b>${esc(names.get(f.who[0]))}</b> arrived at ${esc(place)}` +
      (f.activity ? ` — ${esc(f.activity)}` : ""),
    said: (f) => `<b>${esc(names.get(f.who[0]))}</b>, to ${esc(names.get(f.who[1]))}: ` +
      `<q>${esc(f.text)}</q>`,
    reflected: (f) => `<b>${esc(names.get(f.who[0]))}</b> slept on it — <q>${esc(f.text)}</q>`,
    // The fair's lines. An agent is a resident here, so the same name map
    // serves; the id is the fallback for a stream this page has never met.
    posted: (f) => `the office pinned <b>${esc(f.bounty)}</b> to the board — ${esc(f.generator)}, tier ${esc(f.tier)}, up to ${esc(f.max)} credits`,
    bid: (f) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> bid ${esc(f.price)} on <b>${esc(f.bounty)}</b>`,
    awarded: (f) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> won <b>${esc(f.bounty)}</b> at ${esc(f.price)} credits`,
    nobids: (f) => `the window on <b>${esc(f.bounty)}</b> closed with nobody at the board`,
    shelved: (f) => `<b>${esc(f.bounty)}</b> was shelved after ${esc(f.windows)} empty windows`,
    solved: (f) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> delivered <b>${esc(f.bounty)}</b> — paid ${esc(f.payout)} credits`,
    failed: (f) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> failed <b>${esc(f.bounty)}</b>, burning ${esc(f.burned)}`,
    voided: (f) => `<b>${esc(f.bounty)}</b> was voided${f.reason ? ` — ${esc(f.reason)}` : ""}`,
    bankrupt: (f) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> went bankrupt`,
    // The only line here for something an agent bought rather than won: it
    // paid to still be standing where it already stood.
    stayed: (f, place) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> paid ${esc(f.amount)} to stay at ` +
      `${esc(place)} — ${esc(f.ticks)} more ${f.ticks === 1 ? "tick" : "ticks"}`,
    // An agent writing to its own next step. Shown verbatim and never parsed:
    // the platform does not read these and neither does this page. It is here
    // because watching an agent's memory change is the only way to see it
    // learning — the decision it drives shows up as some other line entirely.
    memo: (f) => `<b>${esc(names.get(f.who[0]) || f.who[0])}</b> noted <q>${esc(f.text)}</q>`,
  };
  feed.innerHTML = s.feed.filter((f) => feedLine[f.kind]).map((f) => {
    const place = placeNames.get(f.place) || f.place;
    return `<div class="feedline ${f.kind}"><span class="when mono">${esc(f.clock)}</span>` +
      `<span>${feedLine[f.kind](f, place)}</span></div>`;
  }).reverse().join("");

  showBubble(s);
}

// ---- controls: the replay page's wiring, unchanged in spirit ----

// The played half of the track is a gradient stop rather than a UA-painted
// progress bar, because WebKit gives no way to style the two halves apart.
// Anything that moves the thumb, or moves the far end, has to repaint it.
function paintScrub() {
  const max = Number(scrub.max) || 0;
  scrub.style.setProperty("--p", `${max ? (cur / max) * 100 : 0}%`);
}

function setPos(i) {
  cur = Math.max(0, Math.min(EVENTS.length - 1, i));
  scrub.value = cur;
  paintScrub();
  pos.textContent = EVENTS.length ? `${cur + 1}/${EVENTS.length}` : "—";
  render(reduce(cur));
}

function setPlaying(on) {
  if (timer) { clearInterval(timer); timer = null; }
  if (on) {
    setFollow(false);
    // The gap between the ticks about to come is the selected one, so say so
    // rather than carrying a measurement over: the old one was taken at another
    // speed, or across the pause just ended, and either way the first tick
    // would be walked at the wrong pace — the one tick the viewer is watching
    // for. Clearing the mark keeps that first tick from measuring the gap too.
    tickDur = Number(speed.value) / 1000;
    lastTickAt = 0;
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
  // two ticks per press.
  if (e.target === scrub && (e.key === "ArrowRight" || e.key === "ArrowLeft")) return;
  if (e.key === "ArrowRight") { setPlaying(false); setFollow(false); setPos(cur + 1); }
  else if (e.key === "ArrowLeft") { setPlaying(false); setFollow(false); setPos(cur - 1); }
  else if (e.key === " " && e.target === document.body) { e.preventDefault(); setPlaying(!timer); }
});
if (livebtn) livebtn.addEventListener("click", () => setFollow(true));

scrub.max = Math.max(0, EVENTS.length - 1);
setPos(Math.max(0, LIVE ? EVENTS.length - 1 : 0));

// ---- the live feed ----

if (LIVE) {
  const last = EVENTS.length ? EVENTS[EVENTS.length - 1].seq : 0;
  const es = new EventSource(`/events?after=${last}`);
  es.onmessage = (m) => {
    EVENTS.push(JSON.parse(m.data));
    scrub.max = EVENTS.length - 1;
    if (follow) setPos(EVENTS.length - 1);
    else { paintScrub(); pos.textContent = `${cur + 1}/${EVENTS.length}`; } // the end of the track moved
  };
  // The file was truncated under us: a new run took the path. Start over.
  es.addEventListener("reset", () => location.reload());
}
