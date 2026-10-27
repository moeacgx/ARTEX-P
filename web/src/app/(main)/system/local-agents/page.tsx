"use client";

import * as React from "react";

import { Download, Laptop, RefreshCw } from "lucide-react";
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
import { api } from "@/lib/api";
import { downloadLocalAgentSkill } from "@/lib/local-agent-download";
import type { LocalAgentDevice, LocalAgentScope } from "@/lib/types";

const scopes: { value: LocalAgentScope; label: string; description: string }[] = [
  { value: "tasks.read", label: "读取任务", description: "查看任务列表和状态" },
  { value: "tasks.create", label: "创建任务", description: "提交新任务，可能启动实际执行" },
  { value: "skills.read", label: "读取 Skill", description: "查看已安装的 Skill 列表" },
  { value: "skills.upload", label: "上传 Skill", description: "安装或覆盖 Skill 压缩包" },
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
  const [devices, setDevices] = React.useState<LocalAgentDevice[]>([]);
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [downloading, setDownloading] = React.useState(false);
  const [selected, setSelected] = React.useState<LocalAgentDevice | null>(null);
  const [revoking, setRevoking] = React.useState<LocalAgentDevice | null>(null);
  const [chosenScopes, setChosenScopes] = React.useState<LocalAgentScope[]>([]);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const response = await api.localAgentDevices();
      setDevices(response);
    } catch (error) {
      toast.error(`读取设备失败：${(error as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

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

      <Card>
        <CardHeader>
          <CardTitle>连接本机 Agent</CardTitle>
          <CardDescription>下载后解压到本机 Agent 的 Skill 目录，再让 Agent 按 SKILL.md 发起配对。</CardDescription>
        </CardHeader>
        <CardContent className="space-y-3">
          <Button
            variant="outline"
            disabled={downloading}
            onClick={async () => {
              setDownloading(true);
              try {
                await downloadLocalAgentSkill();
              } catch (error) {
                toast.error((error as Error).message);
              } finally {
                setDownloading(false);
              }
            }}
          >
            <Download className="mr-2 size-4" />
            下载接入 Skill
          </Button>
          <p className="text-sm text-muted-foreground">
            需要 Node.js 20 或更高版本。首次连接后，在下方核对设备指纹并授权。
          </p>
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
