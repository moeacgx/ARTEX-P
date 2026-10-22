import { catalogAPI, downloadCatalogItem } from "./catalog-api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const originalFetch = globalThis.fetch;
const originalStorage = globalThis.localStorage;
const originalDocument = globalThis.document;
const originalWindow = globalThis.window;
const originalCreateObjectURL = URL.createObjectURL;
const originalRevokeObjectURL = URL.revokeObjectURL;
const originalSetTimeout = globalThis.setTimeout;
test.afterEach(() => {
  globalThis.fetch = originalFetch;
  globalThis.localStorage = originalStorage;
  globalThis.document = originalDocument;
  globalThis.window = originalWindow;
  URL.createObjectURL = originalCreateObjectURL;
  URL.revokeObjectURL = originalRevokeObjectURL;
  globalThis.setTimeout = originalSetTimeout;
});

test("安装请求绑定预览时的提交、来源指纹与本地指纹", async () => {
  globalThis.fetch = async (url, init) => {
    assert.equal(url, "/api/catalog/install");
    assert.equal(init.method, "POST");
    assert.deepEqual(JSON.parse(init.body), {
      kind: "agent",
      id: "research_helper",
      commit: "a".repeat(40),
      expected_local_hash: "local-before-preview",
      expected_source_hash: "source-before-preview",
    });
    return Response.json({ ok: true });
  };
  await catalogAPI.install({
    item: { kind: "agent", id: "research_helper" },
    commit: "a".repeat(40),
    local_hash: "local-before-preview",
    source_hash: "source-before-preview",
  });
});

test("服务端拒绝本地冲突时保留原始错误，不重试覆盖", async () => {
  let calls = 0;
  globalThis.fetch = async () => {
    calls++;
    return Response.json({ error: "本地内容已改变，请重新预览" }, { status: 409 });
  };
  await assert.rejects(catalogAPI.install({ item: { kind: "skill", id: "sample" } }), /本地内容已改变/);
  assert.equal(calls, 1);
});

test("回滚请求包含本地指纹，避免覆盖预览后修改", async () => {
  globalThis.fetch = async (url, init) => {
    assert.equal(url, "/api/catalog/rollback");
    assert.deepEqual(JSON.parse(init.body), { kind: "tool", id: "sample", expected_local_hash: "current-hash" });
    return Response.json({ ok: true });
  };
  await catalogAPI.rollback({ kind: "tool", id: "sample", local_hash: "current-hash" });
});

test("ZIP 下载携带登录凭据并拒绝错误内容类型", async () => {
  globalThis.localStorage = { getItem: () => "test-token" };
  globalThis.fetch = async (url, init) => {
    assert.equal(url, "/api/catalog/download?kind=skill&id=sample&commit=preview-sha");
    assert.equal(init.headers.Authorization, "Bearer test-token");
    return Response.json({ ok: true });
  };
  await assert.rejects(downloadCatalogItem({ kind: "skill", id: "sample" }, "preview-sha"), /ZIP/);
});

test("ZIP 下载未授权时清除过期凭据并跳转登录", async () => {
  let removed;
  globalThis.localStorage = {
    getItem: () => "expired",
    removeItem: (key) => {
      removed = key;
    },
  };
  globalThis.document = { cookie: "" };
  globalThis.window = { location: { href: "" } };
  globalThis.fetch = async () => new Response(null, { status: 401 });
  await assert.rejects(downloadCatalogItem({ kind: "skill", id: "sample" }, "sha"), /未授权/);
  assert.equal(removed, "artex_token");
  assert.equal(globalThis.window.location.href, "/login");
});

test("成功下载触发 ZIP 文件保存并释放临时链接", async () => {
  const events = [];
  let release;
  const anchor = { click: () => events.push("click"), remove: () => events.push("remove") };
  globalThis.localStorage = { getItem: () => null };
  globalThis.document = { createElement: () => anchor, body: { appendChild: () => events.push("append") } };
  globalThis.fetch = async () => new Response("PK-test", { headers: { "Content-Type": "application/zip" } });
  URL.createObjectURL = (blob) => {
    assert.ok(blob instanceof Blob);
    return "blob:test-archive";
  };
  URL.revokeObjectURL = (url) => events.push(`release:${url}`);
  globalThis.setTimeout = (fn, delay) => {
    assert.equal(delay, 1000);
    release = fn;
  };
  await downloadCatalogItem({ kind: "skill", id: "sample", version: "1.0.0" }, "sha");
  assert.equal(anchor.download, "sample-1.0.0.zip");
  assert.equal(anchor.href, "blob:test-archive");
  assert.deepEqual(events, ["append", "click", "remove"]);
  release();
  assert.equal(events.at(-1), "release:blob:test-archive");
});
