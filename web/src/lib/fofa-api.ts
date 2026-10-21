import { http } from "./api";

export type FofaConfig = {
  enabled: boolean;
  api_key_set: boolean;
  request_interval_seconds: number;
  rate_limit_cooldown_seconds: number;
};
export type FofaSettingsPatch = {
  enabled?: boolean;
  api_key?: string;
  request_interval_seconds?: number;
  rate_limit_cooldown_seconds?: number;
};
export type FofaThrottleDraft = { request_interval_seconds: string; rate_limit_cooldown_seconds: string };
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
  if (!validSeconds(config.request_interval_seconds, 60) || !validSeconds(config.rate_limit_cooldown_seconds, 3600)) {
    throw new Error("FOFA 节流配置响应无效，请重新读取");
  }
  // 设置界面仅保留存在状态，绝不回填服务端密钥。
  return {
    enabled: config.enabled,
    api_key_set: config.api_key_set,
    request_interval_seconds: config.request_interval_seconds,
    rate_limit_cooldown_seconds: config.rate_limit_cooldown_seconds,
  };
}

function validSeconds(value: unknown, maximum: number): value is number {
  return typeof value === "number" && Number.isInteger(value) && value >= 1 && value <= maximum;
}

function parseSeconds(value: string, maximum: number, label: string): number {
  const text = value.trim();
  const parsed = Number(text);
  if (!/^\d+$/.test(text) || !validSeconds(parsed, maximum)) {
    throw new Error(`${label}必须为 1–${maximum} 秒的整数`);
  }
  return parsed;
}

export function fofaSettingsPatch(
  config: FofaConfig | null,
  enabled: boolean,
  apiKey: string,
  throttle?: FofaThrottleDraft,
): FofaSettingsPatch {
  if (!config) throw new Error("请先成功读取 FOFA 配置");
  const key = apiKey.trim();
  if (enabled && !config.api_key_set && !key) throw new Error("请先输入 FOFA API Key，再开启工具");
  // 留空表示保留；清除操作由独立按钮明确提交空字符串。
  const patch: FofaSettingsPatch = key ? { enabled, api_key: key } : { enabled };
  if (throttle) {
    const interval = parseSeconds(throttle.request_interval_seconds, 60, "请求间隔");
    const cooldown = parseSeconds(throttle.rate_limit_cooldown_seconds, 3600, "限流冷却");
    // 只提交实际修改的字段，普通 Key/开关保存不会覆盖服务端的其它节流设置。
    if (interval !== config.request_interval_seconds) patch.request_interval_seconds = interval;
    if (cooldown !== config.rate_limit_cooldown_seconds) patch.rate_limit_cooldown_seconds = cooldown;
    if (
      (patch.request_interval_seconds !== undefined || patch.rate_limit_cooldown_seconds !== undefined) &&
      enabled === config.enabled &&
      !key
    ) {
      delete patch.enabled;
    }
  }
  return patch;
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
