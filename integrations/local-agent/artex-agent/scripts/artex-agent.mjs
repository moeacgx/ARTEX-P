#!/usr/bin/env node
import { createHash, createPrivateKey, generateKeyPairSync, randomBytes, sign } from "node:crypto";
import { spawn } from "node:child_process";
import { chmod, mkdir, readFile, readdir, realpath, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const MAX_UPLOAD_BYTES = 20 * 1024 * 1024 - 4096;
const MAX_WORKSPACE_UPLOAD_BYTES = 512 * 1024 * 1024 - 4096;
const API_PATH = "/api/local-agent";

export function normalizeBaseUrl(input) {
  const url = new URL(String(input).trim());
  const local = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (url.protocol !== "https:" && !(local && url.protocol === "http:")) {
    throw new Error("ARTEX 地址必须使用 HTTPS；仅本机地址允许 HTTP");
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new Error("ARTEX 地址不能包含用户名、密码、查询参数或片段");
  }
  const prefix = url.pathname.replace(/\/+$/, "");
  url.pathname = prefix.endsWith(API_PATH) ? prefix : prefix + API_PATH;
  return url.toString().replace(/\/$/, "");
}

export function configDirectory(env = process.env) {
  if (env.ARTEX_AGENT_CONFIG_DIR) return path.resolve(env.ARTEX_AGENT_CONFIG_DIR);
  return process.platform === "win32"
    ? path.join(env.LOCALAPPDATA ?? os.homedir(), "ARTEX", "local-agent")
    : path.join(env.XDG_CONFIG_HOME ?? path.join(os.homedir(), ".config"), "artex", "local-agent");
}

function sha256(body) { return createHash("sha256").update(body).digest("hex"); }

// 清单只描述包内容，版本始终从本地实际文件计算，避免覆盖不完整却虚报已更新。
export async function getSkillVersion(directory) {
  try {
    const script = fileURLToPath(import.meta.url);
    let root = directory ?? path.resolve(path.dirname(script), "..");
    let files;
    let repository = false;
    try {
      const manifest = JSON.parse(await readFile(path.join(root, "skill-manifest.json"), "utf8"));
      if (!/^[a-f0-9]{64}$/.test(manifest.version) || !Array.isArray(manifest.files)) return undefined;
      files = manifest.files;
    } catch (error) {
      if (directory || error.code !== "ENOENT") return undefined;
      root = path.join(root, "integrations", "local-agent", "artex-agent");
      repository = true;
      files = [];
      async function list(relative = "") {
        for (const entry of await readdir(path.join(root, relative), { withFileTypes: true })) {
          if (/^[._]/.test(entry.name) || entry.name === "skill-manifest.json") continue;
          const name = relative ? `${relative}/${entry.name}` : entry.name;
          if (entry.isDirectory()) await list(name);
          else if (entry.isFile()) files.push(name);
          else throw new Error("技能目录包含非常规文件");
        }
      }
      await list();
    }
    if (!files.includes("SKILL.md") || !files.includes("scripts/artex-agent.mjs") || new Set(files).size !== files.length) return undefined;
    const resolvedRoot = await realpath(root);
    const digest = createHash("sha256");
    for (const file of files.sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)))) {
      if (typeof file !== "string" || !file || file.includes("\\") || file.split("/").some(part => !part || part === "." || part === "..") || path.isAbsolute(file)) return undefined;
      const target = await realpath(path.join(root, file));
      if (!target.startsWith(resolvedRoot + path.sep)) return undefined;
      // 仓库入口必须上报自身运行代码的版本，不能借用尚未同步的内嵌副本。
      const content = await readFile(repository && file === "scripts/artex-agent.mjs" ? script : target);
      const normalized = Buffer.from(content.toString("latin1").replaceAll("\r\n", "\n"), "latin1");
      digest.update(`${file}\0${sha256(normalized)}\n`);
    }
    return digest.digest("hex");
  } catch {
    // 保持旧版单文件用法可用；未知版本由页面提示，不能阻断正常请求。
    return undefined;
  }
}

export function generateIdentity(name) {
  const { publicKey, privateKey } = generateKeyPairSync("ed25519");
  const rawKey = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url");
  return {
    name,
    publicKey: rawKey.toString("base64"),
    fingerprint: sha256(rawKey),
    privateKey: privateKey.export({ type: "pkcs8", format: "pem" }).toString(),
  };
}

export function signedHeaders({ method, url, body = Buffer.alloc(0), identity, timestamp = String(Math.floor(Date.now() / 1000)), nonce = randomBytes(24).toString("base64url") }) {
  const message = [method, `${url.pathname}${url.search}`, timestamp, nonce, sha256(body)].join("\n");
  return {
    "X-ARTEX-Device-ID": String(identity.deviceId),
    "X-ARTEX-Timestamp": timestamp,
    "X-ARTEX-Nonce": nonce,
    "X-ARTEX-Signature": sign(null, Buffer.from(message), identity.privateKey).toString("base64"),
  };
}

async function dpapi(value, decrypt) {
  const operation = decrypt
    ? "$b=[Convert]::FromBase64String([Console]::In.ReadToEnd());$v=[Security.Cryptography.ProtectedData]::Unprotect($b,$null,[Security.Cryptography.DataProtectionScope]::CurrentUser);[Console]::Out.Write([Text.Encoding]::UTF8.GetString($v))"
    : "$b=[Text.Encoding]::UTF8.GetBytes([Console]::In.ReadToEnd());$v=[Security.Cryptography.ProtectedData]::Protect($b,$null,[Security.Cryptography.DataProtectionScope]::CurrentUser);[Console]::Out.Write([Convert]::ToBase64String($v))";
  const script = "$ErrorActionPreference='Stop';Add-Type -AssemblyName System.Security;" + operation;
  return new Promise((resolve, reject) => {
    const child = spawn("powershell.exe", ["-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(script, "utf16le").toString("base64")], { windowsHide: true, stdio: ["pipe", "pipe", "pipe"] });
    const output = [];
    child.stdout.on("data", chunk => output.push(chunk));
    child.stderr.resume();
    child.on("error", () => reject(new Error("无法启动 Windows 设备密钥保护")));
    child.on("close", code => code === 0 ? resolve(Buffer.concat(output).toString()) : reject(new Error("无法使用当前 Windows 用户保护或解密设备身份")));
    child.stdin.on("error", () => {});
    child.stdin.end(value);
  });
}

export class IdentityStore {
  constructor(directory = configDirectory()) { this.directory = directory; }
  get file() { return path.join(this.directory, process.platform === "win32" ? "identity.dpapi" : "identity.json"); }
  async load() {
    let content;
    try { content = await readFile(this.file, "utf8"); }
    catch (error) { if (error.code === "ENOENT") return null; throw error; }
    if (process.platform === "win32") content = await dpapi(content, true);
    const identity = JSON.parse(content);
    normalizeBaseUrl(identity.baseUrl);
    createPrivateKey(identity.privateKey);
    return identity;
  }
  async save(identity) {
    await mkdir(this.directory, { recursive: true, mode: 0o700 });
    await chmod(this.directory, 0o700);
    let content = JSON.stringify(identity);
    if (process.platform === "win32") content = await dpapi(content, false);
    await writeFile(this.file, content, { encoding: "utf8", flag: "wx", mode: 0o600 });
  }
}

async function readResponse(response) {
  const text = await response.text();
  let result;
  try { result = JSON.parse(text); }
  catch { throw new Error(`ARTEX 返回非 JSON 响应（HTTP ${response.status}）`); }
  if (!response.ok) throw new Error(`ARTEX 请求失败（HTTP ${response.status}）：${result.error ?? "未知错误"}`);
  return result;
}

export class ArtexAgentClient {
  constructor(identity, fetcher = fetch, versionProvider = getSkillVersion) { this.identity = identity; this.fetcher = fetcher; this.versionProvider = versionProvider; }
  async request(resource, { method = "GET", body, contentType = "application/json" } = {}) {
    const response = await this.fetchRequest(resource, { method, body, contentType });
    return readResponse(response);
  }
  async fetchRequest(resource, { method = "GET", body, contentType = "application/json" } = {}) {
    const url = new URL(normalizeBaseUrl(this.identity.baseUrl) + resource);
    const version = await this.versionProvider();
    if (/^[a-f0-9]{64}$/.test(version ?? "")) url.searchParams.set("_skill_version", version);
    const bytes = body === undefined ? undefined : Buffer.isBuffer(body) ? body : Buffer.from(JSON.stringify(body));
    const headers = { accept: "application/json", ...signedHeaders({ method, url, body: bytes, identity: this.identity }) };
    if (bytes) headers["content-type"] = contentType;
    let response;
    try {
      response = await this.fetcher(url, { method, headers, body: bytes, redirect: "error", signal: AbortSignal.timeout(60000) });
    } catch {
      throw new Error(method === "GET" ? "无法连接 ARTEX，请检查地址与网络" : "ARTEX 请求结果不确定；请先核对网页任务或 Skill 列表，避免重复操作");
    }
    return response;
  }
  async requestBytes(resource) {
    const response = await this.fetchRequest(resource);
    if (!response.ok) return readResponse(response);
    return Buffer.from(await response.arrayBuffer());
  }
  async uploadSkill(file, overwrite = false) {
    const info = await stat(file);
    if (!info.isFile() || info.size > MAX_UPLOAD_BYTES) throw new Error("Skill ZIP 必须为小于 20 MiB 的普通文件");
    const zip = await readFile(file);
    if (zip.length < 4 || zip.length > MAX_UPLOAD_BYTES || zip.readUInt32LE(0) !== 0x04034b50) throw new Error("请选择有效的 Skill ZIP 压缩包");
    const boundary = "artex-" + randomBytes(18).toString("hex");
    const body = Buffer.concat([
      Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="skill.zip"\r\nContent-Type: application/zip\r\n\r\n`),
      zip, Buffer.from(`\r\n--${boundary}--\r\n`),
    ]);
    return this.request("/skills/upload" + (overwrite ? "?overwrite=true" : ""), { method: "POST", body, contentType: `multipart/form-data; boundary=${boundary}` });
  }
  async uploadWorkspace(file, directory = "") {
    const info = await stat(file);
    if (!info.isFile() || info.size > MAX_WORKSPACE_UPLOAD_BYTES) throw new Error("工作区文件必须为不超过 512 MiB 的普通文件");
    const data = await readFile(file);
    const boundary = "artex-" + randomBytes(18).toString("hex");
    const filename = path.basename(file).replace(/[\\"\r\n]/g, "_");
    const body = Buffer.concat([
      Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${filename}"\r\nContent-Type: application/octet-stream\r\n\r\n`),
      data, Buffer.from(`\r\n--${boundary}--\r\n`),
    ]);
    const query = directory ? `?path=${encodeURIComponent(directory)}` : "";
    return this.request(`/workspace/upload${query}`, { method: "POST", body, contentType: `multipart/form-data; boundary=${boundary}` });
  }
  async downloadWorkspace(workspacePath, outputFile) {
    const data = await this.requestBytes(`/workspace/download?path=${encodeURIComponent(workspacePath)}`);
    await writeFile(outputFile, data, { flag: "wx" });
    return { path: outputFile, bytes: data.length };
  }
}

export async function pairDevice(baseUrl, name, fetcher = fetch) {
  const identity = { ...generateIdentity(name), baseUrl: normalizeBaseUrl(baseUrl) };
  const proof = sign(null, Buffer.from(`ARTEX local-agent pair v1\n${name}\n${identity.publicKey}`), identity.privateKey).toString("base64");
  const response = await fetcher(identity.baseUrl + "/pair", {
    method: "POST", redirect: "error", signal: AbortSignal.timeout(30000),
    headers: { "content-type": "application/json", "X-ARTEX-Pair-Proof": proof },
    body: JSON.stringify({ name, public_key: identity.publicKey }),
  });
  const result = await readResponse(response);
  if (!result.id) throw new Error("ARTEX 未返回设备 ID");
  return { ...identity, deviceId: result.id, status: result.status };
}

function parseArguments(argv) {
  const args = [];
  const flags = {};
  for (let index = 0; index < argv.length; index++) {
    const value = argv[index];
    if (!value.startsWith("--")) { args.push(value); continue; }
    const name = value.slice(2);
    if (["overwrite", "help"].includes(name)) { flags[name] = true; continue; }
    const next = argv[++index];
    if (!next || next.startsWith("--")) throw new Error(`缺少 --${name} 的值`);
    if (Object.hasOwn(flags, name)) throw new Error(`参数 --${name} 重复`);
    flags[name] = next;
  }
  return { args, flags };
}

function required(flags, name) {
  if (typeof flags[name] !== "string" || !flags[name].trim()) throw new Error(`缺少 --${name}`);
  return flags[name];
}

function optionalQuery(flags, names, aliases = {}) {
  const params = new URLSearchParams();
  for (const name of names) {
    if (flags[name] !== undefined) params.set(aliases[name] ?? name.replaceAll("-", "_"), String(flags[name]));
  }
  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}

async function jsonFile(file) {
  const body = JSON.parse(await readFile(file, "utf8"));
  if (!body || Array.isArray(body) || typeof body !== "object") throw new Error("JSON 必须为对象");
  return body;
}

const HELP = `ARTEX 本地 Agent（Node.js 20+，无需 npm 依赖）
  auth login --url <ARTEX地址> [--name <设备名称>]
  auth status
  tasks list
  tasks get --id <任务ID>
  tasks create --description <描述> [--goal <目标>]
  tasks create --file <任务JSON路径>
  tasks control --id <任务ID> --action pause|resume
  tasks chat --id <任务ID> --message <消息>
  tasks chat-session --id <任务ID>
  tasks chat-status --id <任务ID>
  tasks chat-stop --id <任务ID>
  tasks worker-message --id <任务ID> --intent-id <意图ID> --message <消息> [--request-id <请求ID>]
  tasks llm-status --id <任务ID>
  tasks llm-set --id <任务ID> --file <LLM配置JSON>
  skills list
  skills upload --file <Skill ZIP路径> [--overwrite]
  workspace list [--path <目录>]
  workspace read --path <文件>
  workspace write --path <文件> (--content <内容> | --file <文本文件>)
  workspace mkdir --path <目录>
  workspace delete --path <文件或目录>
  workspace upload --file <本地文件> [--path <目标目录>]
  workspace download --path <文件> --output <本地文件>
  assets list [--type <类型>] [--company-id <ID>] [--task-id <ID>]
  assets counts
  assets upload --file <资产JSON文件>
  assets download --output <本地JSON文件> [--type <类型>] [--company-id <ID>] [--task-id <ID>]
  assets delete --ids <ID列表，例如 1,2,3>
  conversations list
  conversations create --agent-key <Agent键> [--title <标题>]
  conversations messages --id <会话ID> [--since <序号>] [--before <序号>] [--limit <数量>]
  conversations send --id <会话ID> --message <消息>
  conversations stop --id <会话ID>
  discover findings|stats|graph|nodes|activity [--task-id <ID>]
  logs list [--since <序号>] [--limit <数量>]
  logs history [--before <ID>] [--limit <数量>]
  llm status
  llm profiles
  llm set --file <全局LLM配置JSON>
  llm profile-save --file <LLM档案JSON>
  llm activate --id <档案ID>
  tools list
  tools set --key <工具键> --file <工具JSON>
  tools custom-save --file <自定义工具JSON>
  tools custom-delete --key <工具键>
首次登录后，在 ARTEX 网页“系统 / Agent 接入”核对设备指纹并批准权限。
身份保存在当前系统用户的私有目录；可通过 ARTEX_AGENT_CONFIG_DIR 选择独立身份目录。
撤销设备请在 ARTEX 网页操作。创建任务会按 ARTEX 的现有流程启动任务。`;

export async function main(argv = process.argv.slice(2), store = new IdentityStore(), output = value => process.stdout.write(JSON.stringify(value, null, 2) + "\n"), fetcher = fetch) {
  const { args, flags } = parseArguments(argv);
  if (!args.length || flags.help || args[0] === "help") { process.stdout.write(HELP + "\n"); return; }
  const [command, action] = args;
  if (args.length !== 2) throw new Error("请使用 help 查看命令");
  const known = command === "auth" && ["login", "status"].includes(action)
    || command === "tasks" && ["list", "get", "create", "control", "chat", "chat-session", "chat-status", "chat-stop", "worker-message", "llm-status", "llm-set"].includes(action)
    || command === "skills" && ["list", "upload"].includes(action)
    || command === "workspace" && ["list", "read", "write", "mkdir", "delete", "upload", "download"].includes(action)
    || command === "assets" && ["list", "counts", "upload", "download", "delete"].includes(action)
    || command === "conversations" && ["list", "create", "messages", "send", "stop"].includes(action)
    || command === "discover" && ["findings", "stats", "graph", "nodes", "activity"].includes(action)
    || command === "logs" && ["list", "history"].includes(action)
    || command === "llm" && ["status", "profiles", "set", "profile-save", "activate"].includes(action)
    || command === "tools" && ["list", "set", "custom-save", "custom-delete"].includes(action);
  if (!known) throw new Error("命令无效，请使用 help 查看命令");
  const allowedFlags = command === "auth" && action === "login" ? ["url", "name"]
    : command === "tasks" && action === "create" ? ["file", "description", "goal"]
    : command === "tasks" && action === "get" ? ["id"]
    : command === "tasks" && action === "control" ? ["id", "action"]
    : command === "tasks" && ["chat", "chat-status", "chat-stop"].includes(action) ? ["id", ...(action === "chat" ? ["message"] : [])]
    : command === "tasks" && action === "chat-session" ? ["id"]
    : command === "tasks" && action === "worker-message" ? ["id", "intent-id", "message", "request-id"]
    : command === "tasks" && action === "llm-status" ? ["id"]
    : command === "tasks" && action === "llm-set" ? ["id", "file"]
    : command === "skills" && action === "upload" ? ["file", "overwrite"]
    : command === "workspace" && action === "list" ? ["path"]
    : command === "workspace" && action === "read" ? ["path"]
    : command === "workspace" && action === "write" ? ["path", "content", "file"]
    : command === "workspace" && ["mkdir", "delete"].includes(action) ? ["path"]
    : command === "workspace" && action === "upload" ? ["path", "file"]
    : command === "workspace" && action === "download" ? ["path", "output"]
    : command === "assets" && action === "list" ? ["type", "company-id", "task-id", "limit", "offset", "dsl"]
    : command === "assets" && action === "upload" ? ["file"]
    : command === "assets" && action === "download" ? ["output", "type", "company-id", "task-id", "limit", "offset", "dsl"]
    : command === "assets" && action === "delete" ? ["ids"]
    : command === "conversations" && action === "create" ? ["agent-key", "title", "file"]
    : command === "conversations" && action === "messages" ? ["id", "since", "before", "limit"]
    : command === "conversations" && ["send", "stop"].includes(action) ? ["id", "message"]
    : command === "discover" && action === "findings" ? ["task-id", "page", "limit", "filter"]
    : command === "discover" && action === "nodes" ? ["task-id", "page", "size", "kind", "state", "q", "order"]
    : command === "discover" ? ["task-id", "limit", "offset", "since", "before"]
    : command === "logs" && action === "list" ? ["since", "limit"]
    : command === "logs" && action === "history" ? ["before", "limit"]
    : command === "llm" && ["set", "profile-save"].includes(action) ? ["file"]
    : command === "llm" && action === "activate" ? ["id"]
    : command === "tools" && action === "set" ? ["key", "file"]
    : command === "tools" && action === "custom-save" ? ["file"]
    : command === "tools" && action === "custom-delete" ? ["key"] : [];
  for (const flag of Object.keys(flags)) if (!allowedFlags.includes(flag)) throw new Error(`不支持参数 --${flag}`);
  const existing = await store.load();
  if (command === "auth" && action === "login") {
    if (existing) throw new Error("当前目录已有设备身份，请运行 auth status；连接其他 ARTEX 请使用独立 ARTEX_AGENT_CONFIG_DIR");
    const identity = await pairDevice(required(flags, "url"), flags.name ?? `${os.hostname()} (${process.platform})`, fetcher);
    await store.save(identity);
    output({ configured: true, deviceId: String(identity.deviceId), name: identity.name, status: identity.status, fingerprint: identity.fingerprint, message: "请在 ARTEX 网页的 Agent 接入页面核对指纹并批准权限，然后运行 auth status" });
    return;
  }
  if (!existing) {
    if (command === "auth" && action === "status") { output({ configured: false }); return; }
    throw new Error("尚未配置设备，请先运行 auth login --url <ARTEX地址>");
  }
  const client = new ArtexAgentClient(existing, fetcher);
  if (command === "auth") { output({ configured: true, baseUrl: existing.baseUrl, fingerprint: existing.fingerprint, ...await client.request("/status") }); return; }
  if (action === "list" && !["workspace", "assets", "logs"].includes(command)) { output(await client.request(`/${command}`)); return; }
  if (command === "tasks" && action === "get") {
    const id = required(flags, "id");
    if (!/^[1-9][0-9]*$/.test(id)) throw new Error("任务 ID 必须为正整数");
    output(await client.request(`/tasks/${id}`)); return;
  }
  if (command === "tasks" && action === "control") {
    const id = required(flags, "id");
    if (!/^[1-9][0-9]*$/.test(id)) throw new Error("任务 ID 必须为正整数");
    const controlAction = required(flags, "action");
    if (!["pause", "resume"].includes(controlAction)) throw new Error("--action 只能是 pause 或 resume");
    output(await client.request(`/tasks/${id}/control`, { method: "POST", body: { action: controlAction } })); return;
  }
  if (command === "tasks" && ["chat", "chat-session", "chat-status", "chat-stop", "worker-message", "llm-status", "llm-set"].includes(action)) {
    const id = required(flags, "id");
    if (!/^[1-9][0-9]*$/.test(id)) throw new Error("任务 ID 必须为正整数");
    if (action === "chat") { output(await client.request(`/tasks/${id}/chat`, { method: "POST", body: { message: required(flags, "message") } })); return; }
    if (action === "chat-session") { output(await client.request(`/tasks/${id}/chat/session`, { method: "POST", body: {} })); return; }
    if (action === "chat-status") { output(await client.request(`/tasks/${id}/chat/status`)); return; }
    if (action === "chat-stop") { output(await client.request(`/tasks/${id}/chat/stop`, { method: "POST", body: {} })); return; }
    if (action === "worker-message") {
      const iid = required(flags, "intent-id");
      if (!/^[1-9][0-9]*$/.test(iid)) throw new Error("意图 ID 必须为正整数");
      const body = { message: required(flags, "message") };
      if (flags["request-id"] !== undefined) body.request_id = flags["request-id"];
      output(await client.request(`/tasks/${id}/intents/${iid}/messages`, { method: "POST", body })); return;
    }
    if (action === "llm-status") { output(await client.request(`/tasks/${id}/llm/resolution`)); return; }
    output(await client.request(`/tasks/${id}/llm`, { method: "PUT", body: await jsonFile(required(flags, "file")) })); return;
  }
  if (command === "tasks") {
    if (flags.file && (flags.description || flags.goal)) throw new Error("--file 不能与 --description 或 --goal 同时使用");
    const body = flags.file ? JSON.parse(await readFile(flags.file, "utf8")) : { description: required(flags, "description"), goal: flags.goal ?? "" };
    if (!body || Array.isArray(body) || typeof body !== "object") throw new Error("任务 JSON 必须为对象");
    output(await client.request("/tasks", { method: "POST", body }));
    return;
  }
  if (command === "skills") { output(await client.uploadSkill(required(flags, "file"), flags.overwrite === true)); return; }
  if (command === "workspace") {
    if (action === "list" || action === "read") {
      output(await client.request(`/workspace/${action}${optionalQuery(flags, ["path"])}`)); return;
    }
    if (action === "write") {
      if (flags.content !== undefined && flags.file !== undefined) throw new Error("--content 不能与 --file 同时使用");
      const content = flags.file ? await readFile(required(flags, "file"), "utf8") : required(flags, "content");
      output(await client.request("/workspace/write", { method: "POST", body: { path: required(flags, "path"), content } })); return;
    }
    if (action === "mkdir") { output(await client.request("/workspace/mkdir", { method: "POST", body: { path: required(flags, "path") } })); return; }
    if (action === "delete") { output(await client.request(`/workspace/delete?path=${encodeURIComponent(required(flags, "path"))}`, { method: "DELETE" })); return; }
    if (action === "upload") { output(await client.uploadWorkspace(required(flags, "file"), flags.path ?? "")); return; }
    output(await client.downloadWorkspace(required(flags, "path"), required(flags, "output"))); return;
  }
  if (command === "assets") {
    if (action === "counts") { output(await client.request("/assets/counts")); return; }
    const queryFlags = ["type", "company-id", "task-id", "limit", "offset", "dsl"];
    if (action === "list") { output(await client.request(`/assets${optionalQuery(flags, queryFlags)}`)); return; }
    if (action === "upload") {
      const body = await jsonFile(required(flags, "file"));
      if (!Array.isArray(body.assets)) throw new Error("资产 JSON 必须包含 assets 数组");
      output(await client.request("/assets", { method: "POST", body })); return;
    }
    if (action === "delete") {
      const rawIDs = required(flags, "ids").split(",").map(value => value.trim());
      if (rawIDs.some(value => !/^[1-9][0-9]*$/.test(value))) throw new Error("--ids 必须是逗号分隔的正整数列表");
      output(await client.request("/assets", { method: "DELETE", body: { ids: [...new Set(rawIDs.map(Number))] } })); return;
    }
    const result = await client.request(`/assets${optionalQuery(flags, queryFlags)}`);
    const destination = required(flags, "output");
    const content = Buffer.from(JSON.stringify(result, null, 2) + "\n", "utf8");
    await writeFile(destination, content, { flag: "wx" });
    output({ path: destination, bytes: content.length }); return;
  }
  if (command === "conversations") {
    if (action === "list") { output(await client.request("/conversations")); return; }
    if (action === "create") {
      const body = flags.file ? await jsonFile(required(flags, "file")) : { agent_key: required(flags, "agent-key"), title: flags.title ?? "" };
      output(await client.request("/conversations", { method: "POST", body })); return;
    }
    const id = required(flags, "id");
    if (!/^[1-9][0-9]*$/.test(id)) throw new Error("会话 ID 必须为正整数");
    if (action === "messages") { output(await client.request(`/conversations/${id}/messages${optionalQuery(flags, ["since", "before", "limit"])}`)); return; }
    if (action === "send") { output(await client.request(`/conversations/${id}/messages`, { method: "POST", body: { message: required(flags, "message") } })); return; }
    output(await client.request(`/conversations/${id}/stop`, { method: "POST", body: {} })); return;
  }
  if (command === "discover") {
    const names = action === "findings" ? ["task-id", "page", "limit", "filter"]
      : action === "nodes" ? ["task-id", "page", "size", "kind", "state", "q", "order"]
      : ["task-id", "limit", "offset", "since", "before"];
    const suffix = optionalQuery(flags, names, { "task-id": "task" });
    output(await client.request(`/discovery/${action}${suffix}`)); return;
  }
  if (command === "logs") {
    output(await client.request(`/logs${action === "history" ? "/history" : ""}${optionalQuery(flags, ["since", "before", "limit"])}`)); return;
  }
  if (command === "llm") {
    if (action === "status") { output(await client.request("/llm")); return; }
    if (action === "profiles") { output(await client.request("/llm/profiles")); return; }
    if (action === "set") { output(await client.request("/llm", { method: "POST", body: await jsonFile(required(flags, "file")) })); return; }
    if (action === "profile-save") { output(await client.request("/llm/profiles", { method: "POST", body: await jsonFile(required(flags, "file")) })); return; }
    const id = required(flags, "id");
    if (!/^[1-9][0-9]*$/.test(id)) throw new Error("档案 ID 必须为正整数");
    output(await client.request("/llm/profiles/active", { method: "POST", body: { id: Number(id) } })); return;
  }
  if (command === "tools") {
    if (action === "list") { output(await client.request("/tools")); return; }
    if (action === "set") {
      const key = required(flags, "key");
      output(await client.request(`/tools/${encodeURIComponent(key)}`, { method: "PUT", body: await jsonFile(required(flags, "file")) })); return;
    }
    if (action === "custom-save") { output(await client.request("/tools/custom", { method: "POST", body: await jsonFile(required(flags, "file")) })); return; }
    const key = required(flags, "key");
    output(await client.request(`/tools/custom/${encodeURIComponent(key)}`, { method: "DELETE" })); return;
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(error => { process.stderr.write(`错误：${error.message}\n`); process.exitCode = 1; });
}
