"use client";

import * as React from "react";

import { DownloadIcon, GlobeIcon, RefreshCwIcon, RotateCcwIcon, SearchIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  type CatalogItem,
  type CatalogKind,
  type CatalogList,
  type CatalogPreview,
  type CatalogSource,
  catalogAPI,
  downloadCatalogItem,
} from "@/lib/catalog-api";

const kindNames: Record<CatalogKind, string> = { skill: "Skills", tool: "工具", agent: "Agent" };
const statusNames: Record<CatalogItem["status"], string> = {
  available: "未安装",
  installed: "已安装",
  update: "可更新",
  conflict: "本地有修改",
};

export function CatalogPanel({
  kind,
  onInstalled,
  disabled = false,
}: {
  kind: CatalogKind;
  onInstalled: () => void;
  disabled?: boolean;
}) {
  const [open, setOpen] = React.useState(false);
  const [data, setData] = React.useState<CatalogList | null>(null);
  const [source, setSource] = React.useState<CatalogSource | null>(null);
  const [query, setQuery] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [preview, setPreview] = React.useState<CatalogPreview | null>(null);
  const [rollbackItem, setRollbackItem] = React.useState<CatalogItem | null>(null);
  const requestID = React.useRef(0);
  const id = React.useId();

  const load = React.useCallback(async () => {
    const current = ++requestID.current;
    setBusy(true);
    setError("");
    setPreview(null);
    setRollbackItem(null);
    // 先读取本地来源，远端连接失败时仍能编辑来源并重试。
    try {
      const saved = await catalogAPI.source();
      if (current !== requestID.current) return;
      setSource(saved);
      const result = await catalogAPI.list(kind);
      if (current !== requestID.current) return;
      setData(result);
    } catch (e) {
      if (current === requestID.current) {
        setData(null);
        setError((e as Error).message);
      }
    } finally {
      if (current === requestID.current) setBusy(false);
    }
  }, [kind]);

  React.useEffect(() => {
    if (open) void load();
    return () => {
      ++requestID.current;
    };
  }, [open, load]);

  async function action(run: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      await run();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const sourceChanged =
    !!source &&
    !!data &&
    (source.repository !== data.source.repository ||
      source.ref !== data.source.ref ||
      source.directory !== data.source.directory);
  const items = (data?.items ?? []).filter((item) =>
    `${item.name} ${item.id} ${item.description}`.toLowerCase().includes(query.trim().toLowerCase()),
  );
  const blocked = !!preview && (preview.conflict || (preview.missing_dependencies ?? []).length > 0);

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!busy) setOpen(next);
      }}
    >
      <DialogTrigger asChild>
        <Button
          size="sm"
          variant="outline"
          disabled={disabled}
          title={disabled ? "请先保存或放弃当前编辑器的修改" : undefined}
        >
          <GlobeIcon className="size-4" />
          在线目录
        </Button>
      </DialogTrigger>
      <DialogContent className="flex max-h-[90dvh] flex-col overflow-hidden sm:max-w-5xl" showCloseButton={!busy}>
        <DialogHeader>
          <DialogTitle>{kindNames[kind]} 在线目录</DialogTitle>
          <DialogDescription>从 GitHub 仓库获取版本，预览变更后安装或更新。仓库内容也可打包下载。</DialogDescription>
        </DialogHeader>
        <div className="min-h-0 space-y-4 overflow-y-auto pr-1">
          {source && (
            <form
              className="space-y-3 rounded-lg border p-3"
              onSubmit={(e) => {
                e.preventDefault();
                void action(async () => {
                  await catalogAPI.saveSource({
                    repository: source.repository.trim(),
                    ref: source.ref.trim(),
                    directory: source.directory.trim(),
                  });
                  setData(null);
                  await load();
                });
              }}
            >
              <div className="grid gap-3 sm:grid-cols-[2fr_1fr_1fr]">
                {(["repository", "ref", "directory"] as const).map((field) => (
                  <div key={field} className="space-y-1.5">
                    <Label htmlFor={`${id}-${field}`}>
                      {{ repository: "GitHub 仓库", ref: "分支或提交", directory: "目录路径" }[field]}
                    </Label>
                    <Input
                      id={`${id}-${field}`}
                      value={source[field]}
                      disabled={busy}
                      required
                      placeholder={field === "repository" ? "owner/repository" : undefined}
                      onChange={(e) => {
                        setSource({ ...source, [field]: e.target.value });
                        setPreview(null);
                        setRollbackItem(null);
                      }}
                    />
                  </div>
                ))}
              </div>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-muted-foreground text-xs">在仓库提交内容后点击拉取；来源由三个目录页面共用。</p>
                <div className="flex gap-2">
                  <Button
                    type="submit"
                    size="sm"
                    disabled={busy || !source.repository.trim() || !source.ref.trim() || !source.directory.trim()}
                  >
                    保存来源并拉取
                  </Button>
                  <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void load()}>
                    <RefreshCwIcon className={busy ? "size-3.5 animate-spin" : "size-3.5"} />
                    拉取更新
                  </Button>
                </div>
              </div>
            </form>
          )}
          {error && (
            <div role="alert" className="rounded-md border border-destructive/30 p-3 text-destructive text-sm">
              {error}
              <Button variant="link" size="sm" disabled={busy} onClick={() => void load()}>
                重新拉取
              </Button>
            </div>
          )}
          {busy && (
            <p role="status" className="text-muted-foreground text-sm">
              正在处理，请稍候…
            </p>
          )}
          {sourceChanged && <p className="text-amber-600 text-sm">来源已修改，请先保存并拉取后再操作目录内容。</p>}
          {data && (
            <>
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="relative w-full sm:w-80">
                  <SearchIcon className="absolute top-2.5 left-2.5 size-4 text-muted-foreground" />
                  <Input
                    className="pl-8"
                    aria-label="搜索在线目录"
                    placeholder="搜索名称、标识或描述"
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                  />
                </div>
                <a
                  className="break-all font-mono text-muted-foreground text-xs underline"
                  href={`https://github.com/${data.source.repository}/commit/${data.commit}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  提交 {data.commit.slice(0, 12)}
                </a>
              </div>
              {items.length === 0 && (
                <p className="py-6 text-center text-muted-foreground text-sm">
                  {query ? "没有匹配的目录项" : "该仓库暂未提供此类目录项"}
                </p>
              )}
              <div className="grid gap-3 sm:grid-cols-2">
                {items.map((item) => (
                  <div key={item.id} className="flex flex-col gap-3 rounded-lg border p-3">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-medium">{item.name}</span>
                      <Badge variant="outline">{statusNames[item.status]}</Badge>
                      <span className="text-muted-foreground text-xs">v{item.version}</span>
                    </div>
                    <p className="break-words text-muted-foreground text-sm">{item.description || "暂无描述"}</p>
                    <p className="text-muted-foreground text-xs">
                      {item.id} · 许可：{item.license || "未声明"}
                      {item.installed_version ? ` · 已安装 v${item.installed_version}` : ""}
                    </p>
                    <div className="mt-auto flex flex-wrap gap-2">
                      <Button
                        size="sm"
                        disabled={busy || sourceChanged}
                        onClick={() =>
                          void action(async () => {
                            setRollbackItem(null);
                            setPreview(await catalogAPI.preview(item, data.commit));
                          })
                        }
                      >
                        详情与变更预览
                      </Button>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busy || sourceChanged}
                        onClick={() =>
                          void action(async () => {
                            await downloadCatalogItem(item, data.commit);
                            toast.success("下载已开始");
                          })
                        }
                      >
                        <DownloadIcon className="size-3.5" />
                        下载 ZIP
                      </Button>
                      {item.can_rollback && (
                        <Button
                          size="sm"
                          variant="ghost"
                          disabled={busy || sourceChanged || !item.local_hash}
                          onClick={() => {
                            setPreview(null);
                            setRollbackItem(item);
                          }}
                        >
                          <RotateCcwIcon className="size-3.5" />
                          回滚
                        </Button>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            </>
          )}
          {preview && (
            <section className="space-y-3 rounded-lg border p-4" aria-label="安装变更预览">
              <h3 className="font-semibold">
                {preview.item.name} · v{preview.item.version}
              </h3>
              <p className="text-muted-foreground text-sm">{preview.item.description}</p>
              <p className="break-all font-mono text-xs">提交 SHA：{preview.commit}</p>
              <p className="text-muted-foreground text-xs">许可：{preview.item.license || "未声明"}</p>
              <div className="space-y-1 text-sm">
                <p className="font-medium">变更内容</p>
                {[...new Set(preview.changes ?? [])].map((change) => (
                  <p key={change} className="break-all font-mono text-xs">
                    {change}
                  </p>
                ))}
              </div>
              <p className="text-sm">影响的 Agent：{(preview.affected_agents ?? []).join("、") || "无"}</p>
              <p className="text-sm">
                依赖 Skills：{(preview.item.skills ?? []).join("、") || "无"}；工具：
                {(preview.item.tools ?? []).join("、") || "无"}
              </p>
              <details className="text-sm">
                <summary className="cursor-pointer">
                  文件与 SHA-256（{Object.keys(preview.item.files ?? {}).length}）
                </summary>
                <div className="mt-2 max-h-40 space-y-1 overflow-auto">
                  {Object.entries(preview.item.files ?? {}).map(([path, hash]) => (
                    <p key={path} className="break-all font-mono text-xs">
                      {path}
                      <br />
                      <span className="text-muted-foreground">{hash}</span>
                    </p>
                  ))}
                </div>
              </details>
              {Object.entries(preview.contents ?? {}).map(([path, content]) => (
                <details key={path} className="text-sm">
                  <summary className="cursor-pointer break-all">审阅文件：{path}</summary>
                  <div className="mt-2 grid gap-2 md:grid-cols-2">
                    <div>
                      <p className="mb-1 font-medium text-xs">当前本地内容</p>
                      <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-2 font-mono text-xs">
                        {preview.local_contents?.[path] ?? "（尚未安装）"}
                      </pre>
                    </div>
                    <div>
                      <p className="mb-1 font-medium text-xs">仓库内容</p>
                      <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded bg-muted p-2 font-mono text-xs">
                        {content}
                      </pre>
                    </div>
                  </div>
                </details>
              ))}
              {preview.conflict && (
                <p role="alert" className="text-destructive text-sm">
                  检测到本地修改，已阻止覆盖。请先备份并处理本地差异，再重新预览。
                </p>
              )}
              {(preview.missing_dependencies ?? []).length > 0 && (
                <p role="alert" className="text-destructive text-sm">
                  请先安装依赖：{preview.missing_dependencies.join("、")}
                </p>
              )}
              <p className="text-muted-foreground text-xs">
                更新保留实例本地的凭据与绑定；新工具和 Agent 安装后默认停用，请检查配置后启用。
              </p>
              <div className="flex gap-2">
                <Button
                  disabled={busy || blocked || sourceChanged}
                  onClick={() =>
                    void action(async () => {
                      await catalogAPI.install(preview);
                      toast.success("安装完成");
                      onInstalled();
                      await load();
                    })
                  }
                >
                  确认安装 / 更新
                </Button>
                <Button variant="outline" disabled={busy} onClick={() => setPreview(null)}>
                  关闭预览
                </Button>
              </div>
            </section>
          )}
          {rollbackItem && (
            <section className="space-y-3 rounded-lg border p-4" aria-label="回滚确认">
              <h3 className="font-semibold">回滚 {rollbackItem.name}</h3>
              <p className="text-muted-foreground text-sm">
                恢复上一次安装前的版本。若当前本地内容发生修改，服务端会阻止回滚。
              </p>
              <div className="flex gap-2">
                <Button
                  disabled={busy || sourceChanged}
                  onClick={() =>
                    void action(async () => {
                      await catalogAPI.rollback(rollbackItem);
                      toast.success("已恢复上一版本");
                      onInstalled();
                      await load();
                    })
                  }
                >
                  确认回滚
                </Button>
                <Button variant="outline" disabled={busy} onClick={() => setRollbackItem(null)}>
                  取消
                </Button>
              </div>
            </section>
          )}
        </div>
      </DialogContent>
    </Dialog>
  );
}
