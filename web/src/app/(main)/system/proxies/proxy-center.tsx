"use client";

import * as React from "react";

import { ChevronDown, Plus, RefreshCw, Search } from "lucide-react";
import { toast } from "sonner";

import { GlobalWarpHealthControl } from "@/components/global-warp-health-control";
import { ProxyGroups } from "@/components/proxy-groups";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { WarpHealthStatus } from "@/components/warp-health-status";
import { APIError, api } from "@/lib/api";
import { MOCK } from "@/lib/mock/enabled";
import {
  buildProxyPayload,
  proxyEndpoint as endpoint,
  proxyErrorText as errorText,
  type ProxyForm as Form,
  proxyHealthStatus as listStatus,
  proxyFromFailure,
  proxyProbeResult,
  proxySelectable as selectable,
  WARP_PHASE_LABEL,
  WARP_REGISTER_TERMS,
  WARP_TERMS_URL,
  warpActionNeedsTerms,
  warpAllowedActions,
  warpReady,
} from "@/lib/proxy-center";
import type { ProxyKind, ProxyRecord } from "@/lib/types";
import { WarpCreateIntent, type WarpCreateStage, type WarpIntent } from "@/lib/warp-create-intent";
import { warpCreateFailure } from "@/lib/warp-create-result";
import { warpEffectiveIP } from "@/lib/warp-health";

const KINDS: Array<{ value: ProxyKind; label: string }> = [
  { value: "http", label: "HTTP" },
  { value: "https", label: "HTTPS" },
  { value: "socks5", label: "SOCKS5" },
  { value: "warp", label: "轻量 WARP" },
  { value: "resin", label: "Resin 动态代理" },
];
const STATUS: Record<string, string> = {
  unknown: "未检测",
  healthy: "可用",
  failed: "失败",
  starting: "启动中",
  stopped: "已停止",
};
const freshForm = (): Form => ({
  name: "",
  kind: "http",
  protocol: "socks5",
  url: "",
  host: "",
  port: "",
  password: "",
  platform: "Default",
  adminURL: "",
  adminToken: "",
  enabled: true,
  clearPassword: false,
  clearAdminToken: false,
});
function badgeVariant(status: string): "default" | "secondary" | "destructive" {
  if (status === "failed") return "destructive";
  if (status === "healthy") return "default";
  return "secondary";
}

export function ProxyCenter() {
  const [items, setItems] = React.useState<ProxyRecord[]>([]);
  const [globalID, setGlobalID] = React.useState<string | null>(null);
  const [form, setForm] = React.useState<Form>(freshForm);
  const [formOpen, setFormOpen] = React.useState(false);
  const [groupsOpen, setGroupsOpen] = React.useState(false);
  const [detailID, setDetailID] = React.useState<string | null>(null);
  const [query, setQuery] = React.useState("");
  const [kindFilter, setKindFilter] = React.useState("all");
  const [statusFilter, setStatusFilter] = React.useState("all");
  const [editing, setEditing] = React.useState<ProxyRecord | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState("");
  const [busy, setBusy] = React.useState("");
  const [deleting, setDeleting] = React.useState<ProxyRecord | null>(null);
  const [releasing, setReleasing] = React.useState<string | null>(null);
  const [releaseReadback, setReleaseReadback] = React.useState<string[]>([]);
  const releaseSubmitted = React.useRef(new Set<string>());
  const mutationLock = React.useRef(false);
  const [saveError, setSaveError] = React.useState("");
  const [warpCreation] = React.useState(() => new WarpCreateIntent());
  const [createIntent, setCreateIntent] = React.useState<WarpIntent | null>(null);
  const [createStage, setCreateStage] = React.useState<WarpCreateStage>("draft");
  const [intentStorageError, setIntentStorageError] = React.useState("正在读取本地 WARP 意图…");
  const [pendingID, setPendingID] = React.useState<string | null>(null);
  const [pollAttempt, setPollAttempt] = React.useState(0);
  const pollLock = React.useRef(false);
  const [registrationUncertain, setRegistrationUncertain] = React.useState<string[]>([]);
  // WARP 注册条款确认：registering 记录待确认的代理编号；acceptTerms 必须由用户
  // 手动勾选（每次打开都重置为 false，绝不预选）。
  const [registering, setRegistering] = React.useState<string | null>(null);
  const [acceptTerms, setAcceptTerms] = React.useState(false);

  const restoreIntent = React.useCallback(() => {
    try {
      const intent = warpCreation.restore();
      setCreateIntent(intent);
      setCreateStage(warpCreation.stage);
      setIntentStorageError("");
      if (intent) setForm({ ...freshForm(), kind: "warp", name: intent.name });
    } catch {
      setIntentStorageError("无法读取本地 WARP 意图，已暂停新增。请重试读取并核对原代理，勿重复新建。");
    }
  }, [warpCreation]);
  React.useEffect(restoreIntent, [restoreIntent]);

  const finishCreation = React.useCallback(
    (stage: WarpCreateStage) => {
      warpCreation.finish(stage);
      setCreateStage(stage);
      setCreateIntent(warpCreation.intent);
    },
    [warpCreation],
  );

  const newWarpIntent = () => {
    if (mutationLock.current) return;
    try {
      if (!warpCreation.newIntent()) return;
      setCreateIntent(null);
      setCreateStage("draft");
      setPendingID(null);
      setEditing(null);
      setForm({ ...freshForm(), kind: "warp" });
      setSaveError("");
      setIntentStorageError("");
    } catch {
      setIntentStorageError("无法清理原意图，已保留原请求编号。请重试读取本地存储。");
    }
  };

  const reload = React.useCallback(async () => {
    const result = await api.proxies();
    setItems(result.proxies ?? []);
    setGlobalID(result.global_proxy_id ?? null);
    setLoadError("");
    return result;
  }, []);
  React.useEffect(() => {
    let active = true;
    api
      .proxies()
      .then((result) => {
        if (!active) return;
        setItems(result.proxies ?? []);
        setGlobalID(result.global_proxy_id ?? null);
      })
      .catch((error) => {
        if (active) setLoadError(errorText(error));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);
  React.useEffect(() => {
    if (createStage !== "pending" || !createIntent || pollAttempt === 0) return;
    let active = true;
    let timer: ReturnType<typeof setTimeout>;
    let attempts = 0;
    const intent = createIntent;
    const poll = async () => {
      if (pollLock.current) {
        timer = setTimeout(() => void poll(), 2000);
        return;
      }
      pollLock.current = true;
      try {
        // 202 或结果未知后只 GET 原条目；运行健康不等同于创建意图已持久化完成。
        if (pendingID === null) {
          await reload();
          if (active) setSaveError("原创建请求尚无已确认编号。已刷新目录；不能按名称推断成功或再次注册。");
          return;
        }
        const item = await api.proxy(pendingID);
        if (!active || !warpCreation.isCurrent(intent)) return;
        if (pendingID !== null && item.id !== pendingID) throw new Error("返回的代理编号不一致");
        setItems((prev) => [item, ...prev.filter((entry) => entry.id !== item.id)]);
        if (warpReady(item)) {
          setSaveError(`WARP #${item.id} 当前已验证在线。原创建意图完成状态仍待服务端核对，不重发创建。`);
          return;
        }
        setSaveError(`WARP #${item.id} 已建档，当前未连接；请核对原条目，不会重新注册。`);
        if (++attempts < 30) timer = setTimeout(() => void poll(), 2000);
      } catch (error) {
        if (!active || !warpCreation.isCurrent(intent)) return;
        const result = warpCreateFailure(error);
        if (result.stage === "pending") {
          if (result.id !== null) setPendingID(result.id);
          const proxy = result.proxy;
          if (proxy) setItems((prev) => [proxy, ...prev.filter((item) => item.id !== proxy.id)]);
          if (++attempts < 30) timer = setTimeout(() => void poll(), 2000);
          else setSaveError("原请求仍在处理中，可继续核对进度；不会重新注册。");
        } else if (result.stage === "retained") {
          finishCreation("retained");
          setEditing(result.proxy);
          setSaveError(`WARP #${result.proxy.id} 已保留：${errorText(error)}。请恢复此条目。`);
        } else if (result.stage === "deleted" || result.stage === "conflict") {
          finishCreation(result.stage);
          setSaveError(`原请求已删除或冲突：${errorText(error)}。不会自动更换请求编号。`);
        } else setSaveError(`原请求状态暂时无法确认，可继续核对进度：${errorText(error)}`);
      } finally {
        pollLock.current = false;
      }
    };
    void poll();
    return () => {
      active = false;
      clearTimeout(timer);
    };
  }, [createStage, pendingID, pollAttempt, finishCreation, warpCreation, createIntent, reload]);
  const reset = () => {
    if (mutationLock.current) return;
    setEditing(null);
    setForm(createIntent ? { ...freshForm(), kind: "warp", name: createIntent.name } : freshForm());
    setSaveError("");
  };
  const edit = (item: ProxyRecord) => {
    if (mutationLock.current) return;
    if (createStage === "pending" && pendingID === item.id) return;
    setEditing(item);
    setSaveError("");
    setForm({
      name: item.name,
      kind: item.kind,
      protocol: item.protocol === "http" ? "http" : "socks5",
      // 脱敏 URL 绝不能被当作真实凭据回写；留空让后端保留旧值。
      url: "",
      host: item.host ?? "",
      port: item.port ? String(item.port) : "",
      password: "",
      platform: item.resin_platform ?? "Default",
      adminURL: item.resin_admin_url ?? "",
      adminToken: "",
      enabled: item.enabled,
      clearPassword: false,
      clearAdminToken: false,
    });
    setFormOpen(true);
  };

  async function refreshAfterMutation() {
    try {
      await reload();
    } catch (error) {
      setLoadError(errorText(error));
      toast.error(`操作已返回，但最新状态加载失败：${errorText(error)}。请刷新核对。`);
    }
  }

  async function save() {
    if (mutationLock.current) return;
    if (editing && createStage === "pending" && pendingID === editing.id) return;
    const creatingWarp = !editing && form.kind === "warp";
    let intent: WarpIntent | null = null;
    let data: ReturnType<typeof buildProxyPayload>;
    try {
      data = buildProxyPayload(form, !!editing);
      if (creatingWarp) {
        if (intentStorageError || createStage !== "draft") return;
        intent = warpCreation.begin(form.name, data.accept_terms === true);
        if (!intent) return;
        setCreateIntent(intent);
        data = { name: intent.name, kind: "warp", accept_terms: intent.accept_terms };
      }
    } catch (error) {
      if (creatingWarp && !warpCreation.storageReady) setIntentStorageError(errorText(error));
      toast.error(errorText(error));
      return;
    }
    mutationLock.current = true;
    setBusy("save");
    setSaveError("");
    try {
      const result = editing ? await api.updateProxy(editing.id, data) : await api.createProxy(data, intent?.key);
      if (intent && !warpCreation.isCurrent(intent)) return;
      if (creatingWarp && !warpReady(result)) {
        finishCreation("retained");
        setEditing(result);
        setSaveError(`WARP #${result.id} 已建档，但尚未连接成功。请核对状态并恢复该条目。`);
      } else {
        let message = "代理已加入中心";
        if (editing) message = "代理配置已保存";
        else if (form.kind === "warp") message = `WARP 已连接 · 出口 ${warpEffectiveIP(result) ?? "未验证"}`;
        toast.success(message);
        if (creatingWarp) {
          finishCreation("complete");
          setPendingID(result.id);
          setForm({ ...freshForm(), kind: "warp", name: data.name });
        } else {
          setForm(freshForm());
          setFormOpen(false);
        }
        setEditing(null);
      }
      await refreshAfterMutation();
    } catch (error) {
      if (intent && !warpCreation.isCurrent(intent)) return;
      const retained = proxyFromFailure(error);
      const status = error instanceof APIError ? error.status : undefined;
      const failure = warpCreateFailure(error);
      if (creatingWarp && failure.stage === "pending") {
        finishCreation("pending");
        setPendingID(failure.id);
        setPollAttempt((value) => value + 1);
        setSaveError(`WARP ${failure.id ? `#${failure.id} ` : ""}正在添加 / 连接，正在核对原请求。`);
      } else if (creatingWarp && (status === 409 || status === 410)) {
        finishCreation(status === 410 ? "deleted" : "conflict");
        setSaveError(
          status === 410
            ? "原请求对应的代理已删除，此请求编号不能再次创建。需要新出口时请选择新增另一个。"
            : "原请求与服务端记录冲突，请先核对列表。不会自动更换请求编号重新创建。",
        );
      } else if (retained) {
        if (creatingWarp) finishCreation("retained");
        setEditing(retained);
        setItems((prev) => [retained, ...prev.filter((item) => item.id !== retained.id)]);
        setSaveError(`WARP #${retained.id} 已保留：${errorText(error)}。请恢复此条目，勿重复添加。`);
      } else {
        if (creatingWarp) finishCreation("uncertain");
        setSaveError(
          creatingWarp
            ? `添加结果待确认：${errorText(error)}。已保留原请求编号，只刷新核对，不重发创建或注册。`
            : `保存失败：${errorText(error)}`,
        );
      }
      if (status !== 202) toast.error(errorText(error));
    } finally {
      mutationLock.current = false;
      setBusy("");
    }
  }
  async function choose(id: string | null) {
    setBusy("global");
    try {
      await api.setGlobalProxy(id);
      toast.success(id === null ? "全局默认已设为直连" : `全局默认已设为代理 #${id}`);
      await refreshAfterMutation();
    } catch (error) {
      toast.error(`设置失败：${errorText(error)}`);
    } finally {
      setBusy("");
    }
  }
  async function test(id: string) {
    setBusy(`test:${id}`);
    try {
      const current = items.find((item) => item.id === id);
      if (!current) throw new Error("代理记录不存在，请刷新列表");
      const result = await api.testProxy(id);
      const outcome = proxyProbeResult(current, result);
      const latest = await reload();
      const fresh = latest.proxies.find((item) => item.id === id);
      if (!fresh) throw new Error("检测后代理记录已改变，无法确认结果");
      const confirmed = proxyProbeResult(current, fresh);
      if (outcome.available && confirmed.available) toast.success(`代理 #${id} ${confirmed.message}`);
      else toast.info(`代理 #${id} ${outcome.available ? confirmed.message : outcome.message}`);
    } catch (error) {
      try {
        await reload();
      } catch {
        /* 保留检测错误 */
      }
      toast.error(`代理 #${id} 检测失败：${errorText(error)}`);
    } finally {
      setBusy("");
    }
  }
  // 注册必须由用户先确认条款：只有确认后才会带着 accept_terms:true 调用；
  // start/stop/recover 不涉及条款，也不带请求体。
  async function warp(id: string, action: "register" | "start" | "stop" | "recover") {
    if (mutationLock.current) return;
    if (createStage === "pending" && pendingID === id) return;
    const current = items.find((item) => item.id === id) ?? (editing?.id === id ? editing : null);
    if (
      !current ||
      !warpAllowedActions(current).includes(action) ||
      (action === "register" && registrationUncertain.includes(id))
    )
      return;
    if (warpActionNeedsTerms(action) && acceptTerms !== true) return;
    mutationLock.current = true;
    if (action === "register") setRegistrationUncertain((prev) => [...prev, id]);
    setBusy(`warp:${id}`);
    try {
      const result = await api.proxyWarpAction(id, action, warpActionNeedsTerms(action) ? true : undefined);
      setItems((prev) => prev.map((item) => (item.id === id ? result : item)));
      if (editing?.id === id) setEditing(result);
      await refreshAfterMutation();
      if (warpReady(result)) {
        toast.success(`WARP #${id} 已连接 · 出口 ${warpEffectiveIP(result) ?? "未验证"}`);
        if (editing?.id === id) setSaveError("");
      } else toast.info(`WARP #${id} 当前状态：${WARP_PHASE_LABEL[result.warp_phase ?? ""] ?? "状态待核对"}`);
      setRegistering(null);
      setAcceptTerms(false);
    } catch (error) {
      const retained = proxyFromFailure(error);
      if (retained) {
        setItems((prev) => prev.map((item) => (item.id === id ? retained : item)));
        if (editing?.id === id) setEditing(retained);
      }
      try {
        await reload();
      } catch {
        /* 保留操作错误 */
      }
      toast.error(`WARP #${id} 操作失败：${errorText(error)}`);
      // 注册失败或响应丢失均不重注册；后续仅按服务端 phase 恢复原 ID。
      setRegistering(null);
      setAcceptTerms(false);
    } finally {
      mutationLock.current = false;
      setBusy("");
    }
  }
  async function release(id: string) {
    if (mutationLock.current) return;
    mutationLock.current = true;
    setBusy(`resin:${id}`);
    try {
      if (!releaseSubmitted.current.has(id)) {
        releaseSubmitted.current.add(id);
        setReleaseReadback((previous) => [...previous, id]);
        await api.releaseResin(id);
      }
      await refreshAfterMutation();
      toast.info(`Resin #${id} 原请求状态已刷新，请核对运行状态；不会重复释放`);
      setReleasing(null);
    } catch (error) {
      toast.error(`Resin #${id} 释放待核对：${errorText(error)}。仅刷新，不重发释放请求。`);
    } finally {
      mutationLock.current = false;
      setBusy("");
    }
  }
  async function remove(item: ProxyRecord) {
    if (mutationLock.current) return;
    mutationLock.current = true;
    setBusy(`delete:${item.id}`);
    try {
      await api.deleteProxy(item.id);
      mutationLock.current = false;
      if (editing?.id === item.id) reset();
      setDeleting(null);
      toast.success(`代理 #${item.id} 已删除`);
      await refreshAfterMutation();
    } catch (error) {
      toast.error(`删除失败：${errorText(error)}`);
    } finally {
      mutationLock.current = false;
      setBusy("");
    }
  }

  const global = items.find((item) => item.id === globalID);
  const creatingWarp = !editing && form.kind === "warp";
  let formDescription = "配置代理名称、类型和连接信息。";
  if (editing) formDescription = "修改连接配置；秘密字段留空表示保留。";
  if (editing && form.kind === "warp") formDescription = "修改代理名称和启用状态。";
  if (creatingWarp) formDescription = "输入名称并确认添加，系统自动注册、启动和验证出口。";
  const creationBlocked = creatingWarp && (!!intentStorageError || createStage !== "draft");
  let saveLabel = "添加代理";
  if (!editing && form.kind === "warp") saveLabel = "确认添加（同意条款）";
  if (creatingWarp && createStage === "uncertain") saveLabel = "原请求结果待核对";
  if (creatingWarp && createStage === "complete") saveLabel = "已完成添加";
  if (creatingWarp && createStage === "pending") saveLabel = "正在添加 / 连接…";
  if (creatingWarp && createStage === "deleted") saveLabel = "原代理已删除";
  if (creatingWarp && createStage === "conflict") saveLabel = "请核对原请求";
  if (creatingWarp && createStage === "retained") saveLabel = "已保留原代理";
  if (editing) saveLabel = "保存修改";
  if (busy === "save") saveLabel = "保存中…";
  if (busy === "save" && form.kind === "warp" && !editing) saveLabel = "正在添加 / 连接…";
  let globalLabel = "直连";
  if (globalID) globalLabel = `#${globalID}（记录未返回）`;
  if (global) globalLabel = `#${global.id} ${global.name}`;
  if (loading || loadError) globalLabel = "正在核对或状态未知";
  const detail = items.find((item) => item.id === detailID);
  let emptyLabel = items.length ? "没有符合条件的代理" : "暂无代理，点击“新增代理”添加";
  if (loading) emptyLabel = "正在加载代理…";
  const counts = {
    all: items.length,
    healthy: items.filter((item) => item.enabled && listStatus(item) === "healthy").length,
    failed: items.filter((item) => listStatus(item) === "failed").length,
  };
  const visibleItems = items.filter((item) => {
    if (kindFilter !== "all" && item.kind !== kindFilter) return false;
    if (statusFilter === "healthy" && (!item.enabled || listStatus(item) !== "healthy")) return false;
    if (statusFilter === "failed" && listStatus(item) !== "failed") return false;
    const text =
      `${item.id} ${item.name} ${endpoint(item)} ${item.kind === "warp" ? (warpEffectiveIP(item) ?? "") : (item.egress_ip ?? "")}`.toLowerCase();
    return text.includes(query.trim().toLowerCase());
  });
  return (
    <div className="flex flex-1 flex-col gap-4">
      <div>
        <div className="flex items-center gap-2">
          <h1 className="font-semibold text-xl tracking-tight">代理中心</h1>
          {MOCK && <Badge variant="secondary">演示模式 · 不执行真实检测</Badge>}
        </div>
        <p className="text-muted-foreground text-sm">管理代理出口、分组与全局恢复策略。</p>
      </div>
      <div className="flex flex-wrap items-center gap-2 rounded-lg border bg-card p-3">
        <Button
          size="sm"
          disabled={!!busy}
          onClick={() => {
            reset();
            setFormOpen(true);
          }}
        >
          <Plus className="size-4" />
          新增代理
        </Button>
        <GlobalWarpHealthControl
          onUpdated={() => {
            void refreshAfterMutation();
          }}
        />
        <Button size="sm" variant="outline" onClick={() => setGroupsOpen(true)}>
          管理分组
        </Button>
        <Button
          size="sm"
          variant="outline"
          disabled={!!busy || loading}
          onClick={() => {
            setLoading(true);
            void reload()
              .catch((error) => setLoadError(errorText(error)))
              .finally(() => setLoading(false));
          }}
        >
          <RefreshCw className={loading ? "size-4 animate-spin" : "size-4"} />
          刷新
        </Button>
        <span className="ml-auto text-muted-foreground text-xs" title="更改仅影响之后创建的任务快照">
          全局默认：{globalLabel}
        </span>
        <Button
          size="sm"
          variant="ghost"
          disabled={!!busy || loading || !!loadError || globalID === null}
          onClick={() => void choose(null)}
        >
          改为直连
        </Button>
      </div>
      <Dialog
        open={formOpen}
        onOpenChange={(open) => {
          if (!mutationLock.current) setFormOpen(open);
        }}
      >
        <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editing ? `编辑代理 #${editing.id}` : "新增代理"}</DialogTitle>
            <DialogDescription>{formDescription}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 md:grid-cols-2">
            <div className="grid gap-2">
              <Label htmlFor="proxy-name">名称</Label>
              <Input
                id="proxy-name"
                disabled={!!busy || (creatingWarp && !!createIntent)}
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="proxy-kind">类型</Label>
              <select
                id="proxy-kind"
                className="h-10 rounded-md border border-input bg-background px-3 text-sm"
                value={form.kind}
                disabled={!!editing || !!busy || (creatingWarp && !!createIntent)}
                onChange={(e) => setForm({ ...form, kind: e.target.value as ProxyKind })}
              >
                {KINDS.map((kind) => (
                  <option key={kind.value} value={kind.value}>
                    {kind.label}
                  </option>
                ))}
              </select>
            </div>
            {form.kind !== "warp" && form.kind !== "resin" && (
              <div className="grid gap-2 md:col-span-2">
                <Label htmlFor="proxy-url">代理 URL {editing ? "（留空保留原地址）" : ""}</Label>
                {editing && <p className="break-all text-muted-foreground text-xs">当前：{endpoint(editing)}</p>}
                <Input
                  id="proxy-url"
                  autoComplete="off"
                  value={form.url}
                  onChange={(e) => setForm({ ...form, url: e.target.value })}
                  placeholder={`${form.kind}://user@host:port`}
                />
                <Label htmlFor="ordinary-password">认证密码 {editing?.has_password ? "（已保存）" : ""}</Label>
                <Input
                  id="ordinary-password"
                  type="password"
                  autoComplete="new-password"
                  value={form.password}
                  disabled={form.clearPassword}
                  onChange={(e) => setForm({ ...form, password: e.target.value })}
                  placeholder={editing ? "留空保留原密码" : "可选，也可包含在 URL 中"}
                />
                {editing && (
                  <label className="flex gap-2 text-xs">
                    <input
                      type="checkbox"
                      checked={form.clearPassword}
                      onChange={(e) => setForm({ ...form, clearPassword: e.target.checked, password: "" })}
                    />
                    明确清除认证密码
                  </label>
                )}
              </div>
            )}
            {form.kind === "warp" && (
              <div className="space-y-2 text-muted-foreground text-sm md:col-span-2">
                {!editing && (
                  <p>
                    点击“确认添加”表示同意{" "}
                    <a href={WARP_TERMS_URL} target="_blank" rel="noreferrer" className="underline">
                      Cloudflare WARP 服务条款
                    </a>
                    ，并申请注册。轻量 WARP 提供本机 SOCKS5 出口。
                  </p>
                )}
                {creatingWarp && createStage === "uncertain" && (
                  <p>已保留「{createIntent?.name}」的原 UUID 添加意图。结果未知时仅可核对列表，不再次创建或注册。</p>
                )}
                {creatingWarp && createStage === "complete" && (
                  <p role="status">WARP #{pendingID} 已完成添加。需要另一个出口时，请点击“新增另一个”。</p>
                )}
                {intentStorageError && (
                  <div role="alert" className="space-y-2 text-destructive">
                    <p>{intentStorageError}</p>
                    <Button variant="outline" disabled={!!busy} onClick={restoreIntent}>
                      重试读取本地意图
                    </Button>
                  </div>
                )}
                {editing && (
                  <p>
                    代理 #{editing.id} · {WARP_PHASE_LABEL[editing.warp_phase ?? ""] ?? "状态待核对"} · 出口 IP：
                    {warpEffectiveIP(editing) ?? "未验证"}
                  </p>
                )}
              </div>
            )}
            {form.kind === "resin" && (
              <>
                <div className="grid gap-2">
                  <Label htmlFor="proxy-host">数据面 Host</Label>
                  <Input
                    id="proxy-host"
                    value={form.host}
                    onChange={(e) => setForm({ ...form, host: e.target.value })}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="proxy-port">数据面 Port</Label>
                  <Input
                    id="proxy-port"
                    inputMode="numeric"
                    value={form.port}
                    onChange={(e) => setForm({ ...form, port: e.target.value })}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="proxy-protocol">数据面协议</Label>
                  <select
                    id="proxy-protocol"
                    className="h-10 rounded-md border border-input bg-background px-3 text-sm"
                    value={form.protocol}
                    onChange={(e) => setForm({ ...form, protocol: e.target.value as Form["protocol"] })}
                  >
                    <option value="socks5">SOCKS5</option>
                    <option value="http">HTTP</option>
                  </select>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="resin-platform">Platform</Label>
                  <Input
                    id="resin-platform"
                    value={form.platform}
                    onChange={(e) => setForm({ ...form, platform: e.target.value })}
                  />
                  <p className="text-muted-foreground text-xs">任务 Account 按任务编号生成，不在此设置。</p>
                </div>
                <div className="grid gap-2 md:col-span-2">
                  <Label htmlFor="resin-admin-url">Admin URL（可选）</Label>
                  <Input
                    id="resin-admin-url"
                    value={form.adminURL}
                    onChange={(e) => setForm({ ...form, adminURL: e.target.value })}
                    placeholder="https://resin.example.com"
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="proxy-password">Proxy Token {editing?.has_password ? "（已保存）" : ""}</Label>
                  <Input
                    id="proxy-password"
                    type="password"
                    autoComplete="new-password"
                    value={form.password}
                    disabled={form.clearPassword}
                    onChange={(e) => setForm({ ...form, password: e.target.value })}
                    placeholder={editing ? "留空保留" : "输入 Token"}
                  />
                  {editing && (
                    <label className="flex gap-2 text-xs">
                      <input
                        type="checkbox"
                        checked={form.clearPassword}
                        onChange={(e) => setForm({ ...form, clearPassword: e.target.checked, password: "" })}
                      />
                      明确清除 Proxy Token
                    </label>
                  )}
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="resin-admin-token">
                    Admin Token {editing?.has_resin_admin_token ? "（已保存）" : ""}
                  </Label>
                  <Input
                    id="resin-admin-token"
                    type="password"
                    autoComplete="new-password"
                    value={form.adminToken}
                    disabled={form.clearAdminToken}
                    onChange={(e) => setForm({ ...form, adminToken: e.target.value })}
                    placeholder="留空保留（新建可选）"
                  />
                  {editing && (
                    <label className="flex gap-2 text-xs">
                      <input
                        type="checkbox"
                        checked={form.clearAdminToken}
                        onChange={(e) => setForm({ ...form, clearAdminToken: e.target.checked, adminToken: "" })}
                      />
                      明确清除 Admin Token
                    </label>
                  )}
                </div>
              </>
            )}
            {(form.kind !== "warp" || editing) && (
              <div className="flex items-center gap-3 md:col-span-2">
                <Switch
                  id="proxy-enabled"
                  checked={form.enabled}
                  onCheckedChange={(enabled) => setForm({ ...form, enabled })}
                />
                <Label htmlFor="proxy-enabled">启用此代理</Label>
              </div>
            )}
            {saveError && (
              <p role="alert" className="text-destructive text-sm md:col-span-2">
                {saveError}
              </p>
            )}
            <div className="flex gap-2 md:col-span-2">
              <Button disabled={!!busy || creationBlocked} onClick={() => void save()}>
                {saveLabel}
              </Button>
              {form.kind === "warp" && createIntent && (
                <Button
                  variant="outline"
                  disabled={!!busy || createStage === "pending" || createStage === "uncertain" || !!intentStorageError}
                  onClick={newWarpIntent}
                >
                  新增另一个
                </Button>
              )}
              {creatingWarp && createStage === "pending" && (
                <Button variant="outline" disabled={!!busy} onClick={() => setPollAttempt((value) => value + 1)}>
                  核对进度 {pendingID ? `#${pendingID}` : ""}
                </Button>
              )}
              {editing && (
                <Button variant="outline" disabled={!!busy} onClick={() => setFormOpen(false)}>
                  取消编辑
                </Button>
              )}
            </div>
          </div>
        </DialogContent>
      </Dialog>
      <Card className="gap-0 py-0">
        <CardContent className="p-0">
          <div className="flex flex-wrap items-center justify-between gap-3 px-4 pt-3 pb-2">
            <fieldset className="flex gap-4" aria-label="代理状态筛选">
              {(
                [
                  ["all", "全部"],
                  ["healthy", "可用"],
                  ["failed", "异常"],
                ] as const
              ).map(([value, label]) => (
                <button
                  type="button"
                  key={value}
                  aria-pressed={statusFilter === value}
                  onClick={() => setStatusFilter(value)}
                  className={`border-b-2 py-2 text-sm ${statusFilter === value ? "border-primary font-medium text-primary" : "border-transparent text-muted-foreground"}`}
                >
                  {label}
                  <span className="ml-1.5 text-xs tabular-nums">{loading || loadError ? "—" : counts[value]}</span>
                </button>
              ))}
            </fieldset>
            <div className="flex flex-wrap items-center gap-2">
              <div className="relative">
                <Search className="absolute top-2.5 left-2.5 size-3.5 text-muted-foreground" />
                <Input
                  aria-label="搜索代理"
                  placeholder="搜索名称、地址或 IP"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  className="h-8 w-52 pl-8 text-xs"
                />
              </div>
              <select
                aria-label="代理类型筛选"
                value={kindFilter}
                onChange={(event) => setKindFilter(event.target.value)}
                className="h-8 rounded-md border bg-background px-2 text-xs"
              >
                <option value="all">全部类型</option>
                {KINDS.map((kind) => (
                  <option key={kind.value} value={kind.value}>
                    {kind.label}
                  </option>
                ))}
              </select>
            </div>
          </div>
          <Table className="min-w-[900px] text-xs">
            <TableHeader className="bg-muted/40">
              <TableRow>
                <TableHead className="pl-4">名称</TableHead>
                <TableHead>类型 / 协议</TableHead>
                <TableHead>代理地址</TableHead>
                <TableHead>公网出口 IP</TableHead>
                <TableHead>状态</TableHead>
                <TableHead>最近检测</TableHead>
                <TableHead className="pr-4 text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(loading || loadError || visibleItems.length === 0) && (
                <TableRow>
                  <TableCell colSpan={7} className="h-28 text-center text-muted-foreground">
                    {loadError && !loading ? (
                      <span role="alert" className="text-destructive">
                        加载失败：{loadError}
                      </span>
                    ) : (
                      emptyLabel
                    )}
                  </TableCell>
                </TableRow>
              )}
              {!loading &&
                !loadError &&
                visibleItems.map((item) => (
                  <TableRow key={item.id} className="even:bg-muted/15">
                    <TableCell className="max-w-52 py-3 pl-4">
                      <button
                        type="button"
                        onClick={() => setDetailID(item.id)}
                        className="max-w-full truncate text-left font-medium hover:underline"
                        title={item.name}
                      >
                        {item.name}
                      </button>
                      <div className="mt-1 flex items-center gap-2 text-muted-foreground">
                        <span>#{item.id}</span>
                        {globalID === item.id && (
                          <Badge variant="outline" className="px-1 py-0 text-[10px]">
                            全局默认
                          </Badge>
                        )}
                      </div>
                    </TableCell>
                    <TableCell>
                      <Badge variant="secondary" className="text-[10px]">
                        {item.kind === "resin" ? "Resin" : item.kind.toUpperCase()}
                      </Badge>
                      <div className="mt-1 text-muted-foreground">
                        {item.kind === "warp" ? "SOCKS5" : (item.protocol || item.kind).toUpperCase()}
                      </div>
                    </TableCell>
                    <TableCell className="max-w-64">
                      <span className="block truncate font-mono" title={endpoint(item)}>
                        {endpoint(item)}
                      </span>
                      {item.kind === "resin" && (
                        <div className="mt-1 text-muted-foreground">{item.resin_platform || "Default"}</div>
                      )}
                    </TableCell>
                    <TableCell>
                      <span className="font-mono">
                        {item.kind === "warp" ? (warpEffectiveIP(item) ?? "未验证") : item.egress_ip || "—"}
                      </span>
                    </TableCell>
                    <TableCell>
                      <Badge
                        variant={!item.enabled ? "outline" : badgeVariant(listStatus(item))}
                        className="text-[10px]"
                      >
                        {!item.enabled ? "已停用" : (STATUS[listStatus(item)] ?? listStatus(item))}
                      </Badge>
                      {item.kind === "warp" && (
                        <div className="mt-1 text-muted-foreground">
                          {WARP_PHASE_LABEL[item.warp_phase ?? ""] ?? "状态待确认"}
                        </div>
                      )}
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-muted-foreground">
                      {item.last_checked_at ? new Date(item.last_checked_at).toLocaleString() : "尚未检测"}
                    </TableCell>
                    <TableCell className="pr-4 text-right">
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <Button
                            size="sm"
                            variant="outline"
                            className="h-7 gap-1 px-2 text-xs"
                            aria-label={`操作：${item.name}`}
                          >
                            操作
                            <ChevronDown className="size-3" />
                          </Button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end">
                          <DropdownMenuItem onSelect={() => setDetailID(item.id)}>查看详情</DropdownMenuItem>
                          <DropdownMenuItem disabled={!!busy} onSelect={() => void test(item.id)}>
                            检测连接
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            disabled={!!busy || (createStage === "pending" && pendingID === item.id)}
                            onSelect={() => edit(item)}
                          >
                            编辑代理
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            disabled={!!busy || !selectable(item) || globalID === item.id}
                            onSelect={() => void choose(item.id)}
                          >
                            设为全局默认
                          </DropdownMenuItem>
                          {item.kind === "warp" &&
                            warpAllowedActions(item)
                              .filter((action) => action !== "recover")
                              .map((action) => (
                                <DropdownMenuItem
                                  key={action}
                                  disabled={
                                    !!busy ||
                                    (createStage === "pending" && pendingID === item.id) ||
                                    (action === "register" && registrationUncertain.includes(item.id))
                                  }
                                  onSelect={() => {
                                    if (warpActionNeedsTerms(action)) {
                                      setAcceptTerms(false);
                                      setRegistering(item.id);
                                    } else void warp(item.id, action);
                                  }}
                                >
                                  {
                                    { register: "注册 WARP", start: "启动 WARP", stop: "停止 WARP", recover: "恢复" }[
                                      action
                                    ]
                                  }
                                </DropdownMenuItem>
                              ))}
                          {item.kind === "resin" && (
                            <DropdownMenuItem disabled={!!busy} onSelect={() => setReleasing(item.id)}>
                              {releaseReadback.includes(item.id) ? "核对租约状态" : "释放租约"}
                            </DropdownMenuItem>
                          )}
                          <DropdownMenuSeparator />
                          <DropdownMenuItem variant="destructive" disabled={!!busy} onSelect={() => setDeleting(item)}>
                            删除代理
                          </DropdownMenuItem>
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </TableCell>
                  </TableRow>
                ))}
            </TableBody>
          </Table>
          <div className="border-t px-4 py-2 text-muted-foreground text-xs">
            {loading || loadError
              ? "正在核对代理状态"
              : `共 ${items.length} 个代理${visibleItems.length !== items.length ? `，当前显示 ${visibleItems.length} 个` : ""}`}
          </div>
        </CardContent>
      </Card>
      <Dialog open={groupsOpen} onOpenChange={setGroupsOpen}>
        <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-4xl">
          <DialogHeader>
            <DialogTitle>代理分组</DialogTitle>
            <DialogDescription>管理分组成员、轮询与定时刷新。</DialogDescription>
          </DialogHeader>
          <ProxyGroups proxies={items} />
        </DialogContent>
      </Dialog>
      <Dialog
        open={detailID !== null}
        onOpenChange={(open) => {
          if (!open) setDetailID(null);
        }}
      >
        <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>{detail ? detail.name : "代理详情"}</DialogTitle>
            <DialogDescription>
              #{detailID} · {detail ? endpoint(detail) : "代理记录已不可用，请刷新列表"}
            </DialogDescription>
          </DialogHeader>
          {detail?.kind === "warp" && <WarpHealthStatus proxy={detail} />}
          {detail?.last_error && <p className="break-words text-destructive text-xs">{errorText(detail.last_error)}</p>}
          {detail?.kind === "resin" && (
            <p className="text-sm">
              Platform：{detail.resin_platform || "Default"} · Proxy Token：{detail.has_password ? "已保存" : "未配置"}{" "}
              · Admin Token：{detail.has_resin_admin_token ? "已保存" : "未配置"}
            </p>
          )}
          {detail?.kind === "warp" &&
            (detail.warp_phase === "registering" || registrationUncertain.includes(detail.id)) && (
              <p className="text-muted-foreground text-xs">注册结果尚未确认，系统会保留原条目，避免重复注册。</p>
            )}
        </DialogContent>
      </Dialog>
      <AlertDialog
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open && !mutationLock.current) setDeleting(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除代理「{deleting?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>
              {deleting?.kind === "warp"
                ? "删除会自动断开 WARP 出口并清理本地注册档案。"
                : "删除后此代理将从代理中心移除。"}
              正在创建、被任务或渠道引用、设为全局默认的代理无法删除，请先解除引用。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={!!busy}>取消</AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              disabled={!!busy}
              onClick={(event) => {
                event.preventDefault();
                if (deleting) void remove(deleting);
              }}
            >
              {busy.startsWith("delete:") ? "删除中…" : "确认删除"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <Dialog
        open={releasing !== null}
        onOpenChange={(open) => {
          if (!open && !mutationLock.current) setReleasing(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>释放 Resin #{releasing} 的租约？</DialogTitle>
            <DialogDescription>
              仅请求释放此出口的租约，服务端会检查实际使用状态；不会强制切换任务到直连。
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={!!busy} onClick={() => setReleasing(null)}>
              取消
            </Button>
            <Button
              disabled={!!busy}
              onClick={() => {
                if (releasing !== null) void release(releasing);
              }}
            >
              {releasing !== null && releaseReadback.includes(releasing) ? "仅刷新核对" : "确认请求释放"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* WARP 注册条款确认：勾选项默认不选中；取消不发请求；确认后才带 accept_terms:true。 */}
      <Dialog
        open={registering !== null}
        onOpenChange={(open) => {
          if (mutationLock.current) return;
          if (!open) {
            setRegistering(null);
            setAcceptTerms(false);
          }
        }}
      >
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>注册 WARP 出口 #{registering}</DialogTitle>
            <DialogDescription>注册前请阅读并确认以下说明；未确认不会发起任何注册请求。</DialogDescription>
          </DialogHeader>
          <ul className="list-disc space-y-2 pl-5 text-muted-foreground text-sm">
            {WARP_REGISTER_TERMS.map((line) => (
              <li key={line}>{line}</li>
            ))}
          </ul>
          <a href={WARP_TERMS_URL} target="_blank" rel="noreferrer" className="text-sm underline">
            Cloudflare WARP 官方服务条款
          </a>
          <Label htmlFor="warp-accept-terms" className="flex items-start gap-2 font-normal text-sm">
            <Checkbox
              id="warp-accept-terms"
              checked={acceptTerms}
              onCheckedChange={(checked) => setAcceptTerms(checked === true)}
            />
            <span>我已阅读并同意上述注册说明，确认现在向 Cloudflare 申请注册。</span>
          </Label>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={!!busy}
              onClick={() => {
                setRegistering(null);
                setAcceptTerms(false);
              }}
            >
              取消
            </Button>
            <Button
              type="button"
              disabled={!acceptTerms || !!busy}
              onClick={() => {
                if (registering === null) return;
                void warp(registering, "register");
              }}
            >
              {busy ? "注册中…" : "同意并注册"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
