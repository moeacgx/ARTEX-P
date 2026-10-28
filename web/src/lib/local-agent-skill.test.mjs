import { localAgentSkillNotice, localAgentSkillState, localAgentUpdatePrompt } from "./local-agent-skill.ts";
import assert from "node:assert/strict";
import test from "node:test";

const current = "a".repeat(64);
const different = "b".repeat(64);

test("内容摘要一致才是当前版本；不同摘要只说明不一致，不猜测版本先后", () => {
  assert.equal(localAgentSkillState(current, current).kind, "current");
  assert.equal(localAgentSkillState(current.toUpperCase(), current).kind, "current");
  assert.equal(localAgentSkillState(different, current).kind, "different");
  assert.equal(localAgentSkillState(current, different).kind, "different");
});

test("旧设备、无元信息的服务端和无效摘要均不能误报当前版本", () => {
  assert.equal(localAgentSkillState(undefined, current).kind, "unknown");
  assert.equal(localAgentSkillState("old-version", current).kind, "unknown");
  assert.equal(localAgentSkillState(current, undefined).kind, "unavailable");
  assert.equal(localAgentSkillState(current, "a".repeat(12)).kind, "unavailable");
  assert.equal(localAgentSkillState("", "").kind, "unavailable");
});

test("更新通知仅统计已授权设备，撤销和待审批设备不制造更新通知", () => {
  const devices = [
    { status: "approved", skill_version: current },
    { status: "approved", skill_version: different },
    { status: "approved" },
    { status: "revoked", skill_version: different },
    { status: "pending", skill_version: different },
  ];
  assert.deepEqual(localAgentSkillNotice(devices, current), { different: 1, unknown: 1 });
  assert.deepEqual(localAgentSkillNotice(devices), { different: 0, unknown: 0 });
  // 只有设备重新上报一致版本才解除提醒。
  devices[1].skill_version = current;
  assert.deepEqual(localAgentSkillNotice(devices, current), { different: 0, unknown: 1 });
});

test("更新提示词绑定实例和完整包版本，并要求保留身份后实际核验", () => {
  const prompt = localAgentUpdatePrompt("https://artex.example.com", current);
  assert.ok(prompt.includes("https://artex.example.com"));
  assert.ok(prompt.includes(current));
  assert.ok(prompt.includes("保留本机设备身份"));
  assert.ok(prompt.includes("不重新配对"));
  assert.ok(prompt.includes("auth status"));
  assert.ok(prompt.includes("baseUrl"));
  assert.ok(prompt.includes("仅下载压缩包不代表安装完成"));
});
