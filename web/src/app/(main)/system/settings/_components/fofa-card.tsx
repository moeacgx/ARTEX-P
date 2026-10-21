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
import { type FofaConfig, fofaAPI, fofaConnectionTestInput, fofaSettingsPatch } from "@/lib/fofa-api";

export function FofaCard() {
  const [config, setConfig] = React.useState<FofaConfig | null>(null);
  const [enabled, setEnabled] = React.useState(false);
  const [apiKey, setApiKey] = React.useState("");
  const [loadError, setLoadError] = React.useState("");
  const [action, setAction] = React.useState<"load" | "save" | "clear" | "test" | null>("load");
  const locked = React.useRef(false);

  const applyConfig = React.useCallback((value: FofaConfig) => {
    setConfig(value);
    setEnabled(value.enabled);
    setApiKey("");
  }, []);

  const load = React.useCallback(async () => {
    if (locked.current) return;
    locked.current = true;
    setAction("load");
    setLoadError("");
    try {
      applyConfig(await fofaAPI.config());
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
      const patch = fofaSettingsPatch(config, enabled, apiKey);
      applyConfig(await fofaAPI.save(patch));
      toast.success("已保存 FOFA 配置");
    } catch (error) {
      toast.error(`保存失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const clear = async () => {
    if (locked.current || !config?.api_key_set) return;
    locked.current = true;
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
    setAction("test");
    try {
      const result = await fofaAPI.test(fofaConnectionTestInput(config, apiKey));
      toast.success(result.message || "FOFA 连接测试成功");
    } catch (error) {
      toast.error(`连接测试失败：${(error as Error).message}`);
    } finally {
      locked.current = false;
      setAction(null);
    }
  };

  const busy = action !== null;
  const hasKey = !!config?.api_key_set || apiKey.trim() !== "";
  const changed = !!config && (enabled !== config.enabled || apiKey.trim() !== "");

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
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <Label htmlFor="fofa-api-key" className="font-normal text-muted-foreground text-sm">
                  API Key
                </Label>
                <Badge variant={config.api_key_set ? "secondary" : "outline"}>
                  {config.api_key_set ? "已配置" : "未配置"}
                </Badge>
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
                onChange={(event) => setApiKey(event.target.value)}
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
            <div className="flex flex-wrap items-center gap-2">
              <Button type="button" onClick={() => void save()} disabled={busy || !changed || (enabled && !hasKey)}>
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
            <div className="flex items-center justify-between gap-4 border-t pt-4">
              <p className="text-muted-foreground text-xs leading-relaxed">
                优先测试当前输入的 Key，留空则使用已保存的 Key。仅检查账号连接，不执行资产搜索，也不会保存输入。
              </p>
              <Button
                type="button"
                variant="outline"
                className="shrink-0"
                onClick={() => void test()}
                disabled={busy || !hasKey}
              >
                {action === "test" ? "测试中…" : "测试连接"}
              </Button>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
