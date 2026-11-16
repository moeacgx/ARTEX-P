import { http } from "./api";

export type XiequWhitelistState = {
  managed_ips: string[];
  last_ip: string;
  last_status: string;
  last_error: string;
  whitelist_count: number;
  last_attempt_at: string | null;
  last_success_at: string | null;
};

export type XiequConfig = {
  enabled: boolean;
  configured: boolean;
  uid: string;
  ukey_set: boolean;
  base_url: string;
  memo: string;
  refresh_interval_seconds: number;
  proxy_domains: string[];
  max_whitelist_entries: number;
  quota_hint: string;
  auto_refresh_on_proxy_use: boolean;
  state: XiequWhitelistState;
};

export type XiequSettingsPatch = {
  enabled?: boolean;
  uid?: string;
  ukey?: string;
  base_url?: string;
  memo?: string;
  refresh_interval_seconds?: number;
  proxy_domains?: string[];
};

export type XiequRefreshResult = {
  ok: boolean;
  reason: string;
  message: string;
  public_ip?: string;
  whitelist_count: number;
  added: boolean;
  removed?: string[];
  managed_ips?: string[];
  checked_at: string;
  last_success_at?: string | null;
  warning?: string;
};

function stringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

function validTime(value: unknown): value is string | null {
  return value === null || (typeof value === "string" && Number.isFinite(Date.parse(value)));
}

function configResponse(value: unknown): XiequConfig {
  const config = value as Partial<XiequConfig> | null;
  if (
    !config ||
    typeof config.enabled !== "boolean" ||
    typeof config.configured !== "boolean" ||
    typeof config.uid !== "string" ||
    typeof config.ukey_set !== "boolean" ||
    typeof config.base_url !== "string" ||
    typeof config.memo !== "string" ||
    typeof config.refresh_interval_seconds !== "number" ||
    !Number.isInteger(config.refresh_interval_seconds) ||
    config.refresh_interval_seconds < 30 ||
    config.refresh_interval_seconds > 3600 ||
    !stringArray(config.proxy_domains) ||
    config.max_whitelist_entries !== 5 ||
    typeof config.quota_hint !== "string" ||
    typeof config.auto_refresh_on_proxy_use !== "boolean"
  ) {
    throw new Error("谐趣白名单配置响应无效，请重新读取");
  }
  const state = config.state as Partial<XiequWhitelistState> | undefined;
  if (
    !state ||
    !stringArray(state.managed_ips) ||
    typeof state.last_ip !== "string" ||
    typeof state.last_status !== "string" ||
    typeof state.last_error !== "string" ||
    typeof state.whitelist_count !== "number" ||
    !Number.isInteger(state.whitelist_count) ||
    !validTime(state.last_attempt_at) ||
    !validTime(state.last_success_at)
  ) {
    throw new Error("谐趣白名单状态响应无效，请重新读取");
  }
  return config as XiequConfig;
}

function refreshResponse(value: unknown): XiequRefreshResult {
  const result = value as Partial<XiequRefreshResult> | null;
  if (
    !result ||
    typeof result.ok !== "boolean" ||
    typeof result.reason !== "string" ||
    typeof result.message !== "string" ||
    typeof result.whitelist_count !== "number" ||
    typeof result.added !== "boolean" ||
    typeof result.checked_at !== "string" ||
    !Number.isFinite(Date.parse(result.checked_at))
  ) {
    throw new Error("谐趣白名单刷新响应无效，请重新读取");
  }
  return result as XiequRefreshResult;
}

export const xiequAPI = {
  config: () => http<unknown>("/integrations/xiequ", { cache: "no-store" }).then(configResponse),
  save: (patch: XiequSettingsPatch) =>
    http<unknown>("/integrations/xiequ", {
      method: "PUT",
      body: JSON.stringify(patch),
    }).then(configResponse),
  refresh: () =>
    http<unknown>("/integrations/xiequ/refresh", {
      method: "POST",
      body: JSON.stringify({}),
    }).then(refreshResponse),
};
