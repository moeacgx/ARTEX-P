// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: 意图 / 覆盖 / 任务 / 严重度 each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose 用作「严重」——实心高强调,视觉上明显高于「高危」的软红描边。
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "待领", tone: "slate" },
  running: { label: "执行中", tone: "blue" },
  paused: { label: "已暂停", tone: "amber" },
  done: { label: "已完成", tone: "green" },
  // blocked = 模型/API/网络故障重试用尽，这条意图基本没真正探成（非目标拦截）。
  blocked: { label: "执行出错", tone: "red" },
  // exhausted = 达到单次运行时间上限/步数上限，或任务超时收尾被中途结束，只写回部分结果（非方向已探尽，也与费用预算无关）；具体原因见 blocked_reason。
  exhausted: { label: "已达上限", tone: "violet" },
  // stopped = 历史软删除状态（保留,历史数据）。
  stopped: { label: "已停止", tone: "slate" },
  // deleted = 用户假删除了该意图（保留节点与血缘，删除原因见 delete_reason 字段）。
  deleted: { label: "已删除", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "已创建", tone: "slate" },
  queued: { label: "排队中", tone: "amber" },
  running: { label: "运行中", tone: "blue" },
  paused: { label: "已暂停", tone: "amber" },
  done: { label: "已完成", tone: "green" },
  failed: { label: "失败", tone: "red" },
  timeout: { label: "已超时", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "严重", tone: "rose" },
  high: { label: "高危", tone: "red" },
  medium: { label: "中危", tone: "amber" },
  low: { label: "低危", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "待处理", tone: "amber" },
  in_progress: { label: "处理中", tone: "blue" },
  confirmed: { label: "已确认", tone: "red" },
  resolved: { label: "已处理", tone: "green" },
  fixed: { label: "已修复", tone: "green" },
  false_positive: { label: "误报", tone: "slate" },
  ignored: { label: "忽略", tone: "neutral" },
  duplicate: { label: "重复", tone: "neutral" },
  risk_accepted: { label: "风险接受", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "探索中", tone: "blue" },
  paused: { label: "已暂停", tone: "amber" },
  stalled: { label: "停滞", tone: "red" },
  idle: { label: "空闲", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "进行中", tone: "blue" },
  met: { label: "已达成", tone: "green" },
  abandoned: { label: "已放弃", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "放行", tone: "green" },
  block: { label: "拦截", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "观测", tone: "slate" },
  confirmed: { label: "确认", tone: "green" },
  tombstoned: { label: "废弃", tone: "neutral" },
};

// 推送投递状态。sending 用 blue 而不是 amber：它不是「有问题」，
// 而是「已被领取、正在发」，与 pending 的等待语义要能区分开。
const delivery: Record<string, StatusMeta> = {
  pending: { label: "待发送", tone: "amber" },
  sending: { label: "发送中", tone: "blue" },
  sent: { label: "已送达", tone: "green" },
  failed: { label: "失败", tone: "red" },
  skipped: { label: "已跳过", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
  delivery,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}

// 意图终止原因（exploration_nodes.blocked_reason）→ 用户可读文案。
// 只有 llm_quota_exhausted 才有明确的上游额度证据，才允许出现“额度不足”；
// 401 / 限流 / 5xx / 网络错误一律不得写成余额问题；
// 任何文案都不得暗示“费用预算”——系统没有为意图设置费用预算。
export interface IntentStopReasonText {
  title: string;
  detail: string;
}

const intentStopReasons: Record<string, IntentStopReasonText> = {
  run_time_limit: {
    title: "达到单次运行时间上限",
    detail:
      "本次运行用满了智能体配置的单次运行时限。未单独配置时默认 1200 秒（20 分钟）；该值设为 0 表示明确不限时。这与费用预算无关，已写回的结果保留，可重跑继续。",
  },
  max_turns: {
    title: "达到最大步数上限",
    detail:
      "本次运行用满了智能体配置的最大步数。未配置步数上限时不限步数。这与费用预算无关，已写回的结果保留，可重跑继续。",
  },
  task_timeout_settle: {
    title: "任务超时后收尾结束",
    detail: "任务整体超时，系统在收尾阶段结束了这条意图，已写回的结果保留。这与费用预算无关。",
  },
  llm_quota_exhausted: {
    title: "模型额度不足",
    detail: "上游明确返回了额度或余额不足的信息，且所选模型配置已无可用项。请为该模型配置充值或更换后重跑。",
  },
  llm_auth_failed: {
    title: "模型认证失败",
    detail: "上游返回认证/授权错误（如 401、403），通常是密钥无效或已过期，并非余额问题。请检查模型配置的密钥。",
  },
  llm_rate_limited: {
    title: "模型请求被限流",
    detail: "上游返回限流（429）且没有明确的额度不足信息，并非余额问题。稍后重跑，或降低并发。",
  },
  llm_upstream_error: {
    title: "模型服务端错误",
    detail: "上游返回 5xx 服务端错误，重试后仍失败，并非余额问题。可稍后重跑。",
  },
  llm_response_timeout: {
    title: "等待模型响应超时",
    detail: "等待模型响应超时（网络或上游无响应），与单次运行时限、余额均无关。可稍后重跑。",
  },
  llm_chain_unavailable: {
    title: "所选模型配置均不可用",
    detail:
      "任务所选的模型配置都已被标记为不可用（可能是此前的额度失败或探测失败），这里没有明确的额度不足证据，不能认定为余额问题。请检查并重置模型配置后重跑。",
  },
  llm_error: {
    title: "调用模型出错",
    detail: "调用模型时出现 API 或网络错误，重试后仍失败，未发现明确的额度不足证据。请查看运行日志确认具体原因。",
  },
};

// intentStopReason 返回意图终止原因的具体文案。
// 没有原因代码时（历史数据，或未记录原因的 blocked）只给诚实的中性说明，不推断原因。
export function intentStopReason(state: string, reason?: string | null): IntentStopReasonText | null {
  if (state !== "blocked" && state !== "exhausted") return null;
  const code = (reason ?? "").trim();
  if (code && intentStopReasons[code]) return intentStopReasons[code];
  if (state === "exhausted") {
    return {
      title: "已达运行上限",
      detail:
        "这是更新前产生的记录，未保存具体终止原因，只能确定运行被时间上限、步数上限或任务超时收尾中断。这不代表费用预算耗尽或余额不足。",
    };
  }
  return {
    title: "执行出错",
    detail: "这条记录未保存具体终止原因，无法判断是认证、限流、网络还是其他错误，也不能据此认定为余额不足。",
  };
}
