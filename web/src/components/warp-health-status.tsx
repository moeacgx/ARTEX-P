"use client";

import { Badge } from "@/components/ui/badge";
import { proxyErrorText } from "@/lib/proxy-center";
import type { ProxyRecord, WarpHealth } from "@/lib/types";
import { warpEffectiveIP } from "@/lib/warp-health";

const labels: Record<WarpHealth["state"], string> = {
  unknown: "等待确认上次执行",
  stopped: "已停止",
  disabled: "已停用",
  pending: "创建或注册待确认",
  checking: "正在检测",
  ready: "已验证在线",
  failed: "连接故障",
  deferred: "等待安全空闲",
  repairing: "正在恢复原出口",
};
const time = (value?: string) =>
  value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : "尚未发生";
export function WarpHealthStatus({ proxy }: { proxy: ProxyRecord }) {
  const health = proxy.warp_health,
    ip = warpEffectiveIP(proxy);
  let desiredLabel = "未确认";
  if (health) desiredLabel = health.desired_running ? "保持运行" : "不自动启动";
  return (
    <div className="w-full space-y-3 rounded-lg border bg-muted/20 p-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <p className="font-medium text-sm">运行详情</p>
        <Badge variant={health?.state === "failed" || health?.state === "unknown" ? "destructive" : "secondary"}>
          {health ? labels[health.state] : "健康信息尚未返回"}
        </Badge>
      </div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
        <div>
          <dt className="text-muted-foreground">真实有效出口 IP</dt>
          <dd className="break-all font-mono">{ip ?? "未验证"}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">运行设置</dt>
          <dd>{desiredLabel}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">最近探测</dt>
          <dd>{time(health?.last_checked_at)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">下次检测</dt>
          <dd>{time(health?.next_probe_at)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">连续失败 / 恢复尝试</dt>
          <dd>{health ? `${health.consecutive_failures} / ${health.recovery_attempts}` : "—"}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">下次恢复</dt>
          <dd>{health?.auto_recover_enabled === false ? "自动恢复已关闭" : time(health?.next_recovery_at)}</dd>
        </div>
      </dl>
      {health?.last_error && <p className="text-destructive text-xs">{proxyErrorText(health.last_error)}</p>}
      {(health?.state === "unknown" || health?.state === "pending") && (
        <p className="text-muted-foreground text-xs">
          上次操作尚未确认；确认执行已退出后，系统将按全局策略继续恢复。无法确认时会保留阻塞原因。
        </p>
      )}
      {health?.state === "deferred" && (
        <p className="text-muted-foreground text-xs">
          {health.auto_recover_enabled === false
            ? "自动恢复已关闭；系统仍会检测连接状态。"
            : "恢复条件尚未满足，系统将在满足全局策略和安全空闲条件后重试。"}
        </p>
      )}
    </div>
  );
}
