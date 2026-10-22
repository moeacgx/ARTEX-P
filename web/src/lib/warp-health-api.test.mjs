import { RevisionSubmission } from "./proxy-policy.ts";
import { globalWarpHealthAPI } from "./warp-health-api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const original = globalThis.fetch;
const dto = {
  scope: "global",
  revision: 2,
  auto_recover_enabled: true,
  migrated_disabled_nodes: 1,
  probe_interval_seconds: 60,
  failure_threshold: 3,
  recovery_min_seconds: 300,
  recovery_max_seconds: 3600,
};
test.afterEach(() => {
  globalThis.fetch = original;
});
test("全局策略 GET 与 CAS PUT 只访问单一策略，不遍历节点", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url, method: init?.method ?? "GET", body: init?.body ? JSON.parse(init.body) : null });
    return Response.json(dto);
  };
  assert.deepEqual(await globalWarpHealthAPI.get(), dto);
  await globalWarpHealthAPI.save(2, false);
  assert.deepEqual(calls, [
    { url: "/api/proxies/warp-health-policy", method: "GET", body: null },
    {
      url: "/api/proxies/warp-health-policy",
      method: "PUT",
      body: { expected_revision: 2, auto_recover_enabled: false },
    },
  ]);
});
test("全局读取拒绝未知开关/错误作用域/不安全版本，保存拒绝无效意图", async () => {
  for (const invalid of [
    { scope: "node" },
    { auto_recover_enabled: undefined },
    { revision: 0 },
    { revision: 9007199254740992 },
    { migrated_disabled_nodes: -1 },
  ]) {
    globalThis.fetch = async () => Response.json({ ...dto, ...invalid });
    await assert.rejects(globalWarpHealthAPI.get(), /无效/);
  }
  assert.throws(() => globalWarpHealthAPI.save(undefined, true), /版本/);
  assert.throws(() => globalWarpHealthAPI.save(0, undefined), /开关/);
});
for (const status of [409, 500])
  test(`全局保存 ${status}：保留草稿，仅GET回读，不重放PUT`, async () => {
    const calls = [];
    globalThis.fetch = async (_url, init) => {
      calls.push(init?.method ?? "GET");
      return init?.method === "PUT"
        ? Response.json({ code: "warp_health_revision_conflict", error: "合成失败", current_revision: 3 }, { status })
        : Response.json({ ...dto, revision: 3 });
    };
    const flow = new RevisionSubmission();
    await assert.rejects(
      flow.submit(
        async () => {
          await globalWarpHealthAPI.save(2, false);
        },
        () => globalWarpHealthAPI.get(),
        () => true,
      ),
    );
    await flow.submit(
      async () => {
        await globalWarpHealthAPI.save(2, false);
      },
      () => globalWarpHealthAPI.get(),
      () => true,
    );
    assert.deepEqual(calls, ["PUT", "GET"]);
    assert.equal(flow.confirmed, false);
  });
test("全局保存断网后只核对权威策略，禁止静默重试", async () => {
  const calls = [];
  globalThis.fetch = async (_url, init) => {
    calls.push(init?.method ?? "GET");
    if (init?.method === "PUT") throw new TypeError("network");
    return Response.json({ ...dto, revision: 3, auto_recover_enabled: false });
  };
  const flow = new RevisionSubmission();
  await assert.rejects(
    flow.submit(
      async () => {
        await globalWarpHealthAPI.save(2, false);
      },
      () => globalWarpHealthAPI.get(),
      () => true,
    ),
  );
  await flow.reconcile(() => globalWarpHealthAPI.get());
  assert.deepEqual(calls, ["PUT", "GET"]);
  assert.equal(flow.reviewRequired, true);
});
