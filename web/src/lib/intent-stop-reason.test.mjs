import { intentStopReason, statusMeta } from "./status.ts";
import assert from "node:assert/strict";
import test from "node:test";

const knownCodes = [
  "run_time_limit",
  "max_turns",
  "task_timeout_settle",
  "llm_quota_exhausted",
  "llm_auth_failed",
  "llm_rate_limited",
  "llm_upstream_error",
  "llm_response_timeout",
  "llm_error",
];

test("exhausted 状态标签不再暗示费用预算", () => {
  const meta = statusMeta("intent", "exhausted");
  assert.equal(meta.label, "已达上限");
  assert.ok(!meta.label.includes("预算"));
});

test("非 blocked/exhausted 状态没有终止原因文案", () => {
  for (const state of ["open", "running", "paused", "done", "stopped"]) {
    assert.equal(intentStopReason(state, "run_time_limit"), null);
  }
});

test("历史 exhausted 无原因：给中性说明，不编造原因", () => {
  for (const reason of [undefined, null, "", "   "]) {
    const stop = intentStopReason("exhausted", reason);
    assert.ok(stop);
    assert.ok(!/余额|额度|预算|充值/.test(stop.title));
    assert.ok(stop.detail.includes("更新前"));
    assert.ok(!stop.detail.includes("1200"));
  }
});

test("历史 blocked 无原因：不能认定为余额不足", () => {
  const stop = intentStopReason("blocked", "");
  assert.ok(stop);
  assert.ok(!/余额|额度|预算|充值/.test(stop.title));
  assert.ok(stop.detail.includes("未保存具体终止原因"));
});

test("未知原因代码回退到中性文案", () => {
  const stop = intentStopReason("exhausted", "some_future_code");
  assert.ok(stop);
  assert.equal(stop.title, "已达运行上限");
});

test("只有 llm_quota_exhausted 允许出现额度不足", () => {
  for (const code of knownCodes) {
    const state = code === "llm_quota_exhausted" || code.startsWith("llm_") ? "blocked" : "exhausted";
    const stop = intentStopReason(state, code);
    assert.ok(stop, code);
    const text = (stop.title + stop.detail).replace(
      /没有明确的额度不足信息|未发现明确的额度不足证据|并非余额问题/g,
      "",
    );
    if (code === "llm_quota_exhausted") {
      assert.ok(/额度不足/.test(stop.title));
    } else {
      assert.ok(!/余额不足|额度不足|充值/.test(text), code + " 不应暗示余额/额度不足");
    }
  }
});

test("401 / 限流 / 5xx / 响应超时 / 通用错误明确说明并非余额问题或与余额无关", () => {
  assert.ok(intentStopReason("blocked", "llm_auth_failed").detail.includes("并非余额问题"));
  assert.ok(intentStopReason("blocked", "llm_rate_limited").detail.includes("并非余额问题"));
  assert.ok(intentStopReason("blocked", "llm_upstream_error").detail.includes("并非余额问题"));
  assert.ok(intentStopReason("blocked", "llm_response_timeout").detail.includes("余额均无关"));
  assert.ok(intentStopReason("blocked", "llm_error").detail.includes("未发现明确的额度不足证据"));
});

test("时间上限文案说明默认 1200 秒与 0 表示不限，且与费用预算无关", () => {
  const stop = intentStopReason("exhausted", "run_time_limit");
  assert.ok(stop);
  assert.ok(stop.detail.includes("1200"));
  assert.ok(stop.detail.includes("20 分钟"));
  assert.ok(stop.detail.includes("0 表示明确不限"));
  assert.ok(stop.detail.includes("与费用预算无关"));
});

test("步数上限与任务超时收尾文案各自独立且与费用预算无关", () => {
  const turns = intentStopReason("exhausted", "max_turns");
  const settle = intentStopReason("exhausted", "task_timeout_settle");
  assert.ok(turns && settle);
  assert.notEqual(turns.title, settle.title);
  assert.ok(turns.detail.includes("与费用预算无关"));
  assert.ok(settle.detail.includes("与费用预算无关"));
});

test("llm_chain_unavailable 只说明模型配置不可用，不暗示余额问题", () => {
  const stop = intentStopReason("blocked", "llm_chain_unavailable");
  assert.ok(stop);
  const text = (stop.title + stop.detail).replace(/没有明确的额度不足证据|不能认定为余额问题/g, "");
  assert.ok(!/余额|额度不足|充值/.test(text));
});
