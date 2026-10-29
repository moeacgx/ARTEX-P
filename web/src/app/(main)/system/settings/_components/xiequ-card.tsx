"use client";

import * as React from "react";

import { KeyRoundIcon, RefreshCwIcon, ShieldCheckIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { type XiequConfig, type XiequRefreshResult, type XiequSettingsPatch, xiequAPI } from "@/lib/xiequ-api";

function splitDomains(value: string): string[] {
  return value
    .split(/[\s,，]+/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function parseInterval(value: string): number {
  const text = value.trim();
  const parsed = Number(text);
  if (!/^\d+$/.test(text) || !Number.isInteger(parsed) || parsed < 30 || parsed > 3600) {
    throw new Error("刷新间隔必须为 30–3600 秒的整数");
  }
  return parsed;
}

function formatTime(value: string | null | undefined): string {
  if (!value) return "尚无记录";
  return new Date(value).toLocaleString("zh-CN", { hour12: false });
}

export function XiequCard() {
  const [config, setConfig] = React.useState<XiequConfig | null>(null);
  const [enabled, setEnabled] = React.useState(false);
  const [uid, setUid] = React.useState("");
  const [ukey, setUkey] = React.useState("");
  const [baseURL, setBaseURL] = React.useState("");
  const [memo, setMemo] = React.useState("");
  const [interval, setInterval] = React.useState("");
  const [domains, setDomains] = React.useState("");
  const [loadError, setLoadError] = React.useState("");
  const [action, setAction] = React.useState<"load" | "save" | "refresh" | "clear" | null>("load");
  const [lastRefresh, setLastRefresh] = React.useState<XiequRefreshResult | null>(null);
  const locked = React.useRef(false);

  const applyConfig = React.useCallback((value: XiequConfig) => {
    setConfig(value);
    setEnabled(value.enabled);
    setUid(value.uid);
    setUkey("");
    setBaseURL(value.base_url);
    setMemo(value.memo);
    setInterval(String(value.refresh_interval_seconds));
    setDomains(value.proxy_domains.join("\n"));
  }, []);

  const load = React.useCallback(async () => {
    if (locked.current) return;
    locked.current = true;
    setAction("load");
    setLoadError("");
    try {
      applyConfig(await xiequAPI.config());
    } catch (error) {
      setConfig(null);
      setLoadError((error as Error).message);
    } finally {
      locked.current = false;
      setAction(null);
    }
  }, [applyConfig]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const save = async () => {
    if (locked.current || !config) return;
    locked.current = true;
    setAction("save");
    try {
      const parsedInterval = parseInterval(interval);
      const parsedDomains = splitDomains(domains);
      if (enabled && !config.ukey_set && !ukey.trim()) {
        throw new Error("请先输入 UKEY，再启用谐趣白名单自动刷新");
      }
      const patch: XiequSettingsPatch = {
        enabled,
        uid: uid.trim(),
        base_url: baseURL.trim(),
        memo: memo.trim(),
        refresh_interval_seconds: parsedInterval,
        proxy_domains: parsedDomains,
      };
      if (ukey.trim()) patch.ukey = ukey.trim();
      applyConfig(await xiequAPI.save(patch));
      toast.success(enabled ? "已保存并启用谐趣白名单自动刷新" : "已保存谐趣白名单配置");
    } catch (error) {
      toast.error(`保存失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const refresh = async () => {
    if (locked.current || !config) return;
    locked.current = true;
    setAction("refresh");
    try {
      const result = await xiequAPI.refresh();
      setLastRefresh(result);
      if (result.ok) toast.success(result.message);
      else toast.error(result.message);
      applyConfig(await xiequAPI.config());
    } catch (error) {
      toast.error(`刷新失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const clearKey = async () => {
    if (locked.current || !config?.ukey_set) return;
    locked.current = true;
    setAction("clear");
    try {
      applyConfig(await xiequAPI.save({ enabled: false, ukey: "" }));
      toast.success("已清除谐趣 UKEY，并关闭自动刷新");
    } catch (error) {
      toast.error(`清除失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const busy = action !== null;
  const hasKey = !!config?.ukey_set || ukey.trim() !== "";
  const state = config?.state;
  let statusLabel = "未配置";
  if (config?.configured) statusLabel = config.enabled ? "已启用" : "已配置，未启用";

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ShieldCheckIcon className="size-4" />
          谐趣代理池白名单
        </CardTitle>
        <CardDescription>
          使用谐趣代理前，自动把本机当前公网 IPv4 加入谐趣白名单。管理接口和公网 IP 探测都强制直连，不经过待授权的代理；
          添加后会重新读取白名单确认。账户最多 5 个白名单，本工具只回收自己添加过的旧 IP。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {loadError && (
          <div role="alert" className="flex flex-col gap-3">
            <p className="text-destructive text-sm">谐趣配置读取失败：{loadError}</p>
            <Button type="button" variant="outline" disabled={busy} onClick={() => void load()} className="self-start">
              <RefreshCwIcon className="size-4" />
              重新读取
            </Button>
          </div>
        )}
        {!loadError && !config && (
          <p role="status" className="text-muted-foreground text-sm">
            正在读取谐趣白名单配置…
          </p>
        )}
        {!loadError && config && (
          <>
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-muted-foreground text-sm">当前状态</span>
              <Badge variant={config.enabled ? "secondary" : "outline"}>{statusLabel}</Badge>
              <Badge variant="outline">UKEY {config.ukey_set ? "已保存" : "未保存"}</Badge>
              {state?.whitelist_count !== undefined && (
                <Badge variant="outline">白名单 {state.whitelist_count}/5</Badge>
              )}
            </div>

            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="xiequ-enabled" className="font-normal text-sm">
                启用谐趣白名单自动刷新
              </Label>
              <Switch
                id="xiequ-enabled"
                checked={enabled}
                disabled={busy || (!hasKey && !enabled)}
                onCheckedChange={setEnabled}
              />
            </div>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="flex flex-col gap-2">
                <Label htmlFor="xiequ-uid">UID</Label>
                <Input
                  id="xiequ-uid"
                  value={uid}
                  disabled={busy}
                  autoComplete="off"
                  onChange={(event) => setUid(event.target.value)}
                />
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="xiequ-ukey" className="flex items-center gap-2">
                  <KeyRoundIcon className="size-3.5" />
                  UKEY
                </Label>
                <Input
                  id="xiequ-ukey"
                  type="password"
                  value={ukey}
                  disabled={busy}
                  autoComplete="new-password"
                  placeholder={config.ukey_set ? "已保存；留空保持不变" : "输入谐趣 UKEY"}
                  onChange={(event) => setUkey(event.target.value)}
                />
              </div>
            </div>
            <p className="text-muted-foreground text-xs leading-relaxed">
              UKEY 在服务端加密保存，不会返回给模型或前端。Agent 只能调用工具，不能读取或提交 UID/UKEY。
            </p>

            <div className="grid gap-3 sm:grid-cols-2">
              <div className="flex flex-col gap-2">
                <Label htmlFor="xiequ-interval">刷新冷却（秒）</Label>
                <Input
                  id="xiequ-interval"
                  type="number"
                  min={30}
                  max={3600}
                  value={interval}
                  disabled={busy}
                  onChange={(event) => setInterval(event.target.value)}
                />
                <p className="text-muted-foreground text-xs">30–3600 秒；自动调用按此冷却，手动刷新立即执行。</p>
              </div>
              <div className="flex flex-col gap-2">
                <Label htmlFor="xiequ-memo">添加备注</Label>
                <Input id="xiequ-memo" value={memo} disabled={busy} onChange={(event) => setMemo(event.target.value)} />
              </div>
            </div>

            <div className="flex flex-col gap-2">
              <Label htmlFor="xiequ-domains">识别为谐趣代理的主机域名</Label>
              <Textarea
                id="xiequ-domains"
                value={domains}
                disabled={busy}
                rows={3}
                placeholder="xiequ.cn"
                onChange={(event) => setDomains(event.target.value)}
              />
              <p className="text-muted-foreground text-xs">
                每行一个，也支持逗号分隔；匹配域名本身及其子域。默认 `xiequ.cn`。
              </p>
            </div>

            <div className="flex flex-col gap-2">
              <Label htmlFor="xiequ-base-url">管理接口地址</Label>
              <Input
                id="xiequ-base-url"
                value={baseURL}
                disabled={busy}
                onChange={(event) => setBaseURL(event.target.value)}
              />
              <p className="text-muted-foreground text-xs">
                默认 `http://op.xiequ.cn/IpWhiteList.aspx`；管理接口始终直连。
              </p>
            </div>

            <div className="flex flex-wrap items-center gap-2">
              <Button type="button" onClick={() => void save()} disabled={busy}>
                {action === "save" ? "保存中…" : "保存配置"}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => void refresh()}
                disabled={busy || !config.configured}
              >
                <RefreshCwIcon className="size-4" />
                {action === "refresh" ? "刷新中…" : "立即刷新白名单"}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => void clearKey()}
                disabled={busy || !config.ukey_set}
              >
                {action === "clear" ? "清除中…" : "清除 UKEY 并关闭"}
              </Button>
            </div>

            <div className="flex flex-col gap-2 border-t pt-4 text-xs leading-relaxed">
              <p>
                最近尝试：{formatTime(state?.last_attempt_at)} · 最近成功：{formatTime(state?.last_success_at)}
              </p>
              <p>最近识别公网 IP：{state?.last_ip || "尚无记录"}</p>
              <p>本工具管理过的 IP：{state?.managed_ips?.length ? state.managed_ips.join("、") : "无"}</p>
              {state?.last_error && <p className="text-destructive">最近错误：{state.last_error}</p>}
              {lastRefresh && (
                <p className={lastRefresh.ok ? "text-emerald-600" : "text-destructive"}>
                  本次刷新：{lastRefresh.message}
                  {lastRefresh.removed?.length ? `；已回收 ${lastRefresh.removed.join("、")}` : ""}
                </p>
              )}
              <p className="text-muted-foreground">{config.quota_hint}</p>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
