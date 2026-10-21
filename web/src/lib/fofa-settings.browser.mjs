// 通过 playwright-cli run-code --filename 执行；仅使用本地模拟接口，不调用 FOFA。
async (page) => {
  const check = (condition, message) => {
    if (!condition) throw new Error(message);
  };
  const fixture = { config: { enabled: false, api_key_set: false, request_interval_seconds: 2, rate_limit_cooldown_seconds: 60 }, failLoad: false, failWrite: false, calls: [] };
  page.setDefaultTimeout(8000);
  await page.setViewportSize({ width: 1440, height: 1100 });
  await page.addInitScript(() => {
    localStorage.setItem("artex_token", "synthetic-fofa-ui-test");
  });
  await page.context().addCookies([{ name: "artex_token", value: "synthetic-fofa-ui-test", url: "http://127.0.0.1:3098" }]);
  await page.unroute("**/api/**");
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const method = request.method();
    if (path === "/api/integrations/fofa") {
      fixture.calls.push({ path, method, body: request.postDataJSON() });
      if (method === "GET" && fixture.failLoad) return route.fulfill({ status: 503, json: { error: "合成读取失败" } });
      if (method === "PUT") {
        if (fixture.failWrite) return route.fulfill({ status: 503, json: { error: "合成保存失败" } });
        const body = request.postDataJSON();
        fixture.config.enabled = body.enabled ?? fixture.config.enabled;
        if (Object.hasOwn(body, "api_key")) fixture.config.api_key_set = body.api_key !== "";
        if (Object.hasOwn(body, "request_interval_seconds")) fixture.config.request_interval_seconds = body.request_interval_seconds;
        if (Object.hasOwn(body, "rate_limit_cooldown_seconds")) fixture.config.rate_limit_cooldown_seconds = body.rate_limit_cooldown_seconds;
      }
      return route.fulfill({ json: fixture.config });
    }
    if (path === "/api/integrations/fofa/test") {
      fixture.calls.push({ path, method, body: request.postDataJSON() });
      return route.fulfill({ json: { ok: true, message: "合成连接成功" } });
    }
    if (path === "/api/health") return route.fulfill({ json: { ok: true, service: "ARTEX", version: "dev-fofa-ui" } });
    if (path === "/api/update/check") return route.fulfill({ json: { current: "dev-fofa-ui", has_update: false, supported: true, comparable: false } });
    return route.fulfill({ json: {} });
  });
  await page.goto("http://127.0.0.1:3098/system/settings", { timeout: 60000 });
  const card = page.locator('[data-slot="card"]').filter({ has: page.getByText("FOFA 资产搜索", { exact: true }) });
  const key = card.getByLabel("API Key", { exact: true });
  const interval = card.getByLabel("请求最小间隔（秒）", { exact: true });
  const cooldown = card.getByLabel("限流冷却时间（秒）", { exact: true });
  const toggle = card.getByRole("switch", { name: "启用 FOFA 工具" });
  const save = card.getByRole("button", { name: "保存配置", exact: true });
  const test = card.getByRole("button", { name: "测试连接", exact: true });
  const clear = card.getByRole("button", { name: "清除 Key 并关闭", exact: true });
  const enable = card.getByRole("button", { name: "一键启用 FOFA", exact: true });
  const saveAndEnable = card.getByRole("button", { name: "保存并启用", exact: true });
  await key.waitFor();
  check(await toggle.isDisabled(), "未配置密钥时不应允许开启");
  check(await save.isDisabled(), "未修改时不应允许空白保存");
  check(await key.getAttribute("type") === "text", "凭据输入不应触发登录密码识别");
  check(await key.getAttribute("autocomplete") === "off", "凭据输入应禁用浏览器自动填充");
  check(await key.evaluate((input) => getComputedStyle(input).webkitTextSecurity) === "disc", "凭据输入应保持遮罩");
  check(await interval.inputValue() === "2" && await cooldown.inputValue() === "60", "节流输入未显示服务端默认值");
  await interval.fill("7");
  await cooldown.fill("180");
  await save.click();
  await save.waitFor({ state: "visible" });
  const throttleSave = fixture.calls.filter((call) => call.method === "PUT").at(-1).body;
  check(throttleSave.request_interval_seconds === 7 && throttleSave.rate_limit_cooldown_seconds === 180 && !Object.hasOwn(throttleSave, "api_key") && !Object.hasOwn(throttleSave, "enabled"), "仅节流保存不得覆盖Key/开关或遗漏数字");
  check(!fixture.config.enabled && !fixture.config.api_key_set, "保存节流不能自动开启或创建Key");

  await key.fill("synthetic-first-key");
  const writesBeforeTest = fixture.calls.filter((call) => call.method === "PUT").length;
  await test.click();
  await page.getByText(/合成连接成功.*尚未启用/).waitFor();
  check(fixture.calls.filter((call) => call.method === "PUT").length === writesBeforeTest, "连接测试不能保存配置");
  check(fixture.calls.some((call) => call.path.endsWith("/test") && call.body.api_key === "synthetic-first-key"), "应测试未保存的输入密钥");

  await save.click();
  await card.getByText("Key 已保存，尚未启用", { exact: true }).waitFor();
  check(await key.inputValue() === "", "保存成功后应移除内存中的输入密钥");
  check(!await toggle.isChecked() && !fixture.config.enabled, "只保存密钥不能自动开启工具");
  check(await card.getByText("已启用", { exact: true }).count() === 0, "只保存密钥不能误显示已启用");
  await card.screenshot({ path: "output/playwright/fofa-settings/saved-disabled.png" });
  await test.click();
  await test.waitFor({ state: "visible" });
  check(fixture.calls.filter((call) => call.path.endsWith("/test")).at(-1).body.api_key === undefined, "空白测试应使用已有密钥");
  const beforeEnable = fixture.calls.filter((call) => call.method === "PUT").length;
  const testsBeforeEnable = fixture.calls.filter((call) => call.path.endsWith("/test")).length;
  await interval.fill("9");
  await cooldown.fill("240");
  await enable.evaluate((button) => { button.click(); button.click(); });
  await card.getByText("已启用", { exact: true }).waitFor();
  check(await toggle.isChecked(), "明确启用后开关应打开");
  const enableCalls = fixture.calls.filter((call) => call.method === "PUT").slice(beforeEnable);
  check(enableCalls.length === 1 && JSON.stringify(enableCalls[0].body) === '{"enabled":true}', "一键启用必须只提交开关且防双击");
  check(fixture.calls.filter((call) => call.path.endsWith("/test")).length === testsBeforeEnable, "启用不能自动测试或查询");
  check(await interval.inputValue() === "9" && await cooldown.inputValue() === "240", "一键启用不能丢弃未保存的节流草稿");
  check(fixture.config.request_interval_seconds === 7 && fixture.config.rate_limit_cooldown_seconds === 180, "一键启用不能提交节流草稿");
  await interval.fill("7");
  await cooldown.fill("180");

  await toggle.click();
  await save.click();
  await save.waitFor({ state: "visible" });
  const disabled = fixture.calls.filter((call) => call.method === "PUT").at(-1);
  check(disabled.body.enabled === false && !Object.hasOwn(disabled.body, "api_key"), "只保存开关不能清除旧密钥");
  check(fixture.config.api_key_set, "只关闭开关应保留密钥");
  await key.fill("synthetic-replacement-key");
  await save.click();
  await save.waitFor({ state: "visible" });
  check(fixture.calls.filter((call) => call.method === "PUT").at(-1).body.api_key === "synthetic-replacement-key", "更换密钥未提交");
  check(await key.inputValue() === "", "更换成功后应清空草稿");
  await card.getByText("Key 已保存，尚未启用", { exact: true }).waitFor();
  await card.screenshot({ path: "output/playwright/fofa-settings/configured.png" });

  await clear.click();
  await card.getByText("未配置", { exact: true }).waitFor();
  const cleared = fixture.calls.filter((call) => call.method === "PUT").at(-1).body;
  check(cleared.api_key === "" && cleared.enabled === false, "清除必须明确提交空串并关闭工具");
  check(!await toggle.isChecked() && await toggle.isDisabled(), "清除后应关闭并禁用开关");
  check(await interval.inputValue() === "7" && await cooldown.inputValue() === "180", "清除Key不应重置已保存节流设置");

  await key.fill("synthetic-save-enable-key");
  await saveAndEnable.click();
  await card.getByText("已启用", { exact: true }).waitFor();
  check(fixture.calls.filter((call) => call.method === "PUT").at(-1).body.enabled === true, "保存并启用须明确提交开启");
  check(fixture.calls.filter((call) => call.method === "PUT").at(-1).body.api_key === "synthetic-save-enable-key", "保存并启用须提交当前输入Key");
  await clear.click();
  await card.getByText("未配置", { exact: true }).waitFor();

  fixture.failLoad = true;
  await page.reload();
  await card.getByRole("alert").waitFor();
  check(await card.getByText("未配置", { exact: true }).count() === 0, "读取失败不能误显示未配置");
  check(await key.count() === 0, "读取失败不能开放未知配置写入");
  fixture.failLoad = false;
  await card.getByRole("button", { name: "重新读取", exact: true }).click();
  await key.waitFor();
  await key.fill("synthetic-failed-write-key");
  await toggle.click();
  const beforeInvalid = fixture.calls.filter((call) => call.method === "PUT").length;
  await interval.fill("0");
  await save.click();
  await page.getByText(/保存失败：请求间隔必须为/).waitFor();
  check(fixture.calls.filter((call) => call.method === "PUT").length === beforeInvalid, "无效请求间隔不应发出PUT");
  await interval.fill("7");
  await cooldown.fill("3601");
  await save.click();
  await page.getByText(/保存失败：限流冷却必须为/).waitFor();
  check(fixture.calls.filter((call) => call.method === "PUT").length === beforeInvalid, "无效冷却不应发出PUT");
  await cooldown.fill("180");
  fixture.failWrite = true;
  const before = fixture.calls.filter((call) => call.method === "PUT").length;
  await save.evaluate((button) => { button.click(); button.click(); });
  await page.getByText("保存失败：合成保存失败", { exact: true }).waitFor();
  check(fixture.calls.filter((call) => call.method === "PUT").length === before + 1, "双击不能重复保存");
  check(await key.inputValue() === "synthetic-failed-write-key", "保存失败应保留草稿供修正");
  check(!fixture.config.enabled && !fixture.config.api_key_set, "保存失败不能伪装成功");
  return { passed: true, synthetic: true, cases: 22, writes: fixture.calls.filter((call) => call.method === "PUT").length, searches: 0 };
}
