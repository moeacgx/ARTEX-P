// 下载编译内嵌的客户端 Skill，不依赖服务器工作目录。
export async function downloadLocalAgentSkill(): Promise<void> {
  const token = localStorage.getItem("artex_token");
  const response = await fetch("/api/local-agent/skill", {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    cache: "no-store",
  });
  if (!response.ok || !response.headers.get("Content-Type")?.startsWith("application/zip")) {
    throw new Error(`下载失败（HTTP ${response.status}），请确认已登录`);
  }
  const url = URL.createObjectURL(await response.blob());
  const anchor = document.createElement("a");
  try {
    anchor.href = url;
    anchor.download = "artex-agent.zip";
    document.body.appendChild(anchor);
    anchor.click();
  } finally {
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
}
