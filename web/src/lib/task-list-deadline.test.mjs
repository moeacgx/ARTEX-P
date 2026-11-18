import { api, http, httpWithDeadline, RequestTimeoutError } from "./api.ts";
import assert from "node:assert/strict";
import test from "node:test";

const originalFetch = globalThis.fetch;
test.afterEach(() => {
  globalThis.fetch = originalFetch;
});

// A fetch whose headers never arrive; it only settles when its signal aborts (if it honours it).
function hangingHeaders({ honourSignal = true } = {}) {
  const seen = [];
  globalThis.fetch = (url, init) => {
    seen.push({ url, signal: init?.signal });
    return new Promise((_, reject) => {
      if (honourSignal) init?.signal?.addEventListener("abort", () => reject(init.signal.reason), { once: true });
    });
  };
  return seen;
}

// Headers arrive immediately but the JSON body never finishes.
function hangingBody({ honourSignal = true } = {}) {
  const seen = [];
  globalThis.fetch = async (url, init) => {
    seen.push({ url, signal: init?.signal });
    return {
      ok: true,
      status: 200,
      json: () =>
        new Promise((_, reject) => {
          if (honourSignal) init?.signal?.addEventListener("abort", () => reject(init.signal.reason), { once: true });
        }),
    };
  };
  return seen;
}

for (const [label, install] of [
  ["headers", hangingHeaders],
  ["body", hangingBody],
]) {
  for (const honourSignal of [true, false]) {
    test(`tasks deadline rejects when ${label} never completes (fetch honours signal: ${honourSignal})`, async () => {
      const seen = install({ honourSignal });
      const started = Date.now();
      await assert.rejects(api.tasks({ timeoutMs: 40 }), (error) => {
        assert.ok(error instanceof RequestTimeoutError);
        assert.equal(error.path, "/tasks");
        return true;
      });
      assert.ok(Date.now() - started < 2_000, "deadline must fire promptly");
      assert.equal(seen.length, 1);
      assert.equal(seen[0].url, "/api/tasks");
      assert.ok(seen[0].signal instanceof AbortSignal, "fetch receives an abort signal");
      assert.equal(seen[0].signal.aborted, true, "the stalled fetch is aborted, not leaked");
    });
  }
}

test("a later request is issued and succeeds after a timed-out one", async () => {
  hangingBody();
  await assert.rejects(api.tasks({ timeoutMs: 30 }), RequestTimeoutError);
  let calls = 0;
  globalThis.fetch = async (url) => {
    calls++;
    assert.equal(url, "/api/tasks");
    return Response.json({ tasks: [{ id: "t1" }], active: "t1" });
  };
  const result = await api.tasks({ timeoutMs: 1_000 });
  assert.equal(calls, 1);
  assert.deepEqual(result, { tasks: [{ id: "t1" }], active: "t1" });
});

test("caller abort (unmount) rejects with the caller reason, not a timeout", async () => {
  const seen = hangingHeaders({ honourSignal: false });
  const controller = new AbortController();
  const pending = api.tasks({ signal: controller.signal, timeoutMs: 10_000 });
  controller.abort(new DOMException("unmounted", "AbortError"));
  await assert.rejects(pending, (error) => {
    assert.ok(!(error instanceof RequestTimeoutError));
    assert.equal(error.name, "AbortError");
    return true;
  });
  assert.equal(seen[0].signal.aborted, true);
});

test("already-aborted caller signal does not issue a request", async () => {
  let calls = 0;
  globalThis.fetch = async () => {
    calls++;
    return Response.json({ tasks: [], active: "" });
  };
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(api.tasks({ signal: controller.signal, timeoutMs: 1_000 }));
  assert.equal(calls, 0);
});

test("fast responses resolve normally and HTTP errors keep their APIError path", async () => {
  globalThis.fetch = async () => Response.json({ tasks: null }, { status: 200 });
  assert.deepEqual(await api.tasks({ timeoutMs: 1_000 }), { tasks: [], active: "" });
  globalThis.fetch = async () => Response.json({ error: "数据库暂不可用" }, { status: 503 });
  await assert.rejects(api.tasks({ timeoutMs: 1_000 }), (error) => {
    assert.equal(error.name, "APIError");
    assert.equal(error.message, "数据库暂不可用");
    return true;
  });
});

test("invalid deadlines fail fast without fetching", async () => {
  let calls = 0;
  globalThis.fetch = async () => {
    calls++;
    return Response.json({});
  };
  for (const value of [0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
    await assert.rejects(httpWithDeadline("/tasks", undefined, value), /无效的请求期限/);
  }
  assert.equal(calls, 0);
});

test("plain http and api.tasks() without options stay unbounded (no implicit signal)", async () => {
  const signals = [];
  globalThis.fetch = async (_url, init) => {
    signals.push(init?.signal);
    return Response.json({ tasks: [], active: "" });
  };
  await http("/anything");
  await api.tasks();
  assert.deepEqual(signals, [undefined, undefined]);
});
