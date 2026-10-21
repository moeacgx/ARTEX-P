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
export type FofaAccountQuota = {
  fofa_point: number | null;
  remain_free_point: number | null;
  remain_api_query: number | null;
  remain_api_data: number | null;
};
export type FofaAccountCheck = {
  ok: true;
  message: string;
  account: FofaAccountQuota;
  checked_at: string;
  key_source: "saved" | "draft";
};

function validQuotaNumber(value: unknown, integer: boolean): value is number | null {
  return (
    value === null ||
    (typeof value === "number" && Number.isFinite(value) && value >= 0 && (!integer || Number.isSafeInteger(value)))
  );
}

function validCheckedAt(value: unknown): value is string {
  if (typeof value !== "string") return false;
  const parts = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(\.\d+)?([Zz]|([+-])(\d{2}):(\d{2}))$/.exec(value);
  if (!parts) return false;
  const [year, month, day, hour, minute, second] = parts.slice(1, 7).map(Number);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return (
    month >= 1 &&
    month <= 12 &&
    day >= 1 &&
    day <= days[month - 1] &&
    hour <= 23 &&
    minute <= 59 &&
    second <= 59 &&
    (!parts[9] || (Number(parts[10]) <= 23 && Number(parts[11]) <= 59)) &&
    Number.isFinite(Date.parse(value))
  );
}

function accountResponse(value: unknown, input: FofaConnectionTestInput): FofaAccountCheck {
  const result = value as Partial<FofaAccountCheck> | null;
  if (result?.ok !== true || typeof result.message !== "string") throw new Error("FOFA 连接测试未确认成功");
  const account = result.account;
  if (
    !account ||
    typeof account !== "object" ||
    Array.isArray(account) ||
    !validQuotaNumber(account.fofa_point, false) ||
    !validQuotaNumber(account.remain_free_point, false) ||
    !validQuotaNumber(account.remain_api_query, true) ||
    !validQuotaNumber(account.remain_api_data, true)
  ) {
    throw new Error("FOFA 账户额度响应无效，请重新查询");
  }
  if (!validCheckedAt(result.checked_at)) throw new Error("FOFA 额度读取时间无效，请重新查询");
  const source = input.api_key === undefined ? "saved" : "draft";
  if (result.key_source !== source) throw new Error("FOFA 额度 Key 来源无效，请重新查询");
  // 只保留四项额度与本次读取元数据，丢弃邮箱、密钥等非展示字段。
  return {
    ok: true,
    message: result.message,
    account: {
      fofa_point: account.fofa_point,
      remain_free_point: account.remain_free_point,
      remain_api_query: account.remain_api_query,
      remain_api_data: account.remain_api_data,
    },
    checked_at: result.checked_at,
    key_source: source,
  };
}

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
    const value = await http<unknown>("/integrations/fofa/test", {
      method: "POST",
      body: JSON.stringify(input),
    });
    return accountResponse(value, input);
  },
};
