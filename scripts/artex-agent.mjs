#!/usr/bin/env node
import { createHash, createPrivateKey, generateKeyPairSync, randomBytes, sign } from "node:crypto";
import { spawn } from "node:child_process";
import { chmod, mkdir, readFile, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const MAX_UPLOAD_BYTES = 20 * 1024 * 1024 - 4096;
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
  constructor(identity, fetcher = fetch) { this.identity = identity; this.fetcher = fetcher; }
  async request(resource, { method = "GET", body, contentType = "application/json" } = {}) {
    const url = new URL(normalizeBaseUrl(this.identity.baseUrl) + resource);
    const bytes = body === undefined ? undefined : Buffer.isBuffer(body) ? body : Buffer.from(JSON.stringify(body));
    const headers = { accept: "application/json", ...signedHeaders({ method, url, body: bytes, identity: this.identity }) };
    if (bytes) headers["content-type"] = contentType;
    let response;
    try {
      response = await this.fetcher(url, { method, headers, body: bytes, redirect: "error", signal: AbortSignal.timeout(60000) });
    } catch {
      throw new Error(method === "GET" ? "无法连接 ARTEX，请检查地址与网络" : "ARTEX 请求结果不确定；请先核对网页任务或 Skill 列表，避免重复操作");
    }
    return readResponse(response);
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

const HELP = `ARTEX 本地 Agent（Node.js 20+，无需 npm 依赖）
  auth login --url <ARTEX地址> [--name <设备名称>]
  auth status
  tasks list
  tasks get --id <任务ID>
  tasks create --description <描述> [--goal <目标>]
  tasks create --file <任务JSON路径>
  skills list
  skills upload --file <Skill ZIP路径> [--overwrite]
首次登录后，在 ARTEX 网页“系统 / Agent 接入”核对设备指纹并批准权限。
身份保存在当前系统用户的私有目录；可通过 ARTEX_AGENT_CONFIG_DIR 选择独立身份目录。
撤销设备请在 ARTEX 网页操作。创建任务会按 ARTEX 的现有流程启动任务。`;

export async function main(argv = process.argv.slice(2), store = new IdentityStore(), output = value => process.stdout.write(JSON.stringify(value, null, 2) + "\n")) {
  const { args, flags } = parseArguments(argv);
  if (!args.length || flags.help || args[0] === "help") { process.stdout.write(HELP + "\n"); return; }
  const [command, action] = args;
  if (args.length !== 2) throw new Error("请使用 help 查看命令");
  const known = command === "auth" && ["login", "status"].includes(action)
    || command === "tasks" && ["list", "get", "create"].includes(action)
    || command === "skills" && ["list", "upload"].includes(action);
  if (!known) throw new Error("命令无效，请使用 help 查看命令");
  const allowedFlags = command === "auth" && action === "login" ? ["url", "name"]
    : command === "tasks" && action === "create" ? ["file", "description", "goal"]
    : command === "tasks" && action === "get" ? ["id"]
    : command === "skills" && action === "upload" ? ["file", "overwrite"] : [];
  for (const flag of Object.keys(flags)) if (!allowedFlags.includes(flag)) throw new Error(`不支持参数 --${flag}`);
  const existing = await store.load();
  if (command === "auth" && action === "login") {
    if (existing) throw new Error("当前目录已有设备身份，请运行 auth status；连接其他 ARTEX 请使用独立 ARTEX_AGENT_CONFIG_DIR");
    const identity = await pairDevice(required(flags, "url"), flags.name ?? `${os.hostname()} (${process.platform})`);
    await store.save(identity);
    output({ configured: true, deviceId: String(identity.deviceId), name: identity.name, status: identity.status, fingerprint: identity.fingerprint, message: "请在 ARTEX 网页的 Agent 接入页面核对指纹并批准权限，然后运行 auth status" });
    return;
  }
  if (!existing) {
    if (command === "auth" && action === "status") { output({ configured: false }); return; }
    throw new Error("尚未配置设备，请先运行 auth login --url <ARTEX地址>");
  }
  const client = new ArtexAgentClient(existing);
  if (command === "auth") { output({ configured: true, baseUrl: existing.baseUrl, fingerprint: existing.fingerprint, ...await client.request("/status") }); return; }
  if (action === "list") { output(await client.request(`/${command}`)); return; }
  if (command === "tasks" && action === "get") {
    const id = required(flags, "id");
    if (!/^[1-9][0-9]*$/.test(id)) throw new Error("任务 ID 必须为正整数");
    output(await client.request(`/tasks/${id}`)); return;
  }
  if (command === "tasks") {
    if (flags.file && (flags.description || flags.goal)) throw new Error("--file 不能与 --description 或 --goal 同时使用");
    const body = flags.file ? JSON.parse(await readFile(flags.file, "utf8")) : { description: required(flags, "description"), goal: flags.goal ?? "" };
    if (!body || Array.isArray(body) || typeof body !== "object") throw new Error("任务 JSON 必须为对象");
    output(await client.request("/tasks", { method: "POST", body }));
    return;
  }
  output(await client.uploadSkill(required(flags, "file"), flags.overwrite === true));
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(error => { process.stderr.write(`错误：${error.message}\n`); process.exitCode = 1; });
}
