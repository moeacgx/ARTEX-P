import { api } from "./api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const originalFetch = globalThis.fetch;
test.afterEach(() => {
  globalThis.fetch = originalFetch;
});

test("设备总览保留服务端技能元信息和设备上报，旧列表接口仍返回数组", async () => {
  const version = "a".repeat(64);
  const devices = [{ id: 2, status: "approved", skill_version: version, skill_reported_at: "2026-10-28T01:00:00Z" }];
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, method: options?.method ?? "GET" });
    return Response.json({ devices, skill: { version } });
  };
  assert.deepEqual(await api.localAgentOverview(), { devices, skill: { version } });
  assert.deepEqual(await api.localAgentDevices(), devices);
  assert.deepEqual(calls, [
    { url: "/api/local-agent/devices", method: "GET" },
    { url: "/api/local-agent/devices", method: "GET" },
  ]);
});

test("旧服务器缺少技能元信息时保留未知状态，空设备字段归一为数组", async () => {
  globalThis.fetch = async () => Response.json({ devices: null });
  const response = await api.localAgentOverview();
  assert.deepEqual(response.devices, []);
  assert.equal(response.skill, undefined);
});
