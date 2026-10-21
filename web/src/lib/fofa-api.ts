import { http } from "./api";

export type FofaConfig = { enabled: boolean; api_key_set: boolean };
export type FofaSettingsPatch = { enabled?: boolean; api_key?: string };
export type FofaConnectionTestInput = { api_key?: string };

export function fofaSettingsStatus(config: FofaConfig): "unconfigured" | "disabled" | "enabled" {
  if (!config.api_key_set) return "unconfigured";
  return config.enabled ? "enabled" : "disabled";
}

function configResponse(value: unknown): FofaConfig {
  const config = value as Partial<FofaConfig> | null;
  if (typeof config?.enabled !== "boolean" || typeof config.api_key_set !== "boolean") {
    throw new Error("FOFA 配置响应无效，请重新读取");
  }
  // 设置界面仅保留存在状态，绝不回填服务端密钥。
  return { enabled: config.enabled, api_key_set: config.api_key_set };
}

export function fofaSettingsPatch(config: FofaConfig | null, enabled: boolean, apiKey: string): FofaSettingsPatch {
  if (!config) throw new Error("请先成功读取 FOFA 配置");
  const key = apiKey.trim();
  if (enabled && !config.api_key_set && !key) throw new Error("请先输入 FOFA API Key，再开启工具");
  // 留空表示保留；清除操作由独立按钮明确提交空字符串。
  return key ? { enabled, api_key: key } : { enabled };
}

export function fofaConnectionTestInput(config: FofaConfig | null, apiKey: string): FofaConnectionTestInput {
  if (!config) throw new Error("请先成功读取 FOFA 配置");
  const key = apiKey.trim();
  if (!key && !config.api_key_set) throw new Error("请先输入 FOFA API Key");
  return key ? { api_key: key } : {};
}

export const fofaAPI = {
  config: () => http<FofaConfig>("/integrations/fofa", { cache: "no-store" }).then(configResponse),
  save: (patch: FofaSettingsPatch) =>
    http<FofaConfig>("/integrations/fofa", { method: "PUT", body: JSON.stringify(patch) }).then(configResponse),
  enable: (config: FofaConfig | null) => {
    if (!config) throw new Error("请先成功读取 FOFA 配置");
    if (!config.api_key_set) throw new Error("请先保存 FOFA API Key");
    // 独立启用动作只变更总开关，不读取、替换或测试已保存的密钥。
    return http<FofaConfig>("/integrations/fofa", {
      method: "PUT",
      body: JSON.stringify({ enabled: true }),
    }).then(configResponse);
  },
  clear: () =>
    http<FofaConfig>("/integrations/fofa", {
      method: "PUT",
      body: JSON.stringify({ enabled: false, api_key: "" }),
    }).then(configResponse),
  test: async (input: FofaConnectionTestInput) => {
    const value = await http<{ ok?: unknown; message?: unknown } | null>("/integrations/fofa/test", {
      method: "POST",
      body: JSON.stringify(input),
    });
    if (value?.ok !== true || typeof value.message !== "string") {
      throw new Error("FOFA 连接测试未确认成功");
    }
    return { ok: true as const, message: value.message };
  },
};
