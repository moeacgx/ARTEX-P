"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { proxyErrorText } from "@/lib/proxy-center";
import { RevisionSubmission } from "@/lib/proxy-policy";
import type { GlobalWarpHealthPolicy } from "@/lib/warp-health";
import { type GlobalWarpHealthAdapter, globalWarpHealthAPI } from "@/lib/warp-health-api";

export function GlobalWarpHealthControl({
  onUpdated,
  adapter = globalWarpHealthAPI,
}: {
  onUpdated?: () => void;
  adapter?: GlobalWarpHealthAdapter;
}) {
  const [open, setOpen] = React.useState(false);
  const [policy, setPolicy] = React.useState<GlobalWarpHealthPolicy | null>(null);
  const [draft, setDraft] = React.useState<boolean | null>(null);
  const [expected, setExpected] = React.useState<number | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [error, setError] = React.useState("");
  const [review, setReview] = React.useState(false);
  const [readback, setReadback] = React.useState(false);
  const [submission] = React.useState(() => new RevisionSubmission<GlobalWarpHealthPolicy>());
  const sequence = React.useRef(0);
  const mounted = React.useRef(true);
  React.useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      sequence.current++;
    };
  }, []);
  const sync = () => {
    setReview(submission.reviewRequired);
    setReadback(submission.needsReadback);
  };
  const load = React.useCallback(
    async (initialize: boolean) => {
      const current = ++sequence.current;
      setLoading(true);
      try {
        const value = await adapter.get();
        if (!mounted.current || current !== sequence.current) return;
        setPolicy(value);
        if (initialize && !submission.reviewRequired && !submission.needsReadback) {
          setDraft(value.auto_recover_enabled);
          setExpected(value.revision);
        }
        setError("");
      } catch (failure) {
        if (mounted.current && current === sequence.current) {
          setPolicy(null);
          setError(`暂时无法读取设置：${proxyErrorText(failure)}。请重新读取后再修改。`);
        }
      } finally {
        if (mounted.current && current === sequence.current) setLoading(false);
      }
    },
    [adapter, submission],
  );
  function changeOpen(value: boolean) {
    if (submission.busy) return;
    setOpen(value);
    if (value) void load(true);
    else {
      sequence.current++;
      setLoading(false);
    }
  }
  async function save() {
    if (submission.busy || expected === null || draft === null || readback || review || !policy) return;
    const current = sequence.current;
    setSaving(true);
    setError("");
    try {
      const value = await submission.submit(
        async () => {
          await adapter.save(expected, draft);
        },
        () => adapter.get(),
        (result) => result.revision > expected && result.auto_recover_enabled === draft,
      );
      if (!mounted.current || current !== sequence.current || !value) return;
      setPolicy(value);
      if (submission.confirmed) {
        setExpected(value.revision);
        setDraft(value.auto_recover_enabled);
        onUpdated?.();
        setError("设置已保存。");
      } else setError("设置已发生变化。已保留你的选择，请核对后重新确认。");
    } catch (failure) {
      if (mounted.current && current === sequence.current)
        setError(`暂时无法确认保存结果：${proxyErrorText(failure)}。已保留你的选择，请先核对当前设置。`);
    } finally {
      if (mounted.current && current === sequence.current) {
        setSaving(false);
        sync();
      }
    }
  }
  async function reconcile() {
    if (submission.busy) return;
    const current = sequence.current;
    setSaving(true);
    try {
      const value = await submission.reconcile(() => adapter.get());
      if (!mounted.current || current !== sequence.current || !value) return;
      setPolicy(value);
      setError("已读取当前设置。你的选择已保留，请核对后确认是否继续保存。");
    } catch (failure) {
      if (mounted.current && current === sequence.current) {
        submission.needsReadback = true;
        setError(`暂时无法核对：${proxyErrorText(failure)}。请稍后重试核对。`);
      }
    } finally {
      if (mounted.current && current === sequence.current) {
        setSaving(false);
        sync();
      }
    }
  }
  React.useEffect(() => {
    void load(false);
  }, [load]);
  let stateLabel = "待核对";
  if (policy) stateLabel = policy.auto_recover_enabled ? "开" : "关";
  return (
    <>
      <Button size="sm" variant="outline" onClick={() => changeOpen(true)}>
        WARP 自动恢复：{stateLabel}
      </Button>
      <Dialog open={open} onOpenChange={changeOpen}>
        <DialogContent showCloseButton={!saving}>
          <DialogHeader>
            <DialogTitle>全局 WARP 自动恢复</DialogTitle>
            <DialogDescription>
              统一管理当前和以后新增的 WARP。关闭仍持续检测；已停止的节点不会自动启动。
            </DialogDescription>
          </DialogHeader>
          {loading && <p role="status">正在读取设置…</p>}
          {policy && (
            <>
              {policy.migrated_disabled_nodes > 0 && policy.revision === 1 && !policy.auto_recover_enabled && (
                <p className="text-muted-foreground text-xs">
                  之前有节点关闭了自动恢复，因此升级后默认关闭。开启后全部 WARP 统一生效。
                </p>
              )}
              <div className="flex items-center justify-between gap-3">
                <Label htmlFor="global-warp-automatic">自动恢复</Label>
                <Switch
                  id="global-warp-automatic"
                  checked={draft === true}
                  disabled={loading || saving || readback || review || draft === null}
                  onCheckedChange={setDraft}
                />
              </div>
              <p className="text-muted-foreground text-xs">
                每分钟检测，连续 {policy.failure_threshold} 次失败后尝试恢复。仅在旧操作已退出且没有占用时恢复，
                失败后等待 5–60 分钟重试。关闭不会中断已开始的操作。
              </p>
            </>
          )}
          {error && (
            <p role="status" className="text-sm">
              {error}
            </p>
          )}
          {review && !readback && policy && (
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => {
                if (submission.confirmNewIntent()) {
                  setExpected(policy.revision);
                  sync();
                  setError("已确认当前设置。点击保存后应用你的选择。");
                }
              }}
            >
              已核对，继续修改
            </Button>
          )}
          <DialogFooter>
            <Button variant="outline" disabled={saving} onClick={() => changeOpen(false)}>
              取消
            </Button>
            {(readback || review) && (
              <Button variant="outline" disabled={saving || loading} onClick={() => void reconcile()}>
                核对当前设置
              </Button>
            )}
            {!policy && !readback && !review && (
              <Button variant="outline" disabled={saving || loading} onClick={() => void load(true)}>
                重新读取
              </Button>
            )}
            <Button
              disabled={loading || saving || !policy || readback || review || draft === null}
              onClick={() => void save()}
            >
              保存
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
