import { http } from "./api";
import { validRevision } from "./proxy-policy";
import type { GlobalWarpHealthPolicy } from "./warp-health";

function checkedPolicy(value: GlobalWarpHealthPolicy): GlobalWarpHealthPolicy {
  if (
    value.scope !== "global" ||
    !validRevision(value.revision) ||
    value.revision === 0 ||
    typeof value.auto_recover_enabled !== "boolean" ||
    !Number.isSafeInteger(value.migrated_disabled_nodes) ||
    value.migrated_disabled_nodes < 0 ||
    value.probe_interval_seconds !== 60 ||
    value.failure_threshold !== 3 ||
    value.recovery_min_seconds !== 300 ||
    value.recovery_max_seconds !== 3600
  )
    throw new Error("全局健康策略回读范围、版本或内容无效");
  return value;
}

export const globalWarpHealthAPI = {
  get: async () =>
    checkedPolicy(await http<GlobalWarpHealthPolicy>("/proxies/warp-health-policy", { cache: "no-store" })),
  save: (revision: number, enabled: boolean) => {
    if (!validRevision(revision) || revision === 0 || typeof enabled !== "boolean")
      throw new Error("缺少有效全局健康策略版本或开关状态");
    return http<GlobalWarpHealthPolicy>(
      "/proxies/warp-health-policy",
      { method: "PUT", body: JSON.stringify({ expected_revision: revision, auto_recover_enabled: enabled }) },
      true,
    ).then(checkedPolicy);
  },
};
export type GlobalWarpHealthAdapter = typeof globalWarpHealthAPI;
