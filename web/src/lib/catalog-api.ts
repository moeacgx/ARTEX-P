import { http } from "./api";

export type CatalogKind = "skill" | "tool" | "agent";
export type CatalogSource = { repository: string; ref: string; directory: string };
export type CatalogItem = {
  kind: CatalogKind;
  id: string;
  name: string;
  description: string;
  version: string;
  license: string;
  files: Record<string, string>;
  tools?: string[];
  skills?: string[];
  installed_version?: string;
  status: "available" | "installed" | "update" | "conflict";
  local_hash?: string;
  can_rollback?: boolean;
};
export type CatalogList = { source: CatalogSource; commit: string; items: CatalogItem[] };
export type CatalogPreview = {
  item: CatalogItem;
  commit: string;
  local_hash: string;
  source_hash: string;
  contents?: Record<string, string>;
  local_contents?: Record<string, string>;
  conflict: boolean;
  changes: string[];
  affected_agents: string[];
  missing_dependencies: string[];
};

export const catalogAPI = {
  source: () => http<CatalogSource>("/catalog/source"),
  saveSource: (source: CatalogSource) =>
    http<CatalogSource>("/catalog/source", { method: "PUT", body: JSON.stringify(source) }),
  list: (kind: CatalogKind) => http<CatalogList>(`/catalog?kind=${kind}`, { cache: "no-store" }),
  preview: (item: CatalogItem, commit: string) =>
    http<CatalogPreview>("/catalog/preview", {
      method: "POST",
      body: JSON.stringify({ kind: item.kind, id: item.id, commit }),
    }),
  install: (preview: CatalogPreview) =>
    http<unknown>("/catalog/install", {
      method: "POST",
      body: JSON.stringify({
        kind: preview.item.kind,
        id: preview.item.id,
        commit: preview.commit,
        expected_local_hash: preview.local_hash,
        expected_source_hash: preview.source_hash,
      }),
    }),
  rollback: (item: CatalogItem) =>
    http<unknown>("/catalog/rollback", {
      method: "POST",
      body: JSON.stringify({ kind: item.kind, id: item.id, expected_local_hash: item.local_hash }),
    }),
};

export async function downloadCatalogItem(item: CatalogItem, commit: string): Promise<void> {
  const token = localStorage.getItem("artex_token");
  const query = new URLSearchParams({ kind: item.kind, id: item.id, commit });
  const response = await fetch(`/api/catalog/download?${query}`, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    cache: "no-store",
  });
  if (response.status === 401) {
    localStorage.removeItem("artex_token");
    // biome-ignore lint/suspicious/noDocumentCookie: 与现有登录流程保持一致，兼容未支持 Cookie Store API 的浏览器。
    document.cookie = "artex_token=; path=/; max-age=0";
    window.location.href = "/login";
    throw new Error("未授权");
  }
  if (!response.ok) {
    const body = (await response.json().catch(() => null)) as { error?: string } | null;
    throw new Error(body?.error || `下载失败（${response.status}）`);
  }
  if (!response.headers.get("Content-Type")?.toLowerCase().startsWith("application/zip")) {
    throw new Error("下载失败：服务端未返回 ZIP 压缩包");
  }
  const url = URL.createObjectURL(await response.blob());
  const anchor = document.createElement("a");
  try {
    anchor.href = url;
    anchor.download = `${item.id}-${item.version}.zip`;
    document.body.appendChild(anchor);
    anchor.click();
  } finally {
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
}
