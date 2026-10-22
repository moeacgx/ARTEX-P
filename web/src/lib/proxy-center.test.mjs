import { mockHandle } from "./mock/handler.ts";
import {
  buildProxyPayload,
  proxyEndpoint,
  proxyErrorText,
  proxyHealthStatus,
  proxyProbeResult,
  proxySelectable,
  taskProxyError,
  taskProxyLabel,
} from "./proxy-center.ts";
import assert from "node:assert/strict";
import test from "node:test";

const form = (extra = {}) => ({
  name: "出口一",
  kind: "http",
  protocol: "socks5",
  url: "http://user@proxy.example:8080",
  host: "resin.example",
  port: "1080",
  password: "",
  platform: "Default",
  adminURL: "",
  adminToken: "",
  enabled: true,
  clearPassword: false,
  clearAdminToken: false,
  ...extra,
});
const proxy = (extra = {}) => ({ id: 3, name: "出口一", kind: "http", enabled: true, status: "healthy", ...extra });
const selection = (extra = {}) => ({
  loading: false,
  error: "",
  mode: "inherit",
  globalID: 3,
  items: [proxy()],
  ...extra,
});

test("编辑不把脱敏地址或空秘密回写，保留服务器凭据", () => {
  assert.deepEqual(buildProxyPayload(form({ url: "" }), true), { name: "出口一", kind: "http", enabled: true });
  const payload = buildProxyPayload(form({ kind: "resin", url: "" }), true);
  assert.equal(payload.protocol, "socks5");
  assert.equal("password" in payload, false);
  assert.equal("resin_admin_token" in payload, false);
});

test("显式清除优先于残留输入，普通密码和 Resin 两种秘密都可单独清除", () => {
  const payload = buildProxyPayload(
    form({
      kind: "resin",
      password: "fake-proxy",
      adminToken: "fake-admin",
      clearPassword: true,
      clearAdminToken: true,
    }),
    true,
  );
  assert.equal(payload.clear_password, true);
  assert.equal(payload.clear_resin_admin_token, true);
  assert.equal("password" in payload, false);
  assert.equal("resin_admin_token" in payload, false);
  assert.equal(buildProxyPayload(form({ clearPassword: true }), true).clear_password, true);
});

test("新秘密按原值发送，不裁剪有效认证字符", () => {
  const payload = buildProxyPayload(
    form({ kind: "resin", protocol: "http", password: " fake-proxy ", adminToken: " fake-admin " }),
    false,
  );
  assert.equal(payload.protocol, "http");
  assert.equal(payload.password, " fake-proxy ");
  assert.equal(payload.resin_admin_token, " fake-admin ");
});

test("WARP 创建不要求 URL 且不携带上个表单的凭据", () => {
  assert.deepEqual(buildProxyPayload(form({ kind: "warp", password: "fake", adminToken: "fake" }), false), {
    name: "出口一",
    kind: "warp",
    accept_terms: true,
  });
});

test("无效协议、端口、名称或 Resin Token 在提交前拒绝", () => {
  for (const value of [
    form({ kind: "socks5" }),
    form({ kind: "resin", port: "70000", password: "fake" }),
    form({ kind: "resin", port: "1.5", password: "fake" }),
    form({ name: " " }),
    form({ kind: "resin" }),
    form({ url: "" }),
  ]) {
    assert.throws(() => buildProxyPayload(value, false));
  }
});

test("目录加载或失败会禁止所有任务模式，成功重试后恢复", () => {
  for (const mode of ["direct", "inherit", "fixed"]) {
    assert.notEqual(taskProxyError(selection({ mode, id: 3, loading: true })), "");
    assert.notEqual(taskProxyError(selection({ mode, id: 3, error: "failed" })), "");
    assert.equal(taskProxyError(selection({ mode, id: 3 })), "");
  }
});

test("已删除、停用、失败或停止的编号绝不降级为直连", () => {
  for (const items of [
    [],
    [proxy({ enabled: false })],
    [proxy({ status: "failed" })],
    [proxy({ status: "stopped" })],
    [proxy({ status: "starting" })],
    [proxy({ status: "future-status" })],
  ]) {
    assert.notEqual(taskProxyError(selection({ mode: "fixed", id: 3, items })), "");
    assert.notEqual(taskProxyError(selection({ items })), "");
    assert.equal(taskProxyError(selection({ mode: "direct", items })), "");
  }
  assert.notEqual(taskProxyError(selection({ mode: "fixed" })), "");
  assert.equal(taskProxyError(selection({ globalID: null, items: [] })), "");
  assert.equal(proxySelectable(proxy({ status: "unknown" })), true);
});

test("显示快照并区分默认直连与缺失快照", () => {
  assert.equal(taskProxyLabel({ proxy_mode: "inherit", proxy: proxy({ id: 8 }) }), "代理 #8");
  assert.equal(taskProxyLabel({ proxy_mode: "inherit", proxy_id: 0 }), "默认直连");
  assert.equal(taskProxyLabel({ proxy_mode: "fixed", proxy_id: 8 }), "代理快照缺失");
  assert.equal(taskProxyLabel({}), "代理快照缺失");
  assert.equal(taskProxyLabel({ proxy_mode: "direct" }), "直连");
});

test("URL、诊断和无效地址均不泄露示例凭据", () => {
  const endpoint = proxyEndpoint(
    proxy({ url: "http://fake-user:fake-password@proxy.example:8080?token=fake-secret#fake-secret" }),
  );
  assert.equal(endpoint.includes("fake-"), false);
  assert.equal(proxyEndpoint(proxy({ url: "fake-password" })), "地址格式未知");
  const error = proxyErrorText(
    new Error(
      "http://fake-user:fake-password@proxy.example Bearer fake-bearer token=fake-token password:fake-password",
    ),
  );
  assert.equal(error.includes("fake-"), false);
});

const verifiedWarp = (extra = {}) =>
  proxy({
    id: "3",
    kind: "warp",
    warp_phase: "ready",
    warp_health: {
      proxy_id: "3",
      desired_running: true,
      state: "ready",
      egress_ip: "203.0.113.42",
      last_checked_at: "2026-10-22T06:00:00Z",
    },
    ...extra,
  });

test("WARP 检测提示和列表必须同时有当前有效出口，历史 healthy 不能误报成功", () => {
  const valid = verifiedWarp();
  assert.equal(proxyProbeResult(valid, valid).available, true);
  assert.match(proxyProbeResult(valid, valid).message, /203\.0\.113\.42/);
  assert.equal(proxyHealthStatus(valid), "healthy");
  for (const result of [
    verifiedWarp({ warp_health: undefined, egress_ip: "203.0.113.42" }),
    ...[
      { egress_ip: "" },
      { egress_ip: "   " },
      { desired_running: false },
      { state: "stopped" },
      { state: "checking" },
      { state: "failed" },
      { proxy_id: "4" },
      { last_checked_at: "invalid" },
      { last_checked_at: undefined },
    ].map((patch) => verifiedWarp({ warp_health: { ...valid.warp_health, ...patch } })),
  ]) {
    assert.equal(proxyProbeResult(valid, result).available, false);
    assert.equal(proxyHealthStatus(result), "unknown");
  }
  for (const patch of [{ enabled: false }, { status: "stopped" }, { status: "failed" }])
    assert.equal(proxyProbeResult(valid, verifiedWarp(patch)).available, false);
});

test("检测不能把别的类型或编号冒充当前 WARP，普通失败和停用也不提示可用", () => {
  const valid = verifiedWarp();
  assert.throws(() => proxyProbeResult(valid, { ...valid, kind: "http" }), /不一致/);
  assert.throws(() => proxyProbeResult(valid, { ...valid, id: "4" }), /不一致/);
  assert.equal(proxyProbeResult(proxy(), proxy()).available, true);
  assert.equal(proxyProbeResult(proxy(), proxy({ enabled: false })).available, false);
  assert.equal(proxyProbeResult(proxy(), proxy({ status: "failed" })).available, false);
});

test("演示检测明确拒绝真实验证，不伪造 HTTP healthy，不改变 WARP 列表", async () => {
  const before = structuredClone(await mockHandle("GET", "/proxies"));
  await assert.rejects(mockHandle("POST", "/proxies/1/test"), /演示模式未执行真实代理检测/);
  assert.deepEqual(await mockHandle("GET", "/proxies"), before);
});

test("演示编辑返回完整代理且列表回读一致，不把保存当成连接成功", async () => {
  const before = structuredClone(await mockHandle("GET", "/proxies"));
  const original = before.proxies[0];
  try {
    const saved = await mockHandle(
      "PUT",
      `/proxies/${original.id}`,
      JSON.stringify({ name: "修改名称", enabled: true }),
    );
    assert.equal(saved.id, original.id);
    assert.equal(saved.kind, "warp");
    assert.equal(saved.status, "stopped");
    assert.equal(saved.name, "修改名称");
    assert.deepEqual((await mockHandle("GET", "/proxies")).proxies[0], saved);
    assert.equal(proxyProbeResult(saved, saved).available, false);
  } finally {
    await mockHandle("PUT", `/proxies/${original.id}`, JSON.stringify(original));
  }
});

test("演示全局代理路由优先于编号编辑，改为直连与回读一致", async () => {
  const before = await mockHandle("GET", "/proxies");
  try {
    assert.deepEqual(await mockHandle("PUT", "/proxies/global", JSON.stringify({ proxy_id: null })), {
      global_proxy_id: null,
    });
    assert.equal((await mockHandle("GET", "/proxies")).global_proxy_id, null);
  } finally {
    await mockHandle("PUT", "/proxies/global", JSON.stringify({ proxy_id: before.global_proxy_id }));
  }
});
