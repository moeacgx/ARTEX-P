"use client";

import * as React from "react";

import { BellRing, Clipboard, Download, Laptop, RefreshCw } from "lucide-react";
import { toast } from "sonner";

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
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import { downloadLocalAgentSkill } from "@/lib/local-agent-download";
import {
  localAgentSkillNotice,
  localAgentSkillState,
  localAgentUpdatePrompt,
  validSkillVersion,
} from "@/lib/local-agent-skill";
import type { LocalAgentDevice, LocalAgentScope } from "@/lib/types";

const scopes: { value: LocalAgentScope; label: string; description: string }[] = [
  { value: "tasks.read", label: "读取任务", description: "查看任务列表和状态" },
  { value: "tasks.create", label: "创建任务", description: "提交新任务，可能启动实际执行" },
  { value: "tasks.control", label: "控制任务", description: "暂停、恢复和停止任务执行" },
  { value: "tasks.message", label: "任务消息", description: "向任务主 Agent 或 Worker 发送消息" },
  { value: "skills.read", label: "读取 Skill", description: "查看已安装的 Skill 列表" },
  { value: "skills.upload", label: "上传 Skill", description: "安装或覆盖 Skill 压缩包" },
  { value: "skills.write", label: "修改 Skill", description: "读取、写入或删除 Skill 文件" },
  { value: "assets.read", label: "读取资产", description: "查看资产列表和统计" },
  { value: "assets.write", label: "写入资产", description: "上传或批量添加资产" },
  { value: "assets.delete", label: "删除资产", description: "删除指定资产记录" },
  { value: "workspace.read", label: "读取工作空间", description: "列出目录和读取文件" },
  { value: "workspace.write", label: "写入工作空间", description: "写入文件或创建目录" },
  { value: "workspace.upload", label: "上传工作空间文件", description: "上传文件到工作空间" },
  { value: "workspace.download", label: "下载工作空间文件", description: "下载工作空间文件" },
  { value: "workspace.delete", label: "删除工作空间文件", description: "删除文件或目录" },
  { value: "conversations.read", label: "读取对话", description: "查看对话和消息记录" },
  { value: "conversations.create", label: "创建对话", description: "创建主 Agent 对话" },
  { value: "conversations.message", label: "发送对话消息", description: "向主 Agent 发送消息或修改对话名称" },
  { value: "conversations.stop", label: "停止对话", description: "停止主 Agent 当前回复" },
  { value: "discovery.read", label: "读取发现数据", description: "查看发现页的 findings、图、节点、活动和任务统计" },
  { value: "logs.read", label: "读取日志", description: "查看运行日志和实时日志流" },
  { value: "llm.read", label: "读取模型配置", description: "查看模型渠道、池和调用记录" },
  { value: "llm.write", label: "修改模型配置", description: "切换或修改全局、任务和对话模型" },
  { value: "llm.test", label: "测试模型配置", description: "发起一次模型连通性测试" },
  { value: "tools.read", label: "读取工具配置", description: "查看工具和自定义工具" },
  { value: "tools.write", label: "修改工具配置", description: "启用、禁用或修改工具" },
  { value: "mcp.read", label: "读取 MCP", description: "查看 MCP 服务器和工具" },
  { value: "mcp.write", label: "修改 MCP", description: "新增、修改、刷新或删除 MCP 服务器" },
];

function formatTime(value?: string | null) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("zh-CN");
}

function statusLabel(status: LocalAgentDevice["status"]) {
  if (status === "pending") return "待审批";
  if (status === "approved") return "已授权";
  return "已撤销";
}

export default function LocalAgentsPage() {
  const [serverUrl, setServerUrl] = React.useState("");
  const [devices, setDevices] = React.useState<LocalAgentDevice[]>([]);
  const [skillVersion, setSkillVersion] = React.useState("");
  const [refreshError, setRefreshError] = React.useState("");
  const refreshing = React.useRef(false);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [downloading, setDownloading] = React.useState(false);
  const [selected, setSelected] = React.useState<LocalAgentDevice | null>(null);
  const [revoking, setRevoking] = React.useState<LocalAgentDevice | null>(null);
  const [chosenScopes, setChosenScopes] = React.useState<LocalAgentScope[]>([]);

  const load = React.useCallback(async (silent = false) => {
    if (refreshing.current) return;
    refreshing.current = true;
    if (!silent) setLoading(true);
    try {
      const response = await api.localAgentOverview();
      setDevices(response.devices);
      setSkillVersion(validSkillVersion(response.skill?.version));
      setRefreshError("");
    } catch (error) {
      const message = `读取设备失败：${(error as Error).message}`;
      setRefreshError(message);
      if (!silent) toast.error(message);
    } finally {
      refreshing.current = false;
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    setServerUrl(window.location.origin);
    void load();
    const refreshVisible = () => {
      if (document.visibilityState === "visible") void load(true);
    };
    const timer = window.setInterval(refreshVisible, 45_000);
    window.addEventListener("focus", refreshVisible);
    document.addEventListener("visibilitychange", refreshVisible);
    return () => {
      window.clearInterval(timer);
      window.removeEventListener("focus", refreshVisible);
      document.removeEventListener("visibilitychange", refreshVisible);
    };
  }, [load]);

  const skillNotice = localAgentSkillNotice(devices, skillVersion);
  const updatePrompt = localAgentUpdatePrompt(serverUrl, skillVersion);

  async function downloadSkill() {
    setDownloading(true);
    try {
      await downloadLocalAgentSkill();
    } catch (error) {
      toast.error((error as Error).message);
    } finally {
      setDownloading(false);
    }
  }

  const onboardingPrompt = serverUrl
    ? `请使用我刚安装的 ARTEX 本地 Agent Skill 接入此实例。\nARTEX 地址：${serverUrl}\n\n先阅读 Skill 的 SKILL.md 并检查 auth status。若尚未配对，运行 auth login，使用上面的地址和一个清晰的本机设备名称。然后把设备名称、设备 ID 和 SHA-256 指纹展示给我，并停下来等待我在 ARTEX「本地 Agent 接入」页面核对和审批最小权限。不要索取或读取管理员密码、JWT、API Key 或本机私钥。\n\n获批后再检查 auth status，并且只使用已批准的权限和我在本对话明确给出的任务范围。没有我明确指定的目标和授权范围时，不创建会执行扫描的任务。上传 Skill 时使用我明确提供的文件；除非我明确要求覆盖，不覆盖同名 Skill。`
    : "正在读取当前 ARTEX 地址…";

  async function copyText(value: string, label: string) {
    if (!value || !serverUrl) return;
    try {
      await navigator.clipboard.writeText(value);
      toast.success(`${label}已复制`);
    } catch {
      toast.error(`复制${label}失败，请手动选择文本复制`);
    }
  }

  function openApproval(device: LocalAgentDevice) {
    setSelected(device);
    setChosenScopes(device.status === "approved" ? device.scopes : []);
  }

  function toggleScope(scope: LocalAgentScope, checked: boolean) {
    setChosenScopes((current) => (checked ? [...current, scope] : current.filter((item) => item !== scope)));
  }

  async function approve() {
    if (!selected || chosenScopes.length === 0) return;
    setSaving(true);
    try {
      await api.approveLocalAgent(selected.id, chosenScopes);
      toast.success("设备权限已保存");
      setSelected(null);
      await load();
    } catch (error) {
      toast.error(`授权失败：${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  async function revoke() {
    if (!revoking) return;
    setSaving(true);
    try {
      await api.revokeLocalAgent(revoking.id);
      toast.success("设备已撤销");
      setRevoking(null);
      await load();
    } catch (error) {
      toast.error(`撤销失败：${(error as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-6 p-4 md:p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-2xl">本地 Agent 接入</h1>
          <p className="mt-1 text-muted-foreground text-sm">
            核对设备公钥后批准，并为每台设备单独授予任务与 Skill 权限。
          </p>
        </div>
        <Button variant="outline" onClick={() => void load()} disabled={loading}>
          <RefreshCw className="mr-2 size-4" />
          刷新
        </Button>
      </div>

      {refreshError && (
        <p role="status" className="text-destructive text-sm">
          {refreshError}。当前显示上次读取的信息，请刷新后核验。
        </p>
      )}

      {(skillNotice.different > 0 || skillNotice.unknown > 0) && (
        <div role="status" className="space-y-3 rounded-lg border border-amber-500/50 bg-amber-500/5 p-4">
          <div className="flex items-center gap-2 font-medium">
            <BellRing className="size-4" />
            接入 Skill 更新提醒
          </div>
          <p className="text-sm">
            {skillNotice.different > 0 &&
              `${skillNotice.different} 台已授权设备的 Skill 与当前服务器包不同，建议更新。`}
            {skillNotice.unknown > 0 && `${skillNotice.unknown} 台已授权设备尚未上报版本，请使用新版 Skill 检查。`}
          </p>
          <div className="flex flex-wrap gap-2">
            <Button size="sm" disabled={downloading} onClick={() => void downloadSkill()}>
              <Download className="mr-2 size-4" />
              下载当前 Skill
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={!serverUrl}
              onClick={() => void copyText(updatePrompt, "更新提示词")}
            >
              <Clipboard className="mr-2 size-4" />
              复制更新提示词
            </Button>
          </div>
          <p className="text-muted-foreground text-xs">
            覆盖 Skill 文件并保留设备身份，再运行 auth status 上报版本。下载后提醒不会消失，核验通过后自动刷新。
          </p>
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle>快速接入本机 Agent</CardTitle>
          <CardDescription>复制地址和提示词给本机 Agent；也可以先下载独立 Skill 包。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          <p className="text-muted-foreground text-sm">
            当前可下载 Skill：
            <span className="font-mono" title={skillVersion || undefined}>
              {skillVersion ? skillVersion.slice(0, 12) : loading ? "正在读取…" : "版本未知，暂时无法比较设备版本"}
            </span>
          </p>
          <div className="space-y-2">
            <Label htmlFor="local-agent-server-url">ARTEX 地址</Label>
            <div className="flex gap-2">
              <Input id="local-agent-server-url" readOnly value={serverUrl || "正在读取当前站点地址…"} />
              <Button variant="outline" disabled={!serverUrl} onClick={() => void copyText(serverUrl, "地址")}>
                <Clipboard className="mr-2 size-4" />
                复制地址
              </Button>
            </div>
          </div>
          <div className="space-y-2">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <Label htmlFor="local-agent-onboarding-prompt">首次接入提示词</Label>
              <Button
                variant="outline"
                size="sm"
                disabled={!serverUrl}
                onClick={() => void copyText(onboardingPrompt, "提示词")}
              >
                <Clipboard className="mr-2 size-4" />
                复制提示词
              </Button>
            </div>
            <Textarea
              id="local-agent-onboarding-prompt"
              readOnly
              value={onboardingPrompt}
              className="min-h-44 resize-y"
            />
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <Button variant="outline" disabled={downloading} onClick={() => void downloadSkill()}>
              <Download className="mr-2 size-4" />
              下载接入 Skill
            </Button>
            <Button variant="outline" disabled={!serverUrl} onClick={() => void copyText(updatePrompt, "更新提示词")}>
              <Clipboard className="mr-2 size-4" />
              复制更新提示词
            </Button>
            <p className="text-sm text-muted-foreground">
              需要 Node.js 20 或更高版本。首次配对后，请核对设备指纹并只批准所需权限。
            </p>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>设备审批</CardTitle>
          <CardDescription>
            本地 Agent 发起配对后会出现在这里。设备只在获批后才能使用对应能力；撤销立即生效。
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          {loading && devices.length === 0 && <p className="text-muted-foreground text-sm">正在读取设备…</p>}
          {!loading && devices.length === 0 && <p className="text-muted-foreground text-sm">暂无配对申请。</p>}
          {devices.map((device) => (
            <div key={device.id} className="rounded-lg border p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div className="flex min-w-0 items-start gap-3">
                  <Laptop className="mt-1 size-5 shrink-0 text-muted-foreground" />
                  <div className="min-w-0 space-y-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-medium">{device.name}</span>
                      <Badge variant={device.status === "approved" ? "default" : "secondary"}>
                        {statusLabel(device.status)}
                      </Badge>
                    </div>
                    <p className="break-all font-mono text-muted-foreground text-xs">
                      SHA-256 指纹：{device.fingerprint}
                    </p>
                    <p className="text-muted-foreground text-xs">
                      申请：{formatTime(device.created_at)} · 最近使用：{formatTime(device.last_used_at)}
                    </p>
                    <div className="flex flex-wrap items-center gap-2 text-xs">
                      <Badge
                        variant={
                          localAgentSkillState(device.skill_version, skillVersion).kind === "different"
                            ? "outline"
                            : "secondary"
                        }
                      >
                        {localAgentSkillState(device.skill_version, skillVersion).label}
                      </Badge>
                      <span className="font-mono text-muted-foreground" title={device.skill_version}>
                        Skill：{validSkillVersion(device.skill_version).slice(0, 12) || "未上报"}
                      </span>
                      <span className="text-muted-foreground">版本上报：{formatTime(device.skill_reported_at)}</span>
                    </div>
                    {device.status === "approved" && (
                      <p className="text-muted-foreground text-xs">
                        权限：
                        {device.scopes
                          .map((scope) => scopes.find((item) => item.value === scope)?.label ?? scope)
                          .join("、")}
                      </p>
                    )}
                  </div>
                </div>
                {device.status !== "revoked" && (
                  <div className="flex shrink-0 gap-2">
                    <Button size="sm" onClick={() => openApproval(device)}>
                      {device.status === "pending" ? "审批" : "调整权限"}
                    </Button>
                    <Button size="sm" variant="outline" onClick={() => setRevoking(device)}>
                      撤销
                    </Button>
                  </div>
                )}
              </div>
            </div>
          ))}
        </CardContent>
      </Card>

      <Dialog
        open={selected !== null}
        onOpenChange={(open) => {
          if (!open && !saving) setSelected(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{selected?.status === "approved" ? "调整设备权限" : "批准本地 Agent"}</DialogTitle>
            <DialogDescription>先与本地 Agent 输出核对设备名称和 SHA-256 指纹，再勾选需要的权限。</DialogDescription>
          </DialogHeader>
          {selected && (
            <div className="space-y-4">
              <div className="rounded-md border bg-muted/40 p-3 text-sm">
                <p className="font-medium">{selected.name}</p>
                <p className="mt-1 break-all font-mono text-xs">SHA-256：{selected.fingerprint}</p>
                <p className="mt-2 break-all font-mono text-muted-foreground text-xs">公钥：{selected.public_key}</p>
              </div>
              <div className="space-y-3">
                {scopes.map((scope) => (
                  <label
                    key={scope.value}
                    htmlFor={`local-agent-${scope.value}`}
                    className="flex cursor-pointer items-start gap-3 rounded-md border p-3"
                  >
                    <Checkbox
                      id={`local-agent-${scope.value}`}
                      checked={chosenScopes.includes(scope.value)}
                      onCheckedChange={(value) => toggleScope(scope.value, value === true)}
                    />
                    <span>
                      <span className="block font-medium text-sm">{scope.label}</span>
                      <span className="block text-muted-foreground text-xs">{scope.description}</span>
                    </span>
                  </label>
                ))}
              </div>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setSelected(null)} disabled={saving}>
              取消
            </Button>
            <Button onClick={() => void approve()} disabled={saving || chosenScopes.length === 0}>
              保存授权
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={revoking !== null}
        onOpenChange={(open) => {
          if (!open && !saving) setRevoking(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>撤销设备「{revoking?.name}」？</AlertDialogTitle>
            <AlertDialogDescription>撤销后该设备的现有密钥无法再使用，也无法重新申请授权。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={saving}>取消</AlertDialogCancel>
            <AlertDialogAction disabled={saving} onClick={() => void revoke()}>
              确认撤销
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
