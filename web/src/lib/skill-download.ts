// 下载只读取服务端已保存的技能目录，不提交编辑器草稿。
export async function downloadSkillArchive(name: string, token: string | null): Promise<void> {
  const response = await fetch(`/api/skills/${encodeURIComponent(name)}/download`, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    cache: "no-store",
  });
  if (response.status === 401) {
    localStorage.removeItem("artex_token");
    document.cookie = "artex_token=; path=/; max-age=0";
    window.location.href = "/login";
    throw new Error("未授权");
  }
  if (!response.ok) {
    const body: unknown = await response.json().catch(() => null);
    const error = body && typeof body === "object" && "error" in body ? body.error : null;
    throw new Error(typeof error === "string" && error.trim() ? error : `下载失败(${response.status})`);
  }
  if (!response.headers.get("Content-Type")?.toLowerCase().startsWith("application/zip")) {
    throw new Error("下载失败：服务端未返回 ZIP 压缩包");
  }
  const url = URL.createObjectURL(await response.blob());
  const anchor = document.createElement("a");
  try {
    anchor.href = url;
    anchor.download = `${name}.zip`;
    document.body.appendChild(anchor);
    anchor.click();
  } finally {
    anchor.remove();
    // 给浏览器读取下载链接留出时间，随后释放内存。
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
}
