// The gawk-relay console (docs/76 §7.1). Plain script, no build step and no
// network beyond this origin: the console must work at a party with no
// internet. Every value from the relay is written with textContent, never
// as HTML.
"use strict";

const $ = (id) => document.getElementById(id);

const HEADERS = {
  linked: (s) => "Linked to " + hostOf(s.homeOrigin),
  standalone: () => "Standalone",
  offline: () => "Offline: the party keeps working, outside viewers are paused",
  waiting: () => "Waiting to pair",
  revoked: () => "Revoked",
  repair: () => "Re-pair needed",
};

const OUTSIDE = {
  forwarded: (r) => "Forwarded" + (r.outsideMbps ? " · " + r.outsideMbps.toFixed(1) + " Mbps" : ""),
  "not-requested": () => "Not requested",
  "party-only": () => "Party-only (budget full)",
};

let current = null;
let lanChoice = null; // what the host picked and has not applied yet

function hostOf(origin) {
  try {
    return new URL(origin).host;
  } catch {
    return origin || "home";
  }
}

function joinHint(s) {
  switch (s.join.tier) {
    case "named":
      return "Scan or open this link: no app needed.";
    case "linked-pinned":
      return "Open " + hostOf(s.join.url) + ". Devices here switch to this relay by themselves.";
    case "standalone":
      return "Scan or open this link on a device on this network.";
    default:
      return "No LAN address yet: connect this machine to the party network.";
  }
}

function budgetText(b) {
  if (!b || (!b.Mbps && !b.Streams)) return "no limit";
  let t = b.Mbps ? b.Mbps + " Mbps" : "";
  if (b.Streams) t += (t ? " · " : "") + b.Streams + " streams";
  return t;
}

function render(s) {
  current = s;
  $("name").textContent = s.displayName ? " · " + s.displayName : "";
  $("header-state").textContent = (HEADERS[s.header] || (() => s.header))(s);
  $("header-state").dataset.state = s.header;
  $("notice").hidden = !s.notice;
  $("notice").textContent = s.notice || "";

  // Join
  $("join-hint").textContent = joinHint(s);
  const url = s.join.url;
  $("join-url").textContent = url || "";
  if (url) $("join-url").href = url;
  else $("join-url").removeAttribute("href");
  $("copy-join").disabled = !url;
  $("projector").disabled = !url;
  const qrSrc = "qr.svg?u=" + encodeURIComponent(url);
  if (url && $("qr").dataset.u !== url) {
    $("qr").src = qrSrc;
    $("qr-big").src = qrSrc;
    $("qr").dataset.u = url;
  }
  $("qr").hidden = !url;
  $("projector-url").textContent = url || "";
  $("server-link-row").hidden = !(s.serverLink && s.join.tier === "standalone");
  $("server-link").textContent = s.serverLink || "";

  // Live
  const body = $("live").tBodies[0];
  body.replaceChildren();
  for (const r of s.live) {
    const tr = body.insertRow();
    tr.insertCell().textContent = r.label ? r.label + " (" + r.code + ")" : r.code;
    tr.insertCell().textContent = String(r.localViewers);
    tr.insertCell().textContent = s.header === "linked" ? String(r.outsideViewers) : "—";
    tr.insertCell().textContent = r.outside ? (OUTSIDE[r.outside] || (() => r.outside))(r) : "—";
    tr.insertCell().textContent = r.wall === "on" ? "On the wall" : r.wall === "hidden" ? "Hidden" : "—";
  }
  $("live").hidden = s.live.length === 0;
  $("live-empty").hidden = s.live.length !== 0;

  // Party internet
  if (s.header === "linked" || s.header === "offline") {
    const ceil = s.uplinkCeiling && s.uplinkCeiling.Mbps ? " / " + s.uplinkCeiling.Mbps : "";
    const streams = s.uplink && s.uplink.Streams ? " · " + s.uplinkStreams + " of " + s.uplink.Streams + " streams" : "";
    $("uplink-line").textContent = "Up " + Math.round(s.uplinkMbps) + ceil + " Mbps" + streams;
    $("downlink-line").textContent = "Down " + s.downlinkStreams + (s.downlinkStreams === 1 ? " stream" : " streams") +
      " · budget " + budgetText(s.downlink);
  } else {
    $("uplink-line").textContent = "Not linked: nothing leaves the party network. Uplink budget " + budgetText(s.uplink) + ".";
    $("downlink-line").textContent = "Downlink budget " + budgetText(s.downlink) + ".";
  }

  // Health
  const ul = $("health");
  ul.replaceChildren();
  for (const h of s.health || []) {
    const li = document.createElement("li");
    li.className = h.ok ? "ok" : "bad";
    li.textContent = h.detail;
    ul.appendChild(li);
  }
  $("firewall-row").hidden = !s.firewall || (s.health || []).some((h) => h.key === "firewall" && h.ok);

  const sel = $("lan");
  const options = [["", "Detect automatically"]];
  for (const a of s.lanCandidates) options.push([a, a]);
  if (s.lanAddr && !s.lanCandidates.includes(s.lanAddr)) options.push([s.lanAddr, s.lanAddr]);
  const want = lanChoice ?? "";
  if (sel.options.length !== options.length || [...sel.options].some((o, i) => o.value !== options[i][0])) {
    sel.replaceChildren(...options.map(([v, t]) => new Option(t, v)));
  }
  sel.value = want;

  // Pairing: offered until linked.
  $("pairing").hidden = s.header === "linked" || s.header === "offline";
  $("standalone").hidden = s.header !== "waiting";

  $("version").textContent = "gawk-relay " + (s.version || "") +
    (s.updateAvailable ? " · update available: " + s.updateAvailable : "");
}

function showError(msg) {
  $("error").hidden = !msg;
  $("error").textContent = msg || "";
}

async function refresh() {
  try {
    const rsp = await fetch("api/state", { cache: "no-store" });
    if (!rsp.ok) throw new Error("the relay answered " + rsp.status);
    render(await rsp.json());
    if ($("error").dataset.kind === "poll") showError("");
  } catch (e) {
    $("error").dataset.kind = "poll";
    showError("Cannot reach gawk-relay: is it still running? (" + e.message + ")");
  }
}

async function post(path, body) {
  $("error").dataset.kind = "action";
  showError("");
  const rsp = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (!rsp.ok) {
    let msg = "the relay answered " + rsp.status;
    try {
      msg = (await rsp.json()).Error || msg;
    } catch {}
    showError(msg);
    return false;
  }
  await refresh();
  return true;
}

function copy(text) {
  if (navigator.clipboard) navigator.clipboard.writeText(text).catch(() => {});
}

function intOf(id) {
  const v = parseInt($(id).value, 10);
  return Number.isFinite(v) && v > 0 ? v : 0;
}

function openProjector() {
  const v = $("projector-view");
  v.hidden = false;
  if (v.requestFullscreen) v.requestFullscreen().catch(() => {});
}

function closeProjector() {
  $("projector-view").hidden = true;
  if (document.fullscreenElement) document.exitFullscreen().catch(() => {});
}

document.addEventListener("DOMContentLoaded", () => {
  $("copy-join").addEventListener("click", () => current && copy(current.join.url));
  $("copy-server").addEventListener("click", () => current && copy(current.serverLink));
  $("projector").addEventListener("click", openProjector);
  $("projector-view").addEventListener("click", closeProjector);
  document.addEventListener("keydown", (e) => e.key === "Escape" && closeProjector());
  document.addEventListener("fullscreenchange", () => {
    if (!document.fullscreenElement) $("projector-view").hidden = true;
  });

  $("pair-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (await post("api/pair", { pairing: $("pair-input").value })) $("pair-input").value = "";
  });
  $("standalone").addEventListener("click", () => post("api/standalone"));
  $("lan").addEventListener("change", () => (lanChoice = $("lan").value));
  $("lan-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    if (await post("api/lan", { addr: $("lan").value })) lanChoice = null;
  });
  $("uplink-form").addEventListener("submit", (e) => {
    e.preventDefault();
    post("api/uplink", { mbps: intOf("uplink-mbps"), streams: intOf("uplink-streams") });
  });
  $("downlink-form").addEventListener("submit", (e) => {
    e.preventDefault();
    post("api/downlink", { mbps: intOf("downlink-mbps"), streams: intOf("downlink-streams") });
  });
  $("firewall").addEventListener("click", () => post("api/firewall"));

  refresh();
  setInterval(refresh, 2000);
});
