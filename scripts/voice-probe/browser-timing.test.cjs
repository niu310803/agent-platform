const { test } = require("node:test");
const assert = require("node:assert/strict");
const { readFileSync } = require("node:fs");
const { runInNewContext } = require("node:vm");
const script = readFileSync(`${__dirname}/browser-timing.js`, "utf8");
function harness() {
  let listener;
  const window = {
    addEventListener(_type, receive) { listener = receive; },
    removeEventListener(_type, receive) { if (listener === receive) listener = undefined; },
  };
  const performance = { timeOrigin: 1000, now: () => 100 };
  const probe = runInNewContext(script, { window, performance });
  return { window, probe, send: detail => listener?.({ detail }) };
}
const start = { category: "voice.timing", clock: "client_performance", timeOrigin: 1000, sessionId: "session", turn: 1, stage: "listenStart", atMs: 1 };
test("captures only metadata, validates annotations and removes all temporary state", () => {
  const { window, probe, send } = harness();
  send({ ...start, text: "private", token: "secret" });
  send({ ...start, timeOrigin: 2000 });
  probe.speechEnd({ sessionId: "session", turn: 1, atMs: 50 });
  assert.throws(() => probe.speechEnd({ sessionId: "session", turn: 1, atMs: 51 }), /Duplicate/);
  assert.throws(() => probe.speechEnd({ sessionId: "other", turn: 1, atMs: 50 }), /Invalid/);
  const rows = probe.jsonl().trim().split("\n").map(JSON.parse);
  assert.equal(rows.length, 2);
  assert.equal(rows[1].source, "annotation");
  assert.doesNotMatch(probe.jsonl(), /private|secret/);
  probe.stop(); send(start); assert.equal(probe.jsonl().trim().split("\n").length, 2);
  probe.dispose(); assert.equal(window.voiceTimingProbe, undefined); assert.equal(probe.jsonl(), "\n");
});
test("marks overflow instead of silently calculating a truncated sample set", () => {
  const { probe, send } = harness();
  for (let i = 0; i < 10001; i++) send(start);
  const rows = probe.jsonl().trim().split("\n");
  assert.equal(rows.length, 10001);
  assert.equal(JSON.parse(rows.at(-1)).stage, "probeOverflow");
  probe.dispose();
});
