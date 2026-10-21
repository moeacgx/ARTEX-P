import { api } from "./api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const originals = new Map(
  ["fetch", "window", "document", "localStorage", "setTimeout"].map((key) => [
    key,
    Object.getOwnPropertyDescriptor(globalThis, key),
  ]),
);
const originalCreate = URL.createObjectURL;
const originalRevoke = URL.revokeObjectURL;

test.afterEach(() => {
  for (const [key, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor);
    else delete globalThis[key];
  }
  URL.createObjectURL = originalCreate;
  URL.revokeObjectURL = originalRevoke;
});

function browser() {
  const events = [];
  const timers = [];
  const anchor = {
    href: "",
    download: "",
    click: () => events.push("click"),
    remove: () => events.push("remove"),
  };
  globalThis.window = { location: { href: "" } };
  globalThis.localStorage = {
    getItem: (key) => (key === "artex_token" ? "test-token" : null),
    removeItem: (key) => events.push(`remove:${key}`),
  };
  globalThis.document = {
    cookie: "",
    createElement: (tag) => {
      assert.equal(tag, "a");
      return anchor;
    },
    body: { appendChild: () => events.push("append") },
  };
  globalThis.setTimeout = (callback, delay) => {
    timers.push({ callback, delay });
    return 1;
  };
  URL.createObjectURL = (blob) => {
    assert.equal(blob.type, "application/zip");
    events.push("create");
    return "blob:skill-download";
  };
  URL.revokeObjectURL = (url) => events.push(`revoke:${url}`);
  return { anchor, events, timers };
}

test("打包下载编码中文技能名、携带登录凭据并释放下载资源", async () => {
  const { anchor, events, timers } = browser();
  globalThis.fetch = async (url, options) => {
    assert.equal(url, `/api/skills/${encodeURIComponent("中文技能")}/download`);
    assert.equal(options.headers.Authorization, "Bearer test-token");
    assert.equal(options.cache, "no-store");
    assert.equal(options.method ?? "GET", "GET");
    return new Response(new Uint8Array([80, 75, 3, 4]), { headers: { "Content-Type": "application/zip" } });
  };
  await api.downloadSkill("中文技能");
  assert.equal(anchor.href, "blob:skill-download");
  assert.equal(anchor.download, "中文技能.zip");
  assert.deepEqual(events, ["create", "append", "click", "remove"]);
  assert.equal(timers.length, 1);
  assert.ok(timers[0].delay > 0);
  timers[0].callback();
  assert.equal(events.at(-1), "revoke:blob:skill-download");
});

test("下载失败显示后端原因，且不生成文件或修改技能", async () => {
  const { events } = browser();
  let requests = 0;
  globalThis.fetch = async () => {
    requests++;
    return Response.json({ error: "技能包含不允许导出的符号链接" }, { status: 400 });
  };
  await assert.rejects(api.downloadSkill("fofa-search"), /符号链接/);
  assert.equal(requests, 1);
  assert.deepEqual(events, []);
});

test("非 JSON 下载错误保留状态码", async () => {
  const { events } = browser();
  globalThis.fetch = async () => new Response("gateway unavailable", { status: 502 });
  await assert.rejects(api.downloadSkill("fofa-search"), /502/);
  assert.deepEqual(events, []);
});

test("登录失效时清理凭据并跳转登录，不下载错误页", async () => {
  const { events } = browser();
  globalThis.fetch = async () => Response.json({ error: "unauthorized" }, { status: 401 });
  await assert.rejects(api.downloadSkill("fofa-search"), /未授权/);
  assert.equal(window.location.href, "/login");
  assert.equal(document.cookie, "artex_token=; path=/; max-age=0");
  assert.deepEqual(events, ["remove:artex_token"]);
});

test("代理误返回网页时拒绝将其保存为 ZIP", async () => {
  const { events } = browser();
  globalThis.fetch = async () => new Response("<html>登录</html>", { headers: { "Content-Type": "text/html" } });
  await assert.rejects(api.downloadSkill("fofa-search"), /ZIP/);
  assert.deepEqual(events, []);
});
