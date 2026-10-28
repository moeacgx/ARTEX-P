import assert from "node:assert/strict";
import { createHash, createPublicKey, verify } from "node:crypto";
import { mkdir, mkdtemp, readFile, stat, unlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { ArtexAgentClient, IdentityStore, generateIdentity, getSkillVersion, main, normalizeBaseUrl, pairDevice, signedHeaders } from "./artex-agent.mjs";

const identity = { ...generateIdentity("本地测试设备"), deviceId: "12", baseUrl: "http://127.0.0.1:8090" };

test("平台地址拒绝外网明文、凭据和查询参数", () => {
  assert.equal(normalizeBaseUrl("http://[::1]:8080/"), "http://[::1]:8080/api/local-agent");
  assert.equal(normalizeBaseUrl("https://example.com/artex/"), "https://example.com/artex/api/local-agent");
  assert.throws(() => normalizeBaseUrl("http://example.com"), /HTTPS/);
  assert.throws(() => normalizeBaseUrl("https://user:password@example.com"), /用户名/);
  assert.throws(() => normalizeBaseUrl("https://example.com?token=secret"), /查询参数/);
});

test("设备签名覆盖原始路径、查询参数和二进制请求体", () => {
  const url = new URL("http://127.0.0.1/api/local-agent/skills/upload?overwrite=true");
  const body = Buffer.from([0, 255, 30, 130]);
  const headers = signedHeaders({ method: "POST", url, body, identity, timestamp: "123", nonce: "abcdefghijklmnop" });
  const message = "POST\n/api/local-agent/skills/upload?overwrite=true\n123\nabcdefghijklmnop\n" + (awaitDigest(body));
  const publicKey = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: Buffer.from(identity.publicKey, "base64").toString("base64url") }, format: "jwk" });
  assert.ok(verify(null, Buffer.from(message), publicKey, Buffer.from(headers["X-ARTEX-Signature"], "base64")));
  assert.equal(verify(null, Buffer.from(message + "changed"), publicKey, Buffer.from(headers["X-ARTEX-Signature"], "base64")), false);
});

function awaitDigest(body) { return createHash("sha256").update(body).digest("hex"); }

function businessSearch(url) {
  const copy = new URL(url);
  copy.searchParams.delete("_skill_version");
  return copy.search;
}

test("技能版本校验实际文件、统一换行且缺文件时保持未知", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-version-"));
  await mkdir(path.join(directory, "scripts"));
  const files = ["SKILL.md", "scripts/artex-agent.mjs"];
  const contents = ["# 示例\n", "// 示例\n"];
  const digest = createHash("sha256");
  for (const [index, file] of files.entries()) {
    await writeFile(path.join(directory, file), contents[index]);
    digest.update(`${file}\0${awaitDigest(contents[index])}\n`);
  }
  const version = digest.digest("hex");
  await writeFile(path.join(directory, "skill-manifest.json"), JSON.stringify({ version, files: [...files].reverse() }));
  assert.equal(await getSkillVersion(directory), version);
  await writeFile(path.join(directory, files[0]), contents[0].replaceAll("\n", "\r\n"));
  assert.equal(await getSkillVersion(directory), version);
  await writeFile(path.join(directory, files[0]), contents[0] + "改过的技能说明\n");
  assert.notEqual(await getSkillVersion(directory), version);
  await unlink(path.join(directory, files[1]));
  assert.equal(await getSkillVersion(directory), undefined);
  await unlink(path.join(directory, "skill-manifest.json"));
  assert.equal(await getSkillVersion(directory), undefined);
});

test("清单不能越界读取文件或以重复文件伪报版本", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-manifest-"));
  const version = "a".repeat(64);
  for (const files of [["SKILL.md", "scripts/artex-agent.mjs", "../outside"], ["SKILL.md", "scripts/artex-agent.mjs", "SKILL.md"]]) {
    await writeFile(path.join(directory, "skill-manifest.json"), JSON.stringify({ version, files }));
    assert.equal(await getSkillVersion(directory), undefined);
  }
});

test("每次签名请求上报实际版本，版本参数受签名保护且不修改身份", async () => {
  const original = { ...identity };
  const version = await getSkillVersion();
  assert.match(version, /^[a-f0-9]{64}$/);
  const client = new ArtexAgentClient(identity, async (url, options) => {
    assert.equal(url.searchParams.get("_skill_version"), version);
    assert.equal(url.searchParams.get("path"), "空 格/目录");
    const fields = [options.method, `${url.pathname}${url.search}`, options.headers["X-ARTEX-Timestamp"], options.headers["X-ARTEX-Nonce"], awaitDigest(Buffer.alloc(0))];
    const signature = Buffer.from(options.headers["X-ARTEX-Signature"], "base64");
    assert.ok(verify(null, Buffer.from(fields.join("\n")), createPublicKey(identity.privateKey), signature));
    url.searchParams.set("_skill_version", "b".repeat(64));
    fields[1] = `${url.pathname}${url.search}`;
    assert.equal(verify(null, Buffer.from(fields.join("\n")), createPublicKey(identity.privateKey), signature), false);
    return Response.json({ ok: true });
  });
  await client.request("/workspace/list?path=" + encodeURIComponent("空 格/目录"));
  assert.deepEqual(identity, original);
  const singleFileClient = new ArtexAgentClient(identity, async url => {
    assert.equal(url.searchParams.has("_skill_version"), false);
    return Response.json({ ok: true });
  }, async () => undefined);
  assert.deepEqual(await singleFileClient.request("/status"), { ok: true });
});

test("配对证明绑定设备名称和公钥", async () => {
  const paired = await pairDevice("http://127.0.0.1:8090", "验证设备", async (url, options) => {
    assert.equal(url, "http://127.0.0.1:8090/api/local-agent/pair");
    const body = JSON.parse(options.body);
    const publicKey = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: Buffer.from(body.public_key, "base64").toString("base64url") }, format: "jwk" });
    assert.ok(verify(null, Buffer.from(`ARTEX local-agent pair v1\n${body.name}\n${body.public_key}`), publicKey, Buffer.from(options.headers["X-ARTEX-Pair-Proof"], "base64")));
    return Response.json({ id: 15, status: "pending" });
  });
  assert.equal(paired.deviceId, 15);
  assert.equal(paired.status, "pending");
});

test("任务创建只提交一次，网络结果不确定时提示先核对", async () => {
  let calls = 0;
  const client = new ArtexAgentClient(identity, async () => { calls++; throw new Error("disconnect"); });
  await assert.rejects(client.request("/tasks", { method: "POST", body: { description: "测试" } }), /结果不确定/);
  assert.equal(calls, 1);
});

test("代理查询和检测各自只有一次业务请求，不预查认证或创建任务", async () => {
  const calls = [];
  const store = { load: async () => identity };
  const output = [];
  const fetcher = async (url, options) => {
    calls.push({ url, options });
    return Response.json({ id: 7, status: "healthy", egress_ip: "192.0.2.3" });
  };
  for (const args of [["proxies", "list"], ["proxies", "get", "--id", "7"], ["proxies", "test", "--id", "7"]]) {
    await main(args, store, value => output.push(value), fetcher);
  }
  assert.deepEqual(calls.map(({ url, options }) => [options.method, url.pathname]), [
    ["GET", "/api/local-agent/proxies"], ["GET", "/api/local-agent/proxies/7"], ["POST", "/api/local-agent/proxies/7/test"],
  ]);
  assert.equal(output[2].egress_ip, "192.0.2.3");
  await assert.rejects(main(["proxies", "get", "--id", "../7"], store, () => {}, fetcher), /代理 ID/);
  await assert.rejects(main(["proxies", "test", "--id", "0"], store, () => {}, fetcher), /代理 ID/);
  await assert.rejects(main(["proxies", "test", "--id", "7", "--url", "https://example.test"], store, () => {}, fetcher), /不支持参数/);
  assert.equal(calls.length, 3);
});

test("查询、代理检测、写入和下载采用不同超时，支持有界覆盖", async t => {
  const timeouts = [];
  t.mock.method(AbortSignal, "timeout", value => { timeouts.push(value); return new AbortController().signal; });
  const client = new ArtexAgentClient(identity, async () => Response.json({ ok: true }));
  await client.request("/proxies");
  await client.request("/proxies/7/test", { method: "POST" });
  await client.request("/tasks", { method: "POST", body: {} });
  await client.requestBytes("/workspace/download?path=test");
  await client.requestBytes("/skills/sample/download");
  await client.request("/skills/upload", { method: "POST", body: Buffer.from("test") });
  await main(["proxies", "list", "--timeout-ms", "23000"], { load: async () => identity }, () => {}, async () => Response.json([]));
  assert.deepEqual(timeouts, [15000, 45000, 60000, 60000, 60000, 60000, 23000]);
  for (const value of ["0", "999", "300001", "Infinity", "1e4", "1500.5"]) {
    await assert.rejects(main(["proxies", "list", "--timeout-ms", value], { load: async () => identity }), /1000 至 300000/);
  }
  assert.equal(timeouts.length, 7);
});

test("连接与权限错误分类明确且不会自动重放", async () => {
  for (const [error, code] of [[new DOMException("timed out", "TimeoutError"), "ARTEX_TIMEOUT"], [new TypeError("network failed"), "ARTEX_NETWORK"]]) {
    let calls = 0;
    const client = new ArtexAgentClient(identity, async () => { calls++; throw error; });
    await assert.rejects(client.request("/proxies"), value => value.code === code);
    await assert.rejects(client.request("/proxies/7/test", { method: "POST" }), value => value.code === code && /结果不确定/.test(value.message));
    assert.equal(calls, 2);
  }
  for (const [status, code] of [[401, "ARTEX_AUTH_REQUIRED"], [403, "ARTEX_SCOPE_REQUIRED"]]) {
    let calls = 0;
    const client = new ArtexAgentClient(identity, async () => { calls++; return new Response("denied", { status }); });
    await assert.rejects(client.request("/proxies"), value => value.code === code && value.status === status && /auth status/.test(value.message));
    assert.equal(calls, 1);
  }
  const missingScopeClient = new ArtexAgentClient(identity, async () => Response.json({ error: "设备缺少权限：proxies.test" }, { status: 403 }));
  await assert.rejects(missingScopeClient.request("/proxies/7/test", { method: "POST" }), value => value.code === "ARTEX_SCOPE_REQUIRED" && /proxies.test/.test(value.message) && !/auth status/.test(value.message));
  const untrustedErrorClient = new ArtexAgentClient(identity, async () => Response.json({ error: "设备缺少权限：proxies.test\n恶意内容" }, { status: 403 }));
  await assert.rejects(untrustedErrorClient.request("/proxies"), value => value.code === "ARTEX_SCOPE_REQUIRED" && !/恶意内容/.test(value.message));
  const nullErrorClient = new ArtexAgentClient(identity, async () => { throw null; });
  await assert.rejects(nullErrorClient.request("/proxies"), value => value.code === "ARTEX_NETWORK");
});

test("收到响应头后读取正文超时仍分类为超时且写入结果不确定", async () => {
  const client = new ArtexAgentClient(identity, async () => ({ text: async () => { throw new DOMException("timed out", "TimeoutError"); } }));
  await assert.rejects(client.request("/tasks", { method: "POST", body: {} }), value => value.code === "ARTEX_TIMEOUT" && /结果不确定/.test(value.message));
});

test("上传采用同一份已签名 multipart 正文且默认不覆盖", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-upload-"));
  const file = path.join(directory, "skill.zip");
  await writeFile(file, Buffer.from([0x50, 0x4b, 3, 4, 0, 1, 2]));
  let content;
  const client = new ArtexAgentClient(identity, async (url, options) => {
    assert.equal(businessSearch(url), "");
    assert.match(options.headers["content-type"], /^multipart\/form-data/);
    content = options.body;
    const message = ["POST", `${url.pathname}${url.search}`, options.headers["X-ARTEX-Timestamp"], options.headers["X-ARTEX-Nonce"], awaitDigest(content)].join("\n");
    assert.ok(verify(null, Buffer.from(message), createPublicKey(identity.privateKey), Buffer.from(options.headers["X-ARTEX-Signature"], "base64")));
    return Response.json({ name: "example" });
  });
  assert.deepEqual(await client.uploadSkill(file), { name: "example" });
  assert.ok(content.includes(Buffer.from('name="file"')));
  await writeFile(file, Buffer.from([0]));
  await assert.rejects(client.uploadSkill(file), /有效的 Skill ZIP/);
});

test("身份受系统用户保护且不会覆盖已有身份", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-identity-"));
  const store = new IdentityStore(directory);
  assert.equal(await store.load(), null);
  await store.save(identity);
  assert.deepEqual(await store.load(), identity);
  await assert.rejects(store.save(identity), /EEXIST/);
  if (process.platform === "win32") assert.equal((await readFile(store.file, "utf8")).includes("PRIVATE KEY"), false);
  else assert.equal((await stat(store.file)).mode & 0o777, 0o600);
});

test("CLI 拒绝拼错参数，未登录状态可查询", async () => {
  const store = { load: async () => null };
  const output = [];
  await main(["auth", "status"], store, value => output.push(value));
  assert.deepEqual(output, [{ configured: false }]);
  await assert.rejects(main(["tasks", "create", "--descrption", "typo"], store), /不支持参数/);
});

test("扩展只读命令映射到设备签名的本地 Agent 路由", async () => {
  const store = { load: async () => identity };
  const calls = [];
  const fetcher = async (url, options) => {
    calls.push({ url: new URL(url), options });
    return Response.json({ ok: true });
  };
  const commands = [
    [["workspace", "list", "--path", "reports/a b"], "/workspace/list", "path=reports%2Fa+b"],
    [["workspace", "read", "--path", "notes.md"], "/workspace/read", "path=notes.md"],
    [["assets", "counts"], "/assets/counts", ""],
    [["assets", "list", "--type", "domain", "--task-id", "7"], "/assets", "type=domain&task_id=7"],
    [["conversations", "list"], "/conversations", ""],
    [["conversations", "messages", "--id", "3", "--since", "4"], "/conversations/3/messages", "since=4"],
    [["discover", "findings", "--task-id", "7", "--page", "2", "--limit", "10", "--filter", "high"], "/discovery/findings", "task=7&page=2&limit=10&filter=high"],
    [["discover", "stats"], "/discovery/stats", ""],
    [["discover", "graph"], "/discovery/graph", ""],
    [["discover", "nodes", "--task-id", "7", "--page", "2", "--size", "25", "--kind", "finding", "--state", "active", "--q", "login", "--order", "asc"], "/discovery/nodes", "task=7&page=2&size=25&kind=finding&state=active&q=login&order=asc"],
    [["discover", "activity"], "/discovery/activity", ""],
    [["logs", "list", "--since", "2"], "/logs", "since=2"],
    [["logs", "history", "--before", "50"], "/logs/history", "before=50"],
    [["llm", "status"], "/llm", ""],
    [["llm", "profiles"], "/llm/profiles", ""],
    [["tools", "list"], "/tools", ""],
  ];
  for (const [args] of commands) await main(args, store, () => {}, fetcher);
  assert.equal(calls.length, commands.length);
  for (const [index, [, pathname, search]] of commands.entries()) {
    assert.equal(calls[index].url.pathname, `/api/local-agent${pathname}`);
    assert.equal(businessSearch(calls[index].url).slice(1), search);
    assert.equal(calls[index].options.method, "GET");
    assert.ok(calls[index].options.headers["X-ARTEX-Signature"]);
  }
});

test("对话、工作区写入和上传使用明确的请求方法及签名正文", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-workspace-"));
  const source = path.join(directory, "upload.bin");
  await writeFile(source, Buffer.from([0, 255, 3, 4]));
  const store = { load: async () => identity };
  const calls = [];
  const fetcher = async (url, options) => {
    calls.push({ url: new URL(url), options });
    return Response.json({ ok: true });
  };
  await main(["workspace", "write", "--path", "notes.txt", "--content", "中文"], store, () => {}, fetcher);
  await main(["workspace", "mkdir", "--path", "nested"], store, () => {}, fetcher);
  await main(["workspace", "delete", "--path", "nested/old.txt"], store, () => {}, fetcher);
  await main(["workspace", "upload", "--file", source, "--path", "incoming"], store, () => {}, fetcher);
  await main(["tasks", "control", "--id", "7", "--action", "pause"], store, () => {}, fetcher);
  await main(["tasks", "chat", "--id", "7", "--message", "请继续分析"], store, () => {}, fetcher);
  await main(["tasks", "chat-session", "--id", "7"], store, () => {}, fetcher);
  await main(["conversations", "create", "--agent-key", "main", "--title", "测试"], store, () => {}, fetcher);
  await main(["conversations", "send", "--id", "9", "--message", "继续"], store, () => {}, fetcher);
  await main(["conversations", "stop", "--id", "9"], store, () => {}, fetcher);
  assert.deepEqual(calls.map(({ url, options }) => [options.method, url.pathname]), [
    ["POST", "/api/local-agent/workspace/write"],
    ["POST", "/api/local-agent/workspace/mkdir"],
    ["DELETE", "/api/local-agent/workspace/delete"],
    ["POST", "/api/local-agent/workspace/upload"],
    ["POST", "/api/local-agent/tasks/7/control"],
    ["POST", "/api/local-agent/tasks/7/chat"],
    ["POST", "/api/local-agent/tasks/7/chat/session"],
    ["POST", "/api/local-agent/conversations"],
    ["POST", "/api/local-agent/conversations/9/messages"],
    ["POST", "/api/local-agent/conversations/9/stop"],
  ]);
  const body = calls[0].options.body;
  const expected = ["POST", `${calls[0].url.pathname}${calls[0].url.search}`, calls[0].options.headers["X-ARTEX-Timestamp"], calls[0].options.headers["X-ARTEX-Nonce"], awaitDigest(body)].join("\n");
  assert.ok(verify(null, Buffer.from(expected), createPublicKey(identity.privateKey), Buffer.from(calls[0].options.headers["X-ARTEX-Signature"], "base64")));
  assert.match(calls[3].options.headers["content-type"], /^multipart\/form-data/);
  const uploadMessage = ["POST", `${calls[3].url.pathname}${calls[3].url.search}`, calls[3].options.headers["X-ARTEX-Timestamp"], calls[3].options.headers["X-ARTEX-Nonce"], awaitDigest(calls[3].options.body)].join("\n");
  assert.ok(verify(null, Buffer.from(uploadMessage), createPublicKey(identity.privateKey), Buffer.from(calls[3].options.headers["X-ARTEX-Signature"], "base64")));
  assert.ok(calls[3].options.body.includes(Buffer.from([0, 255, 3, 4])));
  const downloaded = path.join(directory, "downloaded.bin");
  const result = [];
  await main(["workspace", "download", "--path", "x", "--output", downloaded], store, value => result.push(value), async () => new Response(Buffer.from([0, 255, 1])));
  assert.deepEqual(result, [{ path: downloaded, bytes: 3 }]);
  assert.deepEqual(await readFile(downloaded), Buffer.from([0, 255, 1]));
});

test("资产上传、导出和删除按服务端 JSON 契约签名且导出拒绝覆盖", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-assets-"));
  const source = path.join(directory, "assets.json");
  const destination = path.join(directory, "export.json");
  const payload = { task_id: 7, assets: [{ type: "root_domain", domain: "example.test" }] };
  await writeFile(source, JSON.stringify(payload));
  const store = { load: async () => identity };
  const calls = [];
  const fetcher = async (url, options) => {
    calls.push({ url: new URL(url), options });
    return Response.json(options.method === "GET" ? { count: 1, assets: payload.assets } : { ok: true });
  };
  await main(["assets", "upload", "--file", source], store, () => {}, fetcher);
  await main(["assets", "download", "--output", destination, "--task-id", "7"], store, () => {}, fetcher);
  await main(["assets", "delete", "--ids", "12,13,12"], store, () => {}, fetcher);
  assert.deepEqual(calls.map(({ url, options }) => [options.method, url.pathname, businessSearch(url)]), [
    ["POST", "/api/local-agent/assets", ""],
    ["GET", "/api/local-agent/assets", "?task_id=7"],
    ["DELETE", "/api/local-agent/assets", ""],
  ]);
  assert.deepEqual(JSON.parse(calls[0].options.body.toString()), payload);
  assert.deepEqual(JSON.parse(calls[2].options.body.toString()), { ids: [12, 13] });
  for (const { url, options } of calls) {
    const signatureMessage = [options.method, `${url.pathname}${url.search}`, options.headers["X-ARTEX-Timestamp"], options.headers["X-ARTEX-Nonce"], awaitDigest(options.body ?? Buffer.alloc(0))].join("\n");
    assert.ok(verify(null, Buffer.from(signatureMessage), createPublicKey(identity.privateKey), Buffer.from(options.headers["X-ARTEX-Signature"], "base64")));
  }
  assert.deepEqual(JSON.parse(await readFile(destination, "utf8")), { count: 1, assets: payload.assets });
  await assert.rejects(main(["assets", "download", "--output", destination], store, () => {}, fetcher), /EEXIST/);
  await assert.rejects(main(["assets", "delete", "--ids", "1,nope"], store, () => {}, fetcher), /正整数列表/);
});

test("LLM 和工具写命令使用受保护的明确管理路由", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-admin-"));
  const llmFile = path.join(directory, "llm.json");
  const profileFile = path.join(directory, "profile.json");
  const toolFile = path.join(directory, "tool.json");
  const customFile = path.join(directory, "custom.json");
  await writeFile(llmFile, JSON.stringify({ provider: "openai", model: "test", api_key: "secret" }));
  await writeFile(profileFile, JSON.stringify({ name: "备用", provider: "openai", model: "test" }));
  await writeFile(toolFile, JSON.stringify({ description: "更新", schema: {}, agents: [], enabled: true }));
  await writeFile(customFile, JSON.stringify({ key: "custom_test", description: "测试", kind: "shell", enabled: true }));
  const store = { load: async () => identity };
  const calls = [];
  const fetcher = async (url, options) => { calls.push({ url: new URL(url), options }); return Response.json({ ok: true }); };
  await main(["llm", "set", "--file", llmFile], store, () => {}, fetcher);
  await main(["llm", "profile-save", "--file", profileFile], store, () => {}, fetcher);
  await main(["llm", "activate", "--id", "4"], store, () => {}, fetcher);
  await main(["tools", "set", "--key", "clock", "--file", toolFile], store, () => {}, fetcher);
  await main(["tools", "custom-save", "--file", customFile], store, () => {}, fetcher);
  await main(["tools", "custom-delete", "--key", "custom_test"], store, () => {}, fetcher);
  assert.deepEqual(calls.map(({ url, options }) => [options.method, url.pathname]), [
    ["POST", "/api/local-agent/llm"], ["POST", "/api/local-agent/llm/profiles"], ["POST", "/api/local-agent/llm/profiles/active"],
    ["PUT", "/api/local-agent/tools/clock"], ["POST", "/api/local-agent/tools/custom"], ["DELETE", "/api/local-agent/tools/custom/custom_test"],
  ]);
  assert.deepEqual(JSON.parse(calls[2].options.body.toString()), { id: 4 });
  for (const { url, options } of calls) {
    const signatureMessage = [options.method, `${url.pathname}${url.search}`, options.headers["X-ARTEX-Timestamp"], options.headers["X-ARTEX-Nonce"], awaitDigest(options.body ?? Buffer.alloc(0))].join("\n");
    assert.ok(verify(null, Buffer.from(signatureMessage), createPublicKey(identity.privateKey), Buffer.from(options.headers["X-ARTEX-Signature"], "base64")));
  }
});

test("可复制 Skill 中的独立客户端与仓库入口保持一致", async () => {
  const canonical = await readFile(new URL("./artex-agent.mjs", import.meta.url));
  const bundled = await readFile(new URL("../integrations/local-agent/artex-agent/scripts/artex-agent.mjs", import.meta.url));
  assert.ok(canonical.equals(bundled), "修改客户端后须同步 Skill 包内的独立脚本");
});

test("Windows BOM JSON 可用于任务、资产、模型和工具且不回显坏文件内容", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-bom-"));
  const source = path.join(directory, "input.json");
  const store = { load: async () => identity };
  const calls = [];
  const fetcher = async (_url, options) => { calls.push(JSON.parse(options.body)); return Response.json({ ok: true }); };
  for (const args of [["tasks", "create"], ["assets", "upload"], ["llm", "set"], ["tools", "custom-save"]]) {
    await writeFile(source, '\uFEFF' + JSON.stringify({ assets: [], description: "测试" }));
    await main([...args, "--file", source], store, () => {}, fetcher);
  }
  assert.equal(calls.length, 4);
  await writeFile(source, '\uFEFF{"api_key":"private-value",}');
  await assert.rejects(main(["llm", "set", "--file", source], store, () => {}, fetcher), error => /JSON 文件格式无效/.test(error.message) && !error.message.includes("private-value"));
  assert.equal(calls.length, 4);
});

test("大整数写目标和被忽略的参数在发送前拒绝", async () => {
  const store = { load: async () => identity };
  let calls = 0;
  const fetcher = async () => { calls++; return Response.json({ ok: true }); };
  for (const args of [["assets", "delete", "--ids", "9007199254740993"], ["llm", "activate", "--id", "9007199254740993"]]) {
    await assert.rejects(main(args, store, () => {}, fetcher), /9007199254740991/);
  }
  await assert.rejects(main(["conversations", "stop", "--id", "1", "--message", "不能丢弃"], store, () => {}, fetcher), /不支持参数/);
  await assert.rejects(main(["discover", "stats", "--limit", "2"], store, () => {}, fetcher), /不支持参数/);
  await assert.rejects(main(["assets", "list", "--task-id", "0"], store, () => {}, fetcher), /正整数/);
  await assert.rejects(main(["assets", "download", "--all", "--task-id", "9223372036854775808", "--output", "unused.json"], store, () => {}, fetcher), /服务端 ID 上限/);
  await assert.rejects(main(["tasks", "asset-refs", "--id", "50"], store, () => {}, fetcher), /缺少 --asset-id/);
  assert.equal(calls, 0);
});

test("资产完整导出按服务端页长推进，单页下载明确显示未完整", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-export-"));
  const destination = path.join(directory, "all.json");
  const store = { load: async () => identity };
  const offsets = [];
  const fetcher = async url => {
    assert.equal(url.searchParams.get("type"), "ip");
    const offset = Number(url.searchParams.get("offset") ?? 0);
    offsets.push(offset);
    return Response.json({ total: 3, count: offset ? 1 : 2, assets: offset ? [{ id: 3, type: "ip", ip: "192.0.2.3" }] : [{ id: 1 }, { id: 2 }] });
  };
  const output = [];
  await main(["assets", "download", "--type", "ip", "--all", "--output", destination], store, v => output.push(v), fetcher);
  assert.deepEqual(offsets, [0, 2]);
  assert.equal(output[0].complete, true);
  assert.equal(output[0].count, 3);
  const data = JSON.parse(await readFile(destination, "utf8"));
  assert.equal(data.assets.length, 3);
  await main(["assets", "upload", "--file", destination], store, () => {}, async (_url, options) => {
    assert.equal(JSON.parse(options.body).assets.length, 3);
    return Response.json({ count: 3 });
  });
  await main(["assets", "download", "--type", "ip", "--output", path.join(directory, "page.json")], store, v => output.push(v), fetcher);
  assert.equal(output[1].complete, false);
  assert.match(output[1].message, /一页/);
});

test("完整导出发现重复、数量变化、中断时不留下成功文件或自动重试", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-export-fail-"));
  const store = { load: async () => identity };
  for (const mode of ["duplicate", "changed", "network", "empty"]) {
    const destination = path.join(directory, mode + ".json");
    let calls = 0;
    const fetcher = async () => {
      calls++;
      if (calls === 1) return Response.json({ assets: [{ id: 1 }], total: 2 });
      if (mode === "network") throw new TypeError("fetch failed");
      return Response.json({ assets: mode === "empty" ? [] : [{ id: mode === "duplicate" ? 1 : 2 }], total: mode === "changed" ? 3 : 2 });
    };
    await assert.rejects(main(["assets", "download", "--all", "--type", "ip", "--output", destination], store, () => {}, fetcher));
    assert.equal(calls, 2);
    await assert.rejects(stat(destination), /ENOENT/);
  }
});

test("Skill 文件读取、下载、修改和自定义工具更新使用精确签名路由", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-skill-files-"));
  const file = path.join(directory, "tool.json");
  await writeFile(file, JSON.stringify({ kind: "command", exec: { command: "printf ok" } }));
  const store = { load: async () => identity };
  const calls = [];
  const fetcher = async (url, options) => {
    calls.push([options.method, url.pathname, options.body && JSON.parse(options.body)]);
    return url.pathname.endsWith("/download") ? new Response(Buffer.from([80, 75, 3, 4])) : Response.json({ ok: true });
  };
  await main(["skills", "files", "--name", "sample"], store, () => {}, fetcher);
  await main(["skills", "read", "--name", "sample", "--path", "references/说明.md"], store, () => {}, fetcher);
  await main(["skills", "write", "--name", "sample", "--path", "SKILL.md", "--content", ""], store, () => {}, fetcher);
  await main(["skills", "download", "--name", "sample", "--output", path.join(directory, "skill.zip")], store, () => {}, fetcher);
  await main(["tools", "custom-update", "--key", "custom_one", "--file", file], store, () => {}, fetcher);
  await main(["workspace", "write", "--path", "empty.txt", "--content", ""], store, () => {}, fetcher);
  assert.deepEqual(calls.map(v => v.slice(0, 2)), [
    ["GET", "/api/local-agent/skills/sample/files"],
    ["GET", "/api/local-agent/skills/sample/files/references/%E8%AF%B4%E6%98%8E.md"],
    ["PUT", "/api/local-agent/skills/sample/files/SKILL.md"],
    ["GET", "/api/local-agent/skills/sample/download"],
    ["PUT", "/api/local-agent/tools/custom/custom_one"],
    ["POST", "/api/local-agent/workspace/write"],
  ]);
  assert.deepEqual(calls[2][2], { content: "" });
  assert.deepEqual(calls[5][2], { path: "empty.txt", content: "" });
  await assert.rejects(main(["skills", "read", "--name", "sample", "--path", "../secret"], store, () => {}, fetcher), /相对路径/);
  assert.equal(calls.length, 6);
});

test("新增只读入口不创建任务、对话或触发模型", async () => {
  const store = { load: async () => identity };
  const cases = [
    [["conversations", "agents"], "/conversations/agents", ""],
    [["tasks", "coverage", "--id", "50"], "/tasks/50/coverage", ""],
    [["tasks", "asset-refs", "--id", "50", "--asset-id", "7"], "/tasks/50/asset-refs", "?asset_id=7"],
    [["discover", "intents", "--task-id", "50"], "/discovery/intents", "?task=50"],
    [["discover", "intents", "--task-id", "50", "--page", "1", "--before", "10", "--limit", "20"], "/discovery/intents", "?task=50&limit=20&page=1&before=10"],
    [["discover", "finding", "--id", "12", "--context-task", "50"], "/discovery/findings/12", "?context_task=50"],
    [["llm", "models", "--id", "2"], "/llm/profiles/2/models", ""],
    [["mcp", "tools", "--id", "2"], "/mcp/2/tools", ""],
    [["assets", "counts", "--task-id", "50"], "/assets/counts", "?task_id=50"],
  ];
  let calls = 0;
  for (const [args, resource, query] of cases) {
    await main(args, store, () => {}, async (url, options) => {
      calls++;
      assert.equal(options.method, "GET");
      assert.equal(url.pathname, "/api/local-agent" + resource);
      assert.equal(businessSearch(url), query);
      return Response.json({ ok: true });
    });
  }
  assert.equal(calls, cases.length);
});
