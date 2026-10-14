function render(s) {
  const state = document.getElementById("state");
  const tabs = document.getElementById("tabs");
  const err = document.getElementById("err");
  if (!s) {
    state.textContent = "unknown";
    return;
  }
  state.textContent = s.connected ? "connected" : "disconnected";
  state.className = s.connected ? "ok" : "bad";
  tabs.textContent = String(s.attachedTabs ?? 0);
  err.textContent = s.lastError ? `Last error: ${s.lastError}` : "";
}

chrome.runtime.sendMessage({ type: "get_status" }, (s) => {
  render(s);
});

document.getElementById("reconnect").addEventListener("click", () => {
  chrome.runtime.sendMessage({ type: "reconnect" }, () => {
    setTimeout(() => chrome.runtime.sendMessage({ type: "get_status" }, render), 300);
  });
});
