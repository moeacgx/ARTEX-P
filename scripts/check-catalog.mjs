import { createHash } from "node:crypto";
import { lstat, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

// 目录包以原始字节校验，Git 属性固定为 LF，避免跨平台签入后摘要失效。
const root = fileURLToPath(new URL("../catalog/", import.meta.url));
const manifestPath = path.join(root, "catalog.json");
const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
const write = process.argv.includes("--write");
if (manifest.schema_version !== 1 || !Array.isArray(manifest.items)) {
  throw new Error("目录清单格式不正确");
}
const seen = new Set();
for (const item of manifest.items) {
  if (!["skill", "tool", "agent"].includes(item.kind) || !/^[a-z][a-z0-9_-]*$/.test(item.id)) {
    throw new Error(`目录条目标识不正确：${item.kind}/${item.id}`);
  }
  const key = `${item.kind}/${item.id}`;
  if (seen.has(key)) throw new Error(`目录条目重复：${key}`);
  seen.add(key);
  for (const [name, expected] of Object.entries(item.files)) {
    if (name.includes("\\") || name.split("/").some((part) => !part || part === "." || part === "..")) {
      throw new Error(`文件路径不正确：${key}/${name}`);
    }
    const file = path.join(root, `${item.kind}s`, item.id, name);
    if (!(await lstat(file)).isFile()) throw new Error(`目录文件不是普通文件：${file}`);
    const sha = createHash("sha256").update(await readFile(file)).digest("hex");
    if (write) item.files[name] = sha;
    else if (expected !== sha) throw new Error(`文件摘要不匹配：${key}/${name}，请更新清单摘要`);
  }
}
if (write) await writeFile(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`);
console.log(`已${write ? "更新" : "核验"} ${seen.size} 个目录条目`);
