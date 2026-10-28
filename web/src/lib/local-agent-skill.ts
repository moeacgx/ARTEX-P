import type { LocalAgentDevice } from "./types";

export function validSkillVersion(value?: string): string {
  return typeof value === "string" && /^[a-f0-9]{64}$/i.test(value) ? value.toLowerCase() : "";
}

export function localAgentSkillState(deviceVersion?: string, serverVersion?: string) {
  const installed = validSkillVersion(deviceVersion);
  const available = validSkillVersion(serverVersion);
  if (!available) return { kind: "unavailable", label: "服务器版本未知" } as const;
  if (!installed) return { kind: "unknown", label: "版本未知" } as const;
  if (installed === available) return { kind: "current", label: "当前版本" } as const;
  return { kind: "different", label: "有更新待检查" } as const;
}

export function localAgentSkillNotice(devices: LocalAgentDevice[], serverVersion?: string) {
  let different = 0;
  let unknown = 0;
  for (const device of devices) {
    if (device.status !== "approved") continue;
    const state = localAgentSkillState(device.skill_version, serverVersion);
    if (state.kind === "different") different++;
    if (state.kind === "unknown") unknown++;
  }
  return { different, unknown };
}

export function localAgentUpdatePrompt(serverUrl: string, serverVersion?: string) {
  const version = validSkillVersion(serverVersion);
  return `请更新我本机已安装的 ARTEX 本地 Agent Skill。\nARTEX 地址：${serverUrl}\n服务器技能包版本：${version || "待页面读取后核对"}\n\n请使用我从此 ARTEX「本地 Agent 接入」页面下载的新 Skill 包，先定位现有安装目录，再将包中全部 Skill 文件覆盖到同一目录，并重新阅读 SKILL.md。保留本机设备身份及其配置目录，不删除密钥、不重新配对、不运行 auth login。不要读取或输出私钥、管理员密码、JWT 或 API Key。\n\n使用更新后的 scripts/artex-agent.mjs 运行 auth status，核对返回的 baseUrl 与上面 ARTEX 地址指向同一实例：baseUrl 允许带 /api/local-agent 后缀，应核对相同来源和部署路径前缀，不要求原始字符串相等。设备 ID、指纹和审批权限应与更新前一致，并核对 skill_version 与服务器技能包版本。若地址不匹配或无法核验，请停下来说明；不要自动切换身份。向我报告核验结果，再刷新网页确认设备版本。仅下载压缩包不代表安装完成。`;
}
