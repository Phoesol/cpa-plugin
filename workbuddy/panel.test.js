const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function fakeElement() {
  const queries = new Map();
  const element = {
    hidden: false,
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    files: [],
    style: {},
    dataset: {},
    children: [],
    parentNode: null,
    classList: {
      values: new Set(),
      add(value) { this.values.add(value); },
      remove(value) { this.values.delete(value); },
      contains(value) { return this.values.has(value); },
    },
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    querySelector(selector) {
      if (!queries.has(selector)) queries.set(selector, fakeElement());
      return queries.get(selector);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    focus() {},
    remove() {
      if (!this.parentNode) return;
      const index = this.parentNode.children.indexOf(this);
      if (index >= 0) this.parentNode.children.splice(index, 1);
      this.parentNode = null;
    },
  };
  Object.defineProperty(element, "firstChild", {
    get() { return element.children[0] || null; },
  });
  return element;
}

function loadPanel(overrides = {}) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  const source = scripts.at(-1)[1].replace(/\nloadInitial\(\);\s*$/, "");
  const elements = new Map();
  const storage = new Map();
  const document = {
    documentElement: fakeElement(),
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, fakeElement());
      return elements.get(id);
    },
    createElement() { return fakeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  const sessionStorage = {
    getItem(key) { return storage.has(key) ? storage.get(key) : null; },
    setItem(key, value) { storage.set(key, String(value)); },
    removeItem(key) { storage.delete(key); },
  };
  const context = {
    console,
    document,
    sessionStorage,
    localStorage: overrides.localStorage || { getItem() { return null; }, setItem() { throw new Error("localStorage write"); } },
    location: overrides.location || { href: "http://localhost/panel", search: "", pathname: "/panel", hash: "", host: "localhost" },
    history: overrides.history || { replaceState() {} },
    navigator: { userAgent: "node-test" },
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    btoa(value) { return Buffer.from(value, "binary").toString("base64"); },
    atob(value) { return Buffer.from(value, "base64").toString("binary"); },
    requestAnimationFrame(fn) { fn(); },
    setTimeout(fn) { fn(); return 1; },
    clearTimeout() {},
    fetch: overrides.fetch || (async () => { throw new Error("unexpected fetch"); }),
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  return { context, document, elements, storage };
}

test("model status banner hides ready and persists non-ready states", () => {
  const { context, elements } = loadPanel();
  const statuses = [
    { state: "stale", message: "模型目录刷新失败，正在使用上次有效缓存" },
    { state: "failed", message: "模型目录不可用" },
    { state: "loading", message: "模型目录正在初始化" },
    { state: "not_started", message: "模型目录尚未初始化" },
  ];
  for (const status of statuses) {
    context.updateModelStatus(status);
    const banner = elements.get("modelStatus");
    assert.equal(banner.hidden, false);
    assert.equal(banner.className, "model-status " + status.state);
    assert.equal(banner.textContent, status.message);
  }
  context.updateModelStatus({ state: "ready", message: "ignored" });
  const banner = elements.get("modelStatus");
  assert.equal(banner.hidden, true);
  assert.equal(banner.className, "model-status");
  assert.equal(banner.textContent, "");
});

test("load updates model status before dashboard error", async () => {
  const { context, elements, storage } = loadPanel();
  storage.set("workbuddy-mgmt-key", "test-key");
  let toastCalls = 0;
  context.toast = () => { toastCalls += 1; };
  context.api = async () => ({
    model_status: { state: "failed", message: "模型目录不可用" },
    error: "dashboard unavailable",
  });
  await context.load(false);
  assert.equal(elements.get("modelStatus").textContent, "模型目录不可用");
  assert.equal(toastCalls, 0);
});

test("model status message is rendered as text", () => {
  const { context, elements } = loadPanel();
  const message = "<img src=x onerror=1>";
  context.updateModelStatus({ state: "failed", message });
  const banner = elements.get("modelStatus");
  assert.equal(banner.textContent, message);
  assert.equal(banner.innerHTML, "");
});
