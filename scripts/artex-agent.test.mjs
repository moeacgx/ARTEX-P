import assert from "node:assert/strict";
import { createHash, createPublicKey, verify } from "node:crypto";
import { mkdtemp, readFile, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { ArtexAgentClient, IdentityStore, generateIdentity, main, normalizeBaseUrl, pairDevice, signedHeaders } from "./artex-agent.mjs";

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

test("上传采用同一份已签名 multipart 正文且默认不覆盖", async () => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "artex-agent-upload-"));
  const file = path.join(directory, "skill.zip");
  await writeFile(file, Buffer.from([0x50, 0x4b, 3, 4, 0, 1, 2]));
  let content;
  const client = new ArtexAgentClient(identity, async (url, options) => {
    assert.equal(url.search, "");
    assert.match(options.headers["content-type"], /^multipart\/form-data/);
    content = options.body;
    const message = ["POST", url.pathname, options.headers["X-ARTEX-Timestamp"], options.headers["X-ARTEX-Nonce"], awaitDigest(content)].join("\n");
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

test("可复制 Skill 中的独立客户端与仓库入口保持一致", async () => {
  const canonical = await readFile(new URL("./artex-agent.mjs", import.meta.url));
  const bundled = await readFile(new URL("../integrations/local-agent/artex-agent/scripts/artex-agent.mjs", import.meta.url));
  assert.ok(canonical.equals(bundled), "修改客户端后须同步 Skill 包内的独立脚本");
});
