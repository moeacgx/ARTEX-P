import assert from "node:assert/strict";
import test from "node:test";

import * as fofa from "./fofa-api.ts";

const { fofaAPI, fofaConnectionTestInput, fofaSettingsPatch } = fofa;
const throttleDefaults = { request_interval_seconds: 2, rate_limit_cooldown_seconds: 60 };
const configResponse = (value) => Response.json({ ...throttleDefaults, ...value });
const checkedAccount = (source = "saved") => ({
  ok: true,
  message: "连接成功",
  account: { fofa_point: 12.5, remain_free_point: 0, remain_api_query: 42, remain_api_data: null },
  checked_at: "2026-10-22T04:00:00Z",
  key_source: source,
});

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
    return configResponse({ enabled: body.enabled, api_key_set: true });
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
    return configResponse({ enabled: true, api_key_set: true });
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
    return configResponse({ enabled: false, api_key_set: true, api_key: "不得回填的旧密钥" });
  };
  assert.deepEqual(await fofaAPI.config(), { enabled: false, api_key_set: true, ...throttleDefaults });
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
    return configResponse({ enabled: true, api_key_set: true });
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
    return configResponse({ enabled: false, api_key_set: false });
  };
  assert.deepEqual(await fofaAPI.clear(), { enabled: false, api_key_set: false, ...throttleDefaults });
  assert.deepEqual(calls, [{ url: "/api/integrations/fofa", method: "PUT", body: { enabled: false, api_key: "" } }]);
});

test("连接测试可用草稿密钥或旧密钥，只调用测试端点且不保存不搜索", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method, body: JSON.parse(init.body) });
    return Response.json(checkedAccount(JSON.parse(init.body).api_key ? "draft" : "saved"));
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

test("读取节流值严格校验，不将缺失或无效值猜成默认值", async () => {
  for (const field of ["request_interval_seconds", "rate_limit_cooldown_seconds"]) {
    for (const value of [undefined, null, 0, -1, 1.5, "2", field === "request_interval_seconds" ? 61 : 3601]) {
      globalThis.fetch = async () => configResponse({ enabled: false, api_key_set: true, [field]: value });
      await assert.rejects(fofaAPI.config(), /节流.*无效/);
    }
  }
});

test("只修改请求间隔不覆盖冷却值、Key和开关草稿", () => {
  const config = { enabled: false, api_key_set: true, request_interval_seconds: 2, rate_limit_cooldown_seconds: 90 };
  assert.deepEqual(fofaSettingsPatch(config, false, "", { request_interval_seconds: "7", rate_limit_cooldown_seconds: "90" }), {
    request_interval_seconds: 7,
  });
  assert.deepEqual(fofaSettingsPatch(config, true, "new-key", { request_interval_seconds: "2", rate_limit_cooldown_seconds: "180" }), {
    enabled: true, api_key: "new-key", rate_limit_cooldown_seconds: 180,
  });
});

test("节流草稿只接受范围内整数，空值和非整数字符不能被自动转换", () => {
  const config = { enabled: false, api_key_set: false, ...throttleDefaults };
  for (const value of ["", " ", "0", "-1", "1.5", "2e1", "Infinity", "61"]) {
    assert.throws(() => fofaSettingsPatch(config, false, "", { request_interval_seconds: value, rate_limit_cooldown_seconds: "60" }), /请求间隔.*1.*60/);
  }
  for (const value of ["", "0", "-1", "0.5", "3601"]) {
    assert.throws(() => fofaSettingsPatch(config, false, "", { request_interval_seconds: "2", rate_limit_cooldown_seconds: value }), /限流冷却.*1.*3600/);
  }
  assert.deepEqual(fofaSettingsPatch(config, false, "", { request_interval_seconds: "60", rate_limit_cooldown_seconds: "3600" }), {
    request_interval_seconds: 60, rate_limit_cooldown_seconds: 3600,
  });
});

test("额度查询保留四项独立额度的0和null，只接收脱敏字段", async () => {
  globalThis.fetch = async () => Response.json({
    ...checkedAccount(),
    account: { ...checkedAccount().account, email: "private@example.test", api_key: "secret-must-not-leave-parser" },
    email: "private@example.test",
  });
  assert.deepEqual(await fofaAPI.test({}), {
    ok: true, message: "连接成功",
    account: { fofa_point: 12.5, remain_free_point: 0, remain_api_query: 42, remain_api_data: null },
    checked_at: "2026-10-22T04:00:00Z", key_source: "saved",
  });
});

test("额度数值必须非负有限，查询次数和条数还必须是安全整数", async () => {
  for (const field of ["fofa_point", "remain_free_point", "remain_api_query", "remain_api_data"]) {
    const invalid = [undefined, -1, "0", false, {}, []];
    if (field.startsWith("remain_api_")) invalid.push(1.5, Number.MAX_SAFE_INTEGER + 1);
    for (const value of invalid) {
      globalThis.fetch = async () => Response.json({ ...checkedAccount(), account: { ...checkedAccount().account, [field]: value } });
      await assert.rejects(fofaAPI.test({}), /额度.*无效/);
    }
    globalThis.fetch = async () => new Response(JSON.stringify(checkedAccount()).replace(`"${field}":${JSON.stringify(checkedAccount().account[field])}`, `"${field}":1e999`), { headers: { "Content-Type": "application/json" } });
    await assert.rejects(fofaAPI.test({}), /额度.*无效/);
  }
  globalThis.fetch = async () => Response.json({ ...checkedAccount(), account: { fofa_point: 0.25, remain_free_point: null, remain_api_query: 0, remain_api_data: Number.MAX_SAFE_INTEGER } });
  assert.deepEqual((await fofaAPI.test({})).account, { fofa_point: 0.25, remain_free_point: null, remain_api_query: 0, remain_api_data: Number.MAX_SAFE_INTEGER });
});

test("缺失账户或无效读取时间不能伪造额度快照", async () => {
  for (const account of [undefined, null, [], {}]) {
    globalThis.fetch = async () => Response.json({ ...checkedAccount(), account });
    await assert.rejects(fofaAPI.test({}), /额度.*无效/);
  }
  for (const checked_at of [undefined, null, 1, "", "yesterday", "2026-10-22", "2026-10-22T04:00:00", "2026-02-30T04:00:00Z", "2026-13-22T04:00:00Z", "2026-10-22T24:00:00Z", "2026-10-22T04:60:00Z", "2026-10-22T04:00:00+24:00"]) {
    globalThis.fetch = async () => Response.json({ ...checkedAccount(), checked_at });
    await assert.rejects(fofaAPI.test({}), /读取时间.*无效/);
  }
  globalThis.fetch = async () => Response.json({ ...checkedAccount(), checked_at: "2026-10-22T12:00:00.123456789+08:00" });
  assert.equal((await fofaAPI.test({})).checked_at, "2026-10-22T12:00:00.123456789+08:00");
});

test("额度Key来源必须明确且与本次请求一致", async () => {
  for (const key_source of [undefined, null, "", "unknown", "draft", 1]) {
    globalThis.fetch = async () => Response.json({ ...checkedAccount(), key_source });
    await assert.rejects(fofaAPI.test({}), /Key 来源.*无效/);
  }
  globalThis.fetch = async () => Response.json(checkedAccount("saved"));
  await assert.rejects(fofaAPI.test({ api_key: "synthetic-draft-key" }), /Key 来源.*无效/);
  globalThis.fetch = async () => Response.json(checkedAccount("draft"));
  assert.equal((await fofaAPI.test({ api_key: "synthetic-draft-key" })).key_source, "draft");
});

test("额度查询限流只返回错误，不重试或调用搜索端点", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init.method });
    return Response.json({ error: "FOFA 正在冷却，请稍后手动重试" }, { status: 429 });
  };
  await assert.rejects(fofaAPI.test({}), (error) => error.status === 429 && /冷却/.test(error.message));
  assert.deepEqual(calls, [{ url: "/api/integrations/fofa/test", method: "POST" }]);
});
