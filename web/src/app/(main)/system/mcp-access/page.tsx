"use client";

import * as React from "react";

import { Copy, Plus, RefreshCw } from "lucide-react";
import { toast } from "sonner";

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
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import {
  type MCPAccessCalls,
  type MCPAccessKey,
  type MCPAccessOverview,
  mcpClientConfig,
  mcpScopeGroups,
} from "@/lib/mcp-access";

const time = (value: string | null) => (value ? new Date(value).toLocaleString("zh-CN") : "尚未使用");
const statusNames: Record<string, string> = {
  running: "执行中",
  succeeded: "成功",
  failed: "失败",
  unknown: "结果未知",
};
const actionNames: Record<string, string> = {
  read: "读取",
  write: "修改",
  create: "创建",
  control: "控制",
  message: "发送消息",
  stop: "停止",
  upload: "上传",
  download: "下载",
  delete: "删除",
  test: "测试",
};

async function copy(value: string) {
  try {
    await navigator.clipboard.writeText(value);
    toast.success("已复制");
  } catch {
    toast.error("复制失败，请手动选择文本复制");
  }
}

export default function MCPAccessPage() {
  const [data, setData] = React.useState<MCPAccessOverview | null>(null);
  const [tab, setTab] = React.useState("connect");
  const [client, setClient] = React.useState("codex");
  const [endpoint, setEndpoint] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [search, setSearch] = React.useState("");
  const [creating, setCreating] = React.useState(false);
  const [name, setName] = React.useState("");
  const [days, setDays] = React.useState(30);
  const [scopes, setScopes] = React.useState<string[]>([]);
  const [saving, setSaving] = React.useState(false);
  const [secret, setSecret] = React.useState("");
  const [revoking, setRevoking] = React.useState<MCPAccessKey | null>(null);
  const [calls, setCalls] = React.useState<MCPAccessCalls | null>(null);
  const [callError, setCallError] = React.useState("");
  const [callsLoading, setCallsLoading] = React.useState(false);
  const [keyFilter, setKeyFilter] = React.useState("");
  const [toolFilter, setToolFilter] = React.useState("");
  const [statusFilter, setStatusFilter] = React.useState("");
  const [from, setFrom] = React.useState("");
  const [until, setUntil] = React.useState("");
  const [page, setPage] = React.useState(1);
  const [revision, setRevision] = React.useState(0);
  const refreshing = React.useRef(false);

  const load = React.useCallback(async () => {
    if (refreshing.current) return;
    refreshing.current = true;
    setLoading(true);
    try {
      setData(await api.mcpAccessOverview());
      setError("");
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
      refreshing.current = false;
    }
  }, []);

  React.useEffect(() => {
    const base =
      process.env.NEXT_PUBLIC_API_BASE ??
      (process.env.NODE_ENV === "production"
        ? window.location.origin
        : `${window.location.protocol}//${window.location.hostname}:8787`);
    setEndpoint(`${base.replace(/\/api\/?$/, "").replace(/\/$/, "")}/mcp`);
    void load();
  }, [load]);

  React.useEffect(() => {
    if (tab !== "calls") return;
    let active = true;
    const query = new URLSearchParams({ page: String(page), refresh: String(revision) });
    if (keyFilter) query.set("key_id", keyFilter);
    if (toolFilter) query.set("tool", toolFilter);
    if (statusFilter) query.set("status", statusFilter);
    if (from) query.set("from", new Date(from).toISOString());
    if (until) query.set("until", new Date(until).toISOString());
    setCallsLoading(true);
    setCallError("");
    void api
      .mcpAccessCalls(query)
      .then((result) => {
        if (active) setCalls(result);
      })
      .catch((e: Error) => {
        if (active) setCallError(e.message);
      })
      .finally(() => {
        if (active) setCallsLoading(false);
      });
    return () => {
      active = false;
    };
  }, [tab, page, keyFilter, toolFilter, statusFilter, from, until, revision]);

  const config = mcpClientConfig(client, endpoint);
  const instructions = `使用 HTTP MCP 接入 ARTEX：${endpoint}\n1. 在「调用密钥」创建所需权限和有效期的密钥。\n2. 将密钥放入客户端进程的 ARTEX_MCP_KEY 环境变量。\n3. 安装以下 ${client === "codex" ? "Codex" : "Cursor"} 配置并重新连接。\n${config}\n4. 先调用 artex_context 核对服务与权限。写操作必须提供 request_id；结果未知先查询 artex_request_status，重试保持原参数和原标识。`;
  const filteredTools =
    data?.tools.filter((tool) =>
      `${tool.name} ${tool.description} ${tool.scope}`.toLowerCase().includes(search.toLowerCase()),
    ) ?? [];

  async function createKey() {
    setSaving(true);
    try {
      const result = await api.mcpAccessCreateKey(name, scopes, days);
      setSecret(result.token);
      setCreating(false);
      setName("");
      setScopes([]);
      await load();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  async function revoke() {
    if (!revoking) return;
    setSaving(true);
    try {
      await api.mcpAccessRevokeKey(revoking.id);
      setRevoking(null);
      toast.success("密钥已撤销");
      await load();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div>
          <h1 className="font-semibold text-2xl tracking-tight">MCP 管理</h1>
          <p className="mt-1 text-muted-foreground text-sm">连接外部 Agent，管理调用权限与密钥</p>
        </div>
        <div className="flex gap-2">
          <Button disabled={!endpoint} onClick={() => void copy(instructions)}>
            <Copy className="size-4" />
            复制接入说明
          </Button>
          <Button
            variant="outline"
            disabled={loading || callsLoading}
            onClick={() => {
              void load();
              setRevision((v) => v + 1);
            }}
          >
            <RefreshCw className="size-4" />
            刷新
          </Button>
        </div>
      </div>
      {error && (
        <p role="alert" className="rounded-md border border-destructive/30 p-3 text-destructive text-sm">
          读取失败：{error}。请重试刷新。
        </p>
      )}
      {loading && !data && (
        <p role="status" className="text-muted-foreground text-sm">
          正在读取 MCP 配置…
        </p>
      )}
      <Tabs value={tab} onValueChange={setTab} className="gap-6">
        <TabsList variant="line" className="w-full justify-start border-b pb-2">
          <TabsTrigger value="connect">MCP 接入</TabsTrigger>
          <TabsTrigger value="tools">工具参考</TabsTrigger>
          <TabsTrigger value="keys">调用密钥</TabsTrigger>
          <TabsTrigger value="calls">调用记录</TabsTrigger>
        </TabsList>
        <TabsContent value="connect">
          <Card>
            <CardContent className="space-y-8 pt-6">
              <div className="grid gap-8 lg:grid-cols-2">
                <div className="space-y-5">
                  <h2 className="font-semibold text-xl">连接你的 ARTEX Agent</h2>
                  <p className="text-muted-foreground leading-relaxed">
                    通过 HTTP MCP 管理已授权的任务、对话、资产和工作空间。
                  </p>
                  <ol className="list-inside list-decimal space-y-4 leading-relaxed">
                    <li>
                      创建调用密钥，选择权限和有效期。
                      <Button variant="link" onClick={() => setTab("keys")}>
                        管理密钥
                      </Button>
                    </li>
                    <li>
                      复制配置，将密钥放入客户端环境变量 <code className="text-xs">ARTEX_MCP_KEY</code>。
                    </li>
                    <li>
                      连接后先调用 <code>artex_context</code> 核对服务与权限。
                    </li>
                  </ol>
                  <div className="space-y-2 border-t pt-5">
                    <Label htmlFor="mcp-endpoint">当前服务 MCP 地址</Label>
                    <div className="flex gap-2">
                      <Input
                        id="mcp-endpoint"
                        value={endpoint}
                        onChange={(e) => setEndpoint(e.target.value)}
                        className="font-mono text-xs"
                      />
                      <Button variant="outline" onClick={() => void copy(endpoint)} disabled={!endpoint}>
                        复制地址
                      </Button>
                    </div>
                    <p className="text-muted-foreground text-xs">
                      可改为客户端能访问的 HTTPS 反向代理地址；此处仅生成接入配置。
                    </p>
                  </div>
                </div>
                <div className="min-w-0 space-y-3">
                  <Tabs value={client} onValueChange={setClient}>
                    <TabsList>
                      <TabsTrigger value="cursor">Cursor</TabsTrigger>
                      <TabsTrigger value="codex">Codex</TabsTrigger>
                    </TabsList>
                  </Tabs>
                  <div className="overflow-hidden rounded-lg border">
                    <div className="flex items-center justify-between gap-2 border-b px-4 py-2">
                      <span className="font-medium">{client === "codex" ? "Codex" : "Cursor"} MCP 配置</span>
                      <Button size="sm" variant="ghost" onClick={() => void copy(config)}>
                        复制配置
                      </Button>
                    </div>
                    <pre className="overflow-x-auto bg-muted/30 p-4 text-xs leading-7">{config}</pre>
                  </div>
                  <p className="text-muted-foreground text-xs leading-relaxed">
                    配置只使用环境变量占位符，不插入新密钥。客户端进程必须能读取该变量，设置后请重新启动客户端。
                  </p>
                  <p className="text-muted-foreground text-xs">
                    复制配置不代表连接成功；最近使用时间可在「调用密钥」查看。
                  </p>
                </div>
              </div>
              <div className="space-y-3 border-t pt-5">
                <h3 className="font-medium">工具与执行规则 · {data ? data.tools.length + 2 : "—"} 个工具</h3>
                <ul className="list-inside list-disc space-y-2 text-muted-foreground text-sm leading-relaxed">
                  <li>工具由密钥权限决定；权限覆盖当前实例中的相应资源。</li>
                  <li>写工具必须提供 request_id；结果未知先查状态，重试使用原参数、原标识。</li>
                  <li>重复请求只返回状态元数据，不重复执行；运行中或未知状态需查询业务资源确认。</li>
                  <li>创建任务、发送对话和模型测试可能消耗额度；文件传输上限 8 MiB。</li>
                  <li>调用记录保存工具、时间和结果状态，不保存原始参数、结果或密钥。</li>
                </ul>
              </div>
            </CardContent>
          </Card>
        </TabsContent>
        <TabsContent value="tools" className="space-y-4">
          <Input
            aria-label="搜索工具"
            placeholder="搜索工具名称、功能或权限…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
          <div className="rounded-lg border p-4 text-sm leading-7">
            <code className="font-semibold">artex_context</code>：核对服务与权限，无参数。
            <br />
            <code className="font-semibold">artex_request_status</code>：查询当前密钥的写操作，参数{" "}
            <code>{'{"request_id":"your-operation-id"}'}</code>。
          </div>
          {Object.entries(mcpScopeGroups).map(([group, label]) => {
            const tools = filteredTools.filter((tool) => tool.scope.startsWith(`${group}.`));
            return (
              tools.length > 0 && (
                <section key={group} className="space-y-2">
                  <h2 className="font-semibold">
                    {label} <span className="text-muted-foreground text-sm">{tools.length}</span>
                  </h2>
                  <div className="divide-y rounded-lg border">
                    {tools.map((tool) => (
                      <details key={tool.name} className="p-4">
                        <summary className="cursor-pointer space-x-2 break-words">
                          <code className="text-xs">{tool.name}</code>
                          <Badge variant={tool.write ? "secondary" : "outline"}>{tool.write ? "写入" : "只读"}</Badge>
                          <span className="text-muted-foreground text-xs">{tool.scope}</span>
                        </summary>
                        <p className="mt-3 text-muted-foreground leading-relaxed">{tool.description}</p>
                        <pre className="mt-3 overflow-auto rounded-md bg-muted/40 p-3 text-xs">
                          {JSON.stringify(tool.input_schema, null, 2)}
                        </pre>
                      </details>
                    ))}
                  </div>
                </section>
              )
            );
          })}
          {data && filteredTools.length === 0 && (
            <p className="py-8 text-center text-muted-foreground">没有匹配的工具，请更换关键词。</p>
          )}
        </TabsContent>
        <TabsContent value="keys" className="space-y-4">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-muted-foreground">完整密钥仅在创建时显示一次，可随时撤销。</p>
            <Button disabled={!data} onClick={() => setCreating(true)}>
              <Plus className="size-4" />
              创建调用密钥
            </Button>
          </div>
          {data?.keys.length === 0 && (
            <Card>
              <CardContent className="py-12 text-center text-muted-foreground">
                还没有调用密钥。创建密钥并选择 Agent 所需的权限，即可开始接入。
              </CardContent>
            </Card>
          )}
          {data?.keys.map((key) => {
            let state = "有效";
            if (new Date(key.expires_at).getTime() <= Date.now()) state = "已过期";
            if (key.revoked_at) state = "已撤销";
            return (
              <Card key={key.id}>
                <CardHeader className="flex flex-row items-start justify-between gap-3">
                  <div className="min-w-0 space-y-2">
                    <CardTitle className="break-words">
                      {key.name} <Badge variant={state === "有效" ? "default" : "secondary"}>{state}</Badge>
                    </CardTitle>
                    <CardDescription>
                      {key.prefix}… · #{key.id}
                    </CardDescription>
                  </div>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={Boolean(key.revoked_at)}
                    onClick={() => setRevoking(key)}
                  >
                    撤销
                  </Button>
                </CardHeader>
                <CardContent className="space-y-4">
                  <div className="flex flex-wrap gap-2">
                    {key.scopes.map((scope) => (
                      <Badge key={scope} variant="outline">
                        {scope}
                      </Badge>
                    ))}
                  </div>
                  <div className="grid gap-2 text-muted-foreground text-xs sm:grid-cols-3">
                    <span>创建：{time(key.created_at)}</span>
                    <span>到期：{time(key.expires_at)}</span>
                    <span>最近使用：{time(key.last_used_at)}</span>
                  </div>
                </CardContent>
              </Card>
            );
          })}
        </TabsContent>
        <TabsContent value="calls" className="space-y-4">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-5">
            <label className="space-y-1 text-xs">
              调用密钥
              <select
                aria-label="筛选调用密钥"
                className="h-9 w-full rounded-md border bg-background px-2 text-sm"
                value={keyFilter}
                onChange={(e) => {
                  setKeyFilter(e.target.value);
                  setPage(1);
                }}
              >
                <option value="">全部密钥</option>
                {data?.keys.map((key) => (
                  <option key={key.id} value={key.id}>
                    {key.name}
                  </option>
                ))}
              </select>
            </label>
            <label htmlFor="call-tool" className="space-y-1 text-xs">
              工具
              <Input
                id="call-tool"
                value={toolFilter}
                placeholder="精确工具名称"
                onChange={(e) => {
                  setToolFilter(e.target.value);
                  setPage(1);
                }}
              />
            </label>
            <label className="space-y-1 text-xs">
              状态
              <select
                aria-label="筛选调用状态"
                className="h-9 w-full rounded-md border bg-background px-2 text-sm"
                value={statusFilter}
                onChange={(e) => {
                  setStatusFilter(e.target.value);
                  setPage(1);
                }}
              >
                <option value="">全部状态</option>
                {Object.entries(statusNames).map(([value, label]) => (
                  <option key={value} value={value}>
                    {label}
                  </option>
                ))}
              </select>
            </label>
            <label htmlFor="call-from" className="space-y-1 text-xs">
              开始时间
              <Input
                id="call-from"
                type="datetime-local"
                value={from}
                onChange={(e) => {
                  setFrom(e.target.value);
                  setPage(1);
                }}
              />
            </label>
            <label htmlFor="call-until" className="space-y-1 text-xs">
              结束时间
              <Input
                id="call-until"
                type="datetime-local"
                value={until}
                onChange={(e) => {
                  setUntil(e.target.value);
                  setPage(1);
                }}
              />
            </label>
          </div>
          {callError && (
            <p role="alert" className="text-destructive">
              读取记录失败：{callError}
            </p>
          )}
          {callsLoading ? (
            <p role="status" className="py-8 text-center text-muted-foreground">
              正在读取调用记录…
            </p>
          ) : (
            !callError && (
              <div className="overflow-x-auto rounded-lg border">
                <table className="w-full whitespace-nowrap text-left text-sm">
                  <thead className="bg-muted/40">
                    <tr>
                      {["时间", "密钥", "工具", "请求标识", "结果", "耗时"].map((label) => (
                        <th key={label} className="p-3 font-medium">
                          {label}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {calls?.calls.map((call) => (
                      <tr key={call.id} className="border-t">
                        <td className="p-3">{time(call.created_at)}</td>
                        <td className="p-3">
                          {data?.keys.find((key) => key.id === call.key_id)?.name ?? `#${call.key_id}`}
                        </td>
                        <td className="p-3 font-mono text-xs">{call.tool}</td>
                        <td className="p-3 font-mono text-xs">{call.request_id || "—"}</td>
                        <td className="p-3">
                          {statusNames[call.status] ?? call.status}
                          {call.status_code > 0 && ` (${call.status_code})`}
                        </td>
                        <td className="p-3">{call.duration_ms} ms</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {calls?.calls.length === 0 && (
                  <p className="p-10 text-center text-muted-foreground">没有符合条件的调用记录。</p>
                )}
              </div>
            )
          )}
          <div className="flex items-center justify-between gap-2 text-muted-foreground text-sm">
            <span>
              共 {calls?.total ?? 0} 条 · 第 {page} 页
            </span>
            <div className="flex gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={page === 1 || callsLoading}
                onClick={() => setPage((v) => v - 1)}
              >
                上一页
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={!calls || page * calls.page_size >= calls.total || callsLoading}
                onClick={() => setPage((v) => v + 1)}
              >
                下一页
              </Button>
            </div>
          </div>
        </TabsContent>
      </Tabs>

      <Dialog
        open={creating}
        onOpenChange={(open) => {
          if (!saving) setCreating(open);
        }}
      >
        <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>创建调用密钥</DialogTitle>
            <DialogDescription>选择所需权限。权限适用于当前实例中的全部对应资源。</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 sm:grid-cols-2">
            <div className="space-y-2">
              <Label htmlFor="key-name">名称</Label>
              <Input
                id="key-name"
                maxLength={80}
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="例如：我的 Codex"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="key-days">有效期（天）</Label>
              <Input
                id="key-days"
                type="number"
                min={1}
                max={365}
                value={days}
                onChange={(e) => setDays(Number(e.target.value))}
              />
            </div>
          </div>
          <div className="flex gap-2">
            <Button
              variant="outline"
              size="sm"
              onClick={() => setScopes(data?.scopes.filter((scope) => scope.endsWith(".read")) ?? [])}
            >
              选择只读权限
            </Button>
            <Button variant="ghost" size="sm" onClick={() => setScopes([])}>
              清空
            </Button>
          </div>
          <div className="space-y-4">
            {Object.entries(mcpScopeGroups).map(([group, label]) => (
              <fieldset key={group} className="space-y-2">
                <legend className="font-medium text-sm">{label}</legend>
                <div className="flex flex-wrap gap-x-5 gap-y-3">
                  {data?.scopes
                    .filter((scope) => scope.startsWith(`${group}.`))
                    .map((scope) => (
                      <label
                        key={scope}
                        htmlFor={`scope-${scope}`}
                        className="flex cursor-pointer items-center gap-2 text-sm"
                      >
                        <Checkbox
                          id={`scope-${scope}`}
                          checked={scopes.includes(scope)}
                          onCheckedChange={(checked) =>
                            setScopes((old) => (checked ? [...old, scope] : old.filter((item) => item !== scope)))
                          }
                        />
                        {actionNames[scope.split(".")[1]] ?? scope}
                        <code className="text-muted-foreground text-xs">{scope}</code>
                      </label>
                    ))}
                </div>
              </fieldset>
            ))}
          </div>
          <DialogFooter>
            <Button variant="outline" disabled={saving} onClick={() => setCreating(false)}>
              取消
            </Button>
            <Button
              disabled={
                saving || !name.trim() || scopes.length === 0 || !Number.isInteger(days) || days < 1 || days > 365
              }
              onClick={() => void createKey()}
            >
              {saving ? "创建中…" : "创建密钥"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog
        open={Boolean(secret)}
        onOpenChange={(open) => {
          if (!open) setSecret("");
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>保存你的调用密钥</DialogTitle>
            <DialogDescription>完整密钥仅显示这一次。请保存到客户端的 ARTEX_MCP_KEY 环境变量中。</DialogDescription>
          </DialogHeader>
          <pre className="select-all whitespace-pre-wrap break-all rounded-md border bg-muted/40 p-4 text-sm">
            {secret}
          </pre>
          <DialogFooter>
            <Button variant="outline" onClick={() => void copy(secret)}>
              复制密钥
            </Button>
            <Button onClick={() => setSecret("")}>已保存，关闭</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog
        open={Boolean(revoking)}
        onOpenChange={(open) => {
          if (!open && !saving) setRevoking(null);
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>撤销「{revoking?.name}」？</DialogTitle>
            <DialogDescription>撤销后此密钥不能再发起调用。已经开始执行的业务操作不会自动停止。</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" disabled={saving} onClick={() => setRevoking(null)}>
              取消
            </Button>
            <Button variant="destructive" disabled={saving} onClick={() => void revoke()}>
              {saving ? "撤销中…" : "撤销密钥"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
