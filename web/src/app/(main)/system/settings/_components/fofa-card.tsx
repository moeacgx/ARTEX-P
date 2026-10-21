"use client";

import * as React from "react";

import Link from "next/link";

import { RadarIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import {
  type FofaAccountCheck,
  type FofaConfig,
  fofaAPI,
  fofaConnectionTestInput,
  fofaSettingsPatch,
  fofaSettingsStatus,
} from "@/lib/fofa-api";

const quotaFields = [
  { key: "fofa_point", label: "F 点余额" },
  { key: "remain_free_point", label: "免费 F 点" },
  { key: "remain_api_query", label: "API 剩余查询次数" },
  { key: "remain_api_data", label: "API 剩余返回条数" },
] as const;
const quotaNumber = new Intl.NumberFormat("zh-CN", { maximumSignificantDigits: 21 });

export function FofaCard() {
  const [config, setConfig] = React.useState<FofaConfig | null>(null);
  const [enabled, setEnabled] = React.useState(false);
  const [apiKey, setApiKey] = React.useState("");
  const [requestInterval, setRequestInterval] = React.useState("");
  const [rateLimitCooldown, setRateLimitCooldown] = React.useState("");
  const [loadError, setLoadError] = React.useState("");
  const [accountCheck, setAccountCheck] = React.useState<FofaAccountCheck | null>(null);
  const [accountError, setAccountError] = React.useState("");
  const [action, setAction] = React.useState<"load" | "save" | "save-enable" | "enable" | "clear" | "test" | null>(
    "load",
  );
  const locked = React.useRef(false);

  const clearAccount = React.useCallback(() => {
    setAccountCheck(null);
    setAccountError("");
  }, []);

  const applyConfig = React.useCallback(
    (value: FofaConfig) => {
      setConfig(value);
      setEnabled(value.enabled);
      setApiKey("");
      setRequestInterval(String(value.request_interval_seconds));
      setRateLimitCooldown(String(value.rate_limit_cooldown_seconds));
      clearAccount();
    },
    [clearAccount],
  );

  const load = React.useCallback(async () => {
    if (locked.current) return;
    locked.current = true;
    setAction("load");
    setLoadError("");
    clearAccount();
    try {
      applyConfig(await fofaAPI.config());
    } catch (error) {
      setConfig(null);
      setLoadError((error as Error).message);
    } finally {
      locked.current = false;
      setAction(null);
    }
  }, [applyConfig, clearAccount]);

  React.useEffect(() => {
    void load();
  }, [load]);

  const save = async (activate = false) => {
    if (locked.current || !config) return;
    locked.current = true;
    clearAccount();
    setAction(activate ? "save-enable" : "save");
    try {
      const patch = fofaSettingsPatch(config, activate || enabled, apiKey, {
        request_interval_seconds: requestInterval,
        rate_limit_cooldown_seconds: rateLimitCooldown,
      });
      const value = await fofaAPI.save(patch);
      applyConfig(value);
      toast.success(value.enabled ? "已保存 FOFA 配置并启用工具" : "已保存 FOFA 配置；工具尚未启用");
    } catch (error) {
      toast.error(`保存失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const enableSaved = async () => {
    if (locked.current || !config) return;
    locked.current = true;
    clearAccount();
    setAction("enable");
    try {
      const value = await fofaAPI.enable(config);
      setConfig(value);
      setEnabled(value.enabled);
      // 只启用已保存配置，保留尚未保存的新 Key 和节流草稿。
      if (value.enabled) toast.success("已启用 FOFA；可通过测试连接确认 Key 是否有效");
      else toast.error("服务端尚未确认启用 FOFA，请重新读取配置");
    } catch (error) {
      toast.error(`启用失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const clear = async () => {
    if (locked.current || !config?.api_key_set) return;
    locked.current = true;
    clearAccount();
    setAction("clear");
    try {
      applyConfig(await fofaAPI.clear());
      toast.success("已清除 FOFA API Key，并关闭 FOFA 工具");
    } catch (error) {
      toast.error(`清除失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const test = async () => {
    if (locked.current || !config) return;
    locked.current = true;
    clearAccount();
    setAction("test");
    try {
      const result = await fofaAPI.test(fofaConnectionTestInput(config, apiKey));
      setAccountCheck(result);
      const message = result.message || "FOFA 连接测试成功";
      if (!config.enabled) {
        toast.success(`${message}；FOFA 尚未启用，请保存并启用，或一键启用已保存的 Key`);
      } else if (apiKey.trim()) {
        toast.success(`${message}；本次测试的新 Key 尚未保存`);
      } else {
        toast.success(message);
      }
    } catch (error) {
      setAccountError((error as Error).message);
      toast.error(`连接与额度查询失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const busy = action !== null;
  const hasKey = !!config?.api_key_set || apiKey.trim() !== "";
  const changed =
    !!config &&
    (enabled !== config.enabled ||
      apiKey.trim() !== "" ||
      requestInterval !== String(config.request_interval_seconds) ||
      rateLimitCooldown !== String(config.rate_limit_cooldown_seconds));
  const status = config ? fofaSettingsStatus(config) : "unconfigured";
  const statusLabel = { unconfigured: "未配置", disabled: "Key 已保存，尚未启用", enabled: "已启用" }[status];

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <RadarIcon className="size-4" />
          FOFA 资产搜索
        </CardTitle>
        <CardDescription>
          配置后，已绑定 FOFA 工具的 Agent 可通过 fofa_search 搜索公开资产数据。实际搜索受 FOFA 账号权限与额度限制。
          默认返回 20 条，单次最多 100 条。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        {loadError && (
          <div role="alert" className="flex flex-col gap-3">
            <p className="text-destructive text-sm">FOFA 配置读取失败：{loadError}</p>
            <Button type="button" variant="outline" disabled={busy} onClick={() => void load()} className="self-start">
              <RefreshCwIcon className="size-4" />
              重新读取
            </Button>
          </div>
        )}
        {!loadError && !config && (
          <p role="status" className="text-muted-foreground text-sm">
            正在读取 FOFA 配置…
          </p>
        )}
        {!loadError && config && (
          <>
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <span className="text-muted-foreground text-sm">当前已保存状态</span>
                <Badge variant={status === "enabled" ? "secondary" : "outline"}>{statusLabel}</Badge>
              </div>
              {status === "disabled" && (
                <div
                  role="status"
                  className="flex flex-col gap-3 rounded-md border border-amber-500/30 bg-amber-500/5 p-3"
                >
                  <p className="text-sm leading-relaxed">
                    FOFA 总开关已关闭，Agent 目前不能搜索资产。保存 Key 或测试连接不会自动启用工具。
                  </p>
                  <Button type="button" onClick={() => void enableSaved()} disabled={busy} className="self-start">
                    {action === "enable" ? "启用中…" : "一键启用 FOFA"}
                  </Button>
                  <p className="text-muted-foreground text-xs">
                    此操作仅启用已保存的 Key，不保存当前输入，也不执行搜索。
                  </p>
                </div>
              )}
              {status === "enabled" && (
                <p className="text-muted-foreground text-xs leading-relaxed">
                  总开关已开启。可通过「测试连接并查询额度」核对 Key 与账户信息，实际搜索仍受账号权限与额度限制。
                </p>
              )}
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="fofa-enabled" className="font-normal text-sm">
                启用 FOFA 工具
              </Label>
              <Switch
                id="fofa-enabled"
                checked={enabled}
                disabled={busy || (!hasKey && !enabled)}
                onCheckedChange={setEnabled}
                aria-describedby="fofa-key-help"
              />
            </div>
            {enabled !== config.enabled && (
              <p className="text-muted-foreground text-xs">开关修改尚未保存，点击「保存配置」后生效。</p>
            )}
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <Label htmlFor="fofa-api-key" className="font-normal text-muted-foreground text-sm">
                  API Key
                </Label>
              </div>
              <Input
                id="fofa-api-key"
                name="fofa-integration-api-key"
                // 使用遮罩文本字段，避免浏览器误认登录密码并自动回填。
                type="text"
                className="[-webkit-text-security:disc]"
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                data-1p-ignore="true"
                data-lpignore="true"
                data-bwignore="true"
                placeholder={config.api_key_set ? "留空保留原 Key；输入新 Key 后保存即可更换" : "输入 FOFA API Key"}
                value={apiKey}
                disabled={busy}
                onChange={(event) => {
                  setApiKey(event.target.value);
                  clearAccount();
                }}
                aria-describedby="fofa-key-help"
              />
              <p id="fofa-key-help" className="text-muted-foreground text-xs leading-relaxed">
                API Key 在服务端加密保存，不会提供给模型，也不会回显原值。输入 Key 后才可启用工具；留空保存不会清除原
                Key。
              </p>
              <p className="text-muted-foreground text-xs leading-relaxed">
                可在{" "}
                <Link href="/system/agents" className="underline underline-offset-4">
                  Agent 管理
                </Link>{" "}
                查看工具和 Skill 绑定，在{" "}
                <Link href="/system/skills" className="underline underline-offset-4">
                  Skill 管理
                </Link>{" "}
                查看内置 FOFA 使用说明。
              </p>
            </div>
            <div className="flex flex-col gap-3 border-t pt-4">
              <div className="grid gap-3 sm:grid-cols-2">
                <div className="flex flex-col gap-2">
                  <Label htmlFor="fofa-request-interval">请求最小间隔（秒）</Label>
                  <Input
                    id="fofa-request-interval"
                    type="number"
                    min={1}
                    max={60}
                    step={1}
                    value={requestInterval}
                    disabled={busy}
                    onChange={(event) => setRequestInterval(event.target.value)}
                    aria-describedby="fofa-throttle-help"
                  />
                  <p className="text-muted-foreground text-xs">1–60 秒，默认 2 秒。</p>
                </div>
                <div className="flex flex-col gap-2">
                  <Label htmlFor="fofa-rate-limit-cooldown">限流冷却时间（秒）</Label>
                  <Input
                    id="fofa-rate-limit-cooldown"
                    type="number"
                    min={1}
                    max={3600}
                    step={1}
                    value={rateLimitCooldown}
                    disabled={busy}
                    onChange={(event) => setRateLimitCooldown(event.target.value)}
                    aria-describedby="fofa-throttle-help"
                  />
                  <p className="text-muted-foreground text-xs">1–3600 秒，默认 60 秒。</p>
                </div>
              </div>
              <p id="fofa-throttle-help" className="text-muted-foreground text-xs leading-relaxed">
                本实例内所有 Worker 共用请求间隔和限流冷却，切换代理不会绕过冷却。发生限流后，按设置值与上游 Retry-After
                中较长的时间等待。默认 60 秒是本程序的保守设置，不代表 FOFA 官方额度或限额。
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              {!config.enabled && apiKey.trim() !== "" && (
                <Button type="button" onClick={() => void save(true)} disabled={busy}>
                  {action === "save-enable" ? "保存并启用中…" : "保存并启用"}
                </Button>
              )}
              <Button
                type="button"
                variant="outline"
                onClick={() => void save()}
                disabled={busy || !changed || (enabled && !hasKey)}
              >
                {action === "save" ? "保存中…" : "保存配置"}
              </Button>
              <Button
                type="button"
                variant="outline"
                onClick={() => void clear()}
                disabled={busy || !config.api_key_set}
              >
                {action === "clear" ? "清除中…" : "清除 Key 并关闭"}
              </Button>
            </div>
            <div className="flex flex-col gap-3 border-t pt-4">
              <p className="text-muted-foreground text-xs leading-relaxed">
                优先使用当前输入的 Key，留空则使用已保存的 Key。仅访问账户接口，不执行资产搜索，也不会保存输入。
              </p>
              <Button
                type="button"
                variant="outline"
                className="self-start"
                onClick={() => void test()}
                disabled={busy || !hasKey}
              >
                {action === "test" ? "正在查询额度…" : "测试连接并查询额度"}
              </Button>
              {action === "test" && (
                <p role="status" className="text-muted-foreground text-xs">
                  正在读取账户额度…
                </p>
              )}
              {accountError && (
                <p role="alert" className="text-destructive text-sm">
                  额度读取失败：{accountError}
                </p>
              )}
              {accountCheck && (
                <section aria-label="FOFA 账户额度" className="flex flex-col gap-3 rounded-md border p-3">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <h3 className="font-medium text-sm">账户额度（本次读取）</h3>
                    <Badge variant="outline">
                      {accountCheck.key_source === "draft" ? "草稿 Key（未保存）" : "已保存 Key"}
                    </Badge>
                  </div>
                  <dl className="grid grid-cols-2 gap-x-4 gap-y-3">
                    {quotaFields.map(({ key, label }) => (
                      <div key={key} className="flex flex-col gap-1">
                        <dt className="text-muted-foreground text-xs">{label}</dt>
                        <dd className="break-all font-medium text-base tabular-nums">
                          {accountCheck.account[key] === null
                            ? "未提供"
                            : quotaNumber.format(accountCheck.account[key])}
                        </dd>
                      </div>
                    ))}
                  </dl>
                  <p className="text-muted-foreground text-xs">
                    读取时间：
                    <time dateTime={accountCheck.checked_at}>
                      {new Date(accountCheck.checked_at).toLocaleString("zh-CN", {
                        hour12: false,
                        timeZoneName: "short",
                      })}
                    </time>
                  </p>
                  <p className="text-muted-foreground text-xs leading-relaxed">
                    额度是读取时的账户快照，仅手动刷新。上游未提供的项目显示「未提供」，不会按零计算。
                  </p>
                </section>
              )}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
