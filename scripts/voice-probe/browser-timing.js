// Paste the printed -browser-script into the intended, already-running voice
// page before recording. No microphone access, upload, or persistent storage.
(() => {
  if (window.voiceTimingProbe) throw new Error("Dispose the existing voiceTimingProbe first");
  const records = [];
  let overflowed = false;
  const keys = ["category", "clock", "timeOrigin", "sessionId", "turn", "stage", "atMs", "runId", "awaitingId", "speechId", "purpose", "outcome", "clockSource", "build", "host", "mode", "codec"];
  const receive = event => {
    if (records.length >= 10000) { overflowed = true; stop(); return; }
    const point = event.detail;
    if (!point || point.clock !== "client_performance" || point.timeOrigin !== performance.timeOrigin) return;
    const record = {};
    for (const key of keys) if (["string", "number"].includes(typeof point[key])) record[key] = point[key];
    records.push(record);
  };
  const stop = () => window.removeEventListener("agent:voice-timing", receive);
  const probe = {
    // atMs must be the annotated last speech sample on THIS page's performance
    // clock. Do not pass ASR final receipt, file duration, or Date.now().
    speechEnd({ sessionId, turn, atMs }) {
      const start = records.find(p => p.stage === "listenStart" && p.sessionId === sessionId && p.turn === turn);
      if (!start || !Number.isFinite(atMs) || atMs < start.atMs || atMs > performance.now()) throw new Error("Invalid speech-end annotation");
      if (records.some(p => p.stage === "speechEnd" && p.sessionId === sessionId && p.turn === turn)) throw new Error("Duplicate speech-end annotation");
      if (records.length >= 10000) { overflowed = true; stop(); throw new Error("Timing buffer overflow"); }
      records.push({ ...start, stage: "speechEnd", atMs, source: "annotation" });
    },
    jsonl() {
      const output = overflowed ? [...records, { category: "voice.timing", stage: "probeOverflow" }] : records;
      return output.map(p => JSON.stringify(p)).join("\n") + "\n";
    },
    stop,
    dispose() { stop(); records.length = 0; if (window.voiceTimingProbe === probe) delete window.voiceTimingProbe; },
  };
  window.addEventListener("agent:voice-timing", receive);
  window.voiceTimingProbe = probe;
  return probe;
})();
