// xdev Browser — MV3 service worker.
// Speaks length-prefixed JSON with the native host (com.freepeak.xdev_browser)
// and drives the user's real tabs through chrome.debugger (CDP).

const NATIVE_HOST = "com.freepeak.xdev_browser";
const RECONNECT_MS = 2000;
const MAX_RECONNECT_MS = 30000;

/** @type {chrome.runtime.Port|null} */
let nativePort = null;
let reconnectDelay = RECONNECT_MS;
let reconnectTimer = null;
/** tabId -> attached */
const attached = new Map();
/** pending CDP replies: `${tabId}:${cdpId}` -> true (extension just forwards) */
let status = {
  connected: false,
  lastError: "",
  attachedTabs: 0,
};

function setStatus(patch) {
  Object.assign(status, patch);
  status.attachedTabs = attached.size;
  chrome.storage.session.set({ xdevStatus: status }).catch(() => {});
  chrome.action.setBadgeText({ text: status.connected ? "on" : "" });
  chrome.action.setBadgeBackgroundColor({ color: status.connected ? "#2f6fed" : "#666" });
}

function postNative(msg) {
  if (!nativePort) return false;
  try {
    nativePort.postMessage(msg);
    return true;
  } catch (err) {
    setStatus({ connected: false, lastError: String(err) });
    nativePort = null;
    scheduleReconnect();
    return false;
  }
}

function connectNative() {
  if (nativePort) return;
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  try {
    nativePort = chrome.runtime.connectNative(NATIVE_HOST);
  } catch (err) {
    setStatus({ connected: false, lastError: `connectNative: ${err}` });
    scheduleReconnect();
    return;
  }
  nativePort.onMessage.addListener(onNativeMessage);
  nativePort.onDisconnect.addListener(() => {
    const err = chrome.runtime.lastError?.message || "disconnected";
    nativePort = null;
    setStatus({ connected: false, lastError: err });
    scheduleReconnect();
  });
  setStatus({ connected: true, lastError: "" });
  reconnectDelay = RECONNECT_MS;
  postNative({ type: "hello", extensionId: chrome.runtime.id, version: chrome.runtime.getManifest().version });
}

function scheduleReconnect() {
  if (reconnectTimer) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connectNative();
    reconnectDelay = Math.min(reconnectDelay * 1.5, MAX_RECONNECT_MS);
  }, reconnectDelay);
}

async function onNativeMessage(msg) {
  if (!msg || typeof msg !== "object") return;
  switch (msg.type) {
    case "ping":
      postNative({ type: "pong", t: Date.now() });
      return;
    case "list_targets":
      await replyListTargets(msg.id);
      return;
    case "cdp":
      await handleCDP(msg);
      return;
    case "attach":
      await ensureAttached(msg.tabId);
      postNative({ type: "attach_result", id: msg.id, ok: attached.has(msg.tabId) });
      return;
    case "detach":
      await detachTab(msg.tabId);
      postNative({ type: "detach_result", id: msg.id, ok: true });
      return;
    default:
      postNative({ type: "error", id: msg.id, error: `unknown type ${msg.type}` });
  }
}

async function listPageTargets() {
  const tabs = await chrome.tabs.query({});
  const out = [];
  for (const tab of tabs) {
    if (tab.id == null) continue;
    const url = tab.url || "";
    if (url.startsWith("chrome://") || url.startsWith("chrome-extension://") || url.startsWith("devtools://") || url.startsWith("edge://")) {
      continue;
    }
    out.push({
      id: String(tab.id),
      type: "page",
      title: tab.title || "",
      url,
      favIconUrl: tab.favIconUrl || "",
      attached: attached.has(tab.id),
    });
  }
  return out;
}

async function replyListTargets(id) {
  try {
    const targets = await listPageTargets();
    postNative({ type: "list_targets_result", id, targets });
  } catch (err) {
    postNative({ type: "list_targets_result", id, error: String(err), targets: [] });
  }
}

async function ensureAttached(tabId) {
  const id = Number(tabId);
  if (!Number.isFinite(id)) throw new Error(`invalid tabId ${tabId}`);
  if (attached.has(id)) return id;
  await chrome.debugger.attach({ tabId: id }, "1.3");
  attached.set(id, true);
  setStatus({});
  chrome.debugger.onDetach.addListener(function onDetach(source, reason) {
    if (source.tabId === id) {
      attached.delete(id);
      setStatus({});
      postNative({ type: "detached", tabId: id, reason: reason || "" });
    }
  });
  return id;
}

async function detachTab(tabId) {
  const id = Number(tabId);
  if (!attached.has(id)) return;
  try {
    await chrome.debugger.detach({ tabId: id });
  } catch (_) {
    /* already gone */
  }
  attached.delete(id);
  setStatus({});
}

async function handleCDP(msg) {
  const { id, tabId, method, params } = msg;
  try {
    const tid = await ensureAttached(tabId);
    const result = await chrome.debugger.sendCommand({ tabId: tid }, method, params || {});
    postNative({ type: "cdp_result", id, result: result ?? {} });
  } catch (err) {
    const message = err && err.message ? err.message : String(err);
    postNative({
      type: "cdp_result",
      id,
      error: { code: -32000, message },
    });
  }
}

chrome.runtime.onInstalled.addListener(() => connectNative());
chrome.runtime.onStartup.addListener(() => connectNative());
chrome.alarms.create("xdev-keepalive", { periodInMinutes: 1 });
chrome.alarms.onAlarm.addListener((a) => {
  if (a.name === "xdev-keepalive") {
    if (!nativePort) connectNative();
    else postNative({ type: "ping", t: Date.now() });
  }
});

chrome.runtime.onMessage.addListener((msg, _sender, sendResponse) => {
  if (msg && msg.type === "get_status") {
    sendResponse(status);
    return true;
  }
  if (msg && msg.type === "reconnect") {
    if (nativePort) {
      try { nativePort.disconnect(); } catch (_) {}
      nativePort = null;
    }
    connectNative();
    sendResponse({ ok: true });
    return true;
  }
  return false;
});

connectNative();
