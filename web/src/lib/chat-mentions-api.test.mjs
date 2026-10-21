import { api } from "./api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const originalFetch = globalThis.fetch;
test.afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("技能候选复用引用接口并保留中文查询、分页和字符串标识", async () => {
  const controller = new AbortController();
  const item = { kind: "skill", id: "中文技能", label: "中文技能", description: "测试描述" };
  globalThis.fetch = async (url, options) => {
    const parsed = new URL(url, "http://localhost");
    assert.equal(parsed.pathname, "/api/chat/mentions");
    assert.equal(parsed.searchParams.get("kind"), "skill");
    assert.equal(parsed.searchParams.get("q"), "资产发现 & 查询");
    assert.equal(parsed.searchParams.get("cursor"), "技能-二");
    assert.equal(options.signal, controller.signal);
    return Response.json({ items: [item], next_cursor: "next" });
  };
  assert.deepEqual(await api.chatMentions("skill", "资产发现 & 查询", controller.signal, "技能-二"), {
    items: [item],
    next_cursor: "next",
  });
});

test("发送消息保留技能引用与既有记录引用", async () => {
  const message = "请使用 @[技能#中文技能] 分析 @[漏洞#12 标题]";
  globalThis.fetch = async (url, options) => {
    assert.equal(url, "/api/chat?task=34");
    assert.equal(JSON.parse(options.body).message, message);
    return Response.json({ reply: "", mode: "async" });
  };
  await api.chat(message, "34");
});
