// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT
//
// The battle page's script (the browser trial's battle.js, cut down): load
// the battle from the api component, render both sides, vote.
"use strict";

const $ = (id) => document.getElementById(id);
const battleId = location.pathname.split("/").filter(Boolean).pop();

function showError(msg) {
  $("error").textContent = msg;
  $("error").hidden = !msg;
}

function render(b) {
  $("name-a").textContent = b.nameA;
  $("name-b").textContent = b.nameB;
  $("score-a").textContent = String(b.votesA);
  $("score-b").textContent = String(b.votesB);
}

async function call(method, body) {
  const url = "/api/battles/" + encodeURIComponent(battleId) + (body ? "/votes" : "");
  const opts = { method, headers: {} };
  if (body) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(url, opts);
  if (!res.ok) throw new Error(res.status + " " + res.statusText);
  render(await res.json());
}

function vote(side) {
  showError("");
  call("POST", { side }).catch((e) => showError(e.message));
}

$("vote-a").addEventListener("click", () => vote("a"));
$("vote-b").addEventListener("click", () => vote("b"));
call("GET").catch((e) => showError(e.message));
