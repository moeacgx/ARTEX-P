import assert from "node:assert/strict";
import test from "node:test";

import * as fofa from "./fofa-api.ts";

const { fofaAPI, fofaConnectionTestInput, fofaSettingsPatch } = fofa;

const originalFetch = globalThis.fetch;
test.afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("已保存 Key 但关闭总开关时，状态必须仍为未启用", () => {
  assert.equal(fofa.fofaSettingsStatus({ enabled: false, api_key_set: true }), "disabled");
  assert.equal(fofa.fofaSettingsStatus({ enabled: false, api_key_set: false }), "unconfigured");
  assert.equal(fofa.fofaSettingsStatus({ enabled: true, api_key_set: true }), "enabled");
  assert.equal(fofa.fofaSettingsStatus({ enabled: true, api_key_set: false }), "unconfigured");
});

test("单纯保存首个 Key 不自动启用，只有明确保存并启用才开启", async () => {
  const config = { enabled: false, api_key_set: false };
  const calls = [];
  globalThis.fetch = async (url, init) => {
    const body = JSON.parse(init.body);
    calls.push({ url, method: init.method, body });
    return Response.json({ enabled: body.enabled, api_key_set: true });
  };
  const saved = await fofaAPI.save(fofaSettingsPatch(config, false, "synthetic-first-key"));
  assert.equal(fofa.fofaSettingsStatus(saved), "disabled");
  const enabled = await fofaAPI.save(fofaSettingsPatch(config, true, "synthetic-first-key"));
  assert.equal(fofa.fofaSettingsStatus(enabled), "enabled");
  assert.deepEqual(calls, [
    { url: "/api/integrations/fofa", method: "PUT", body: { enabled: false, api_key: "synthetic-first-key" } },
    { url: "/api/integrations/fofa", method: "PUT", body: { enabled: true, api_key: "synthetic-first-key" } },
  ]);
});

test("一键启用只提交开关，保留已保存 Key 且不测试或搜索", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method, body: JSON.parse(init.body) });
    return Response.json({ enabled: true, api_key_set: true });
  };
  await fofaAPI.enable({ enabled: false, api_key_set: true });
  assert.deepEqual(calls, [{ url: "/api/integrations/fofa", method: "PUT", body: { enabled: true } }]);
});

test("配置未知或尚无已保存 Key 时，不能通过一键启用绕过检查", () => {
  assert.throws(() => fofaAPI.enable(null), /先.*读取/);
  assert.throws(() => fofaAPI.enable({ enabled: false, api_key_set: false }), /保存.*Key/);
});

test("读取 FOFA 配置不缓存，只返回开关与密钥存在状态", async () => {
  globalThis.fetch = async (url, init) => {
    assert.equal(url, "/api/integrations/fofa");
    assert.equal(init.cache, "no-store");
    return Response.json({ enabled: false, api_key_set: true, api_key: "不得回填的旧密钥" });
  };
  assert.deepEqual(await fofaAPI.config(), { enabled: false, api_key_set: true });
});

test("缺失或错误类型的配置不能被当成未配置", async () => {
  for (const value of [null, {}, { enabled: false }, { enabled: false, api_key_set: "false" }]) {
    globalThis.fetch = async () => Response.json(value);
    await assert.rejects(fofaAPI.config(), /配置响应无效/);
  }
  assert.throws(() => fofaSettingsPatch(null, false, ""), /先.*读取/);
  assert.throws(() => fofaConnectionTestInput(null, "新密钥"), /先.*读取/);
});

test("没有已保存或输入的密钥时不能开启工具或测试连接", () => {
  const config = { enabled: false, api_key_set: false };
  assert.throws(() => fofaSettingsPatch(config, true, "  "), /API Key/);
  assert.throws(() => fofaConnectionTestInput(config, "  "), /API Key/);
  assert.deepEqual(fofaSettingsPatch(config, false, ""), { enabled: false });
});

test("保存开关时留空保留旧密钥，输入新密钥才提交更换", async () => {
  const config = { enabled: false, api_key_set: true };
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method, body: JSON.parse(init.body) });
    return Response.json({ enabled: true, api_key_set: true });
  };
  await fofaAPI.save(fofaSettingsPatch(config, true, "  "));
  await fofaAPI.save(fofaSettingsPatch(config, true, "  synthetic-new-key  "));
  assert.deepEqual(calls, [
    { url: "/api/integrations/fofa", method: "PUT", body: { enabled: true } },
    { url: "/api/integrations/fofa", method: "PUT", body: { enabled: true, api_key: "synthetic-new-key" } },
  ]);
});

test("清除密钥明确提交空字符串，并同时关闭 FOFA 工具", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method, body: JSON.parse(init.body) });
    return Response.json({ enabled: false, api_key_set: false });
  };
  assert.deepEqual(await fofaAPI.clear(), { enabled: false, api_key_set: false });
  assert.deepEqual(calls, [{ url: "/api/integrations/fofa", method: "PUT", body: { enabled: false, api_key: "" } }]);
});

test("连接测试可用草稿密钥或旧密钥，只调用测试端点且不保存不搜索", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method, body: JSON.parse(init.body) });
    return Response.json({ ok: true, message: "连接成功" });
  };
  await fofaAPI.test(fofaConnectionTestInput({ enabled: false, api_key_set: false }, " synthetic-draft-key "));
  await fofaAPI.test(fofaConnectionTestInput({ enabled: false, api_key_set: true }, ""));
  assert.deepEqual(calls, [
    { url: "/api/integrations/fofa/test", method: "POST", body: { api_key: "synthetic-draft-key" } },
    { url: "/api/integrations/fofa/test", method: "POST", body: {} },
  ]);
});

test("连接测试响应必须确认成功，不能把未知结果当成功", async () => {
  for (const value of [{}, { ok: false, message: "失败" }, { ok: true }]) {
    globalThis.fetch = async () => Response.json(value);
    await assert.rejects(fofaAPI.test({}), /未确认成功/);
  }
});

test("保存失败保留服务端错误且不自动重试", async () => {
  let count = 0;
  globalThis.fetch = async () => {
    count += 1;
    return Response.json({ error: "FOFA API Key 无效" }, { status: 400 });
  };
  await assert.rejects(fofaAPI.save({ enabled: true }), (error) => error.status === 400 && /API Key 无效/.test(error.message));
  assert.equal(count, 1);
});
