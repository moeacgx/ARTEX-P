import { XIEQU_DEFAULT_BASE_URL, xiequAPI, xiequBaseURLError } from "./xiequ-api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const originalFetch = globalThis.fetch;
test.afterEach(() => {
  globalThis.fetch = originalFetch;
});

// Fake, non-secret fixture values only.
const config = (state = {}) => ({
  enabled: true,
  configured: true,
  uid: "uid-fixture",
  ukey_set: true,
  base_url: XIEQU_DEFAULT_BASE_URL,
  memo: "artex",
  refresh_interval_seconds: 300,
  proxy_domains: ["xiequ.cn"],
  max_whitelist_entries: 5,
  quota_hint: "提示",
  auto_refresh_on_proxy_use: true,
  state: {
    managed_ips: [],
    last_ip: "",
    last_status: "",
    last_error: "",
    whitelist_count: 0,
    last_attempt_at: null,
    last_success_at: null,
    ...state,
  },
});

test("default management endpoint is HTTPS", () => {
  assert.equal(new URL(XIEQU_DEFAULT_BASE_URL).protocol, "https:");
  assert.equal(xiequBaseURLError(XIEQU_DEFAULT_BASE_URL), "");
});

test("base URL validation rejects HTTP downgrade and unsafe forms", () => {
  assert.equal(xiequBaseURLError(""), "", "empty uses the server default");
  assert.equal(xiequBaseURLError("  "), "");
  assert.match(xiequBaseURLError("http://op.xiequ.cn/IpWhiteList.aspx"), /HTTPS/);
  assert.match(xiequBaseURLError("HTTP://op.xiequ.cn/"), /HTTPS/);
  assert.match(xiequBaseURLError("ftp://op.xiequ.cn/"), /HTTPS/);
  assert.match(xiequBaseURLError("not a url"), /无效/);
  assert.match(xiequBaseURLError("https://u:p@op.xiequ.cn/"), /不能包含/);
  assert.match(xiequBaseURLError("https://op.xiequ.cn/a?uid=1"), /不能包含/);
  assert.match(xiequBaseURLError("https://op.xiequ.cn/a#x"), /不能包含/);
  assert.equal(xiequBaseURLError(" https://op.xiequ.cn/IpWhiteList.aspx "), "");
});

test("config accepts managed_ips as an array (backend DTO contract)", async () => {
  globalThis.fetch = async () => Response.json(config({ managed_ips: ["203.0.113.7"] }));
  const value = await xiequAPI.config();
  assert.deepEqual(value.state.managed_ips, ["203.0.113.7"]);
});

for (const [label, managed] of [
  ["null", null],
  ["missing", undefined],
  ["string", "203.0.113.7"],
  ["mixed array", ["203.0.113.7", 7]],
]) {
  test(`config stays strict when managed_ips is ${label}`, async () => {
    const body = config();
    if (managed === undefined) delete body.state.managed_ips;
    else body.state.managed_ips = managed;
    globalThis.fetch = async () => Response.json(body);
    await assert.rejects(xiequAPI.config(), /状态响应无效/);
  });
}
