// Original CRW test (no CXC counterpart): the api.ts foundation this issue writes new -
// the token moves out of the address, the token and JSON headers are attached to writes
// only, and a non-2xx response throws {status, error}.
//
// Each test loads the module fresh (a unique query defeats the ESM cache), because api.ts
// keeps a tab-local token and a storage-denied flag. Sharing one instance across tests
// would make the assertions depend on the order they run in.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { ApiError } from "../src/api.ts";

interface FakeLocation {
  hash: string;
  pathname: string;
  search: string;
}

interface Fake {
  location: FakeLocation;
  replaced: string[];
  stored: Map<string, string>;
  /** Every key removeItem was called with, so a test can see the clear attempt. */
  removed: string[];
  fetched: Array<{ url: string; init?: RequestInit }>;
  /** True makes sessionStorage.setItem throw, as a storage-denied context does. */
  storageRefused: boolean;
  /** True makes sessionStorage.removeItem throw, so the old stored token stays behind. */
  storageRemoveRefused: boolean;
}

const GLOBALS = globalThis as unknown as Record<string, unknown>;

let moduleSeq = 0;

interface Api {
  TOKEN_STORAGE_KEY: string;
  bootstrapToken(): void;
  getToken(): string | null;
  request<T>(path: string, options?: { method?: string; body?: string }): Promise<T>;
  getPolicy(signal?: AbortSignal): Promise<unknown>;
  writePolicy(payload: { expectedDigest: string; change: unknown }): Promise<{ status: number; body: unknown }>;
  checkPolicy(payload: { expectedDigest: string; change: unknown }): Promise<{ status: number; body: unknown }>;
}

/** A fresh api.ts instance, so the module's tab-local state never leaks between tests. */
async function freshApi(): Promise<Api> {
  return (await import(`../src/api.ts?case=${moduleSeq++}`)) as unknown as Api;
}

function install(hash: string, status: number, payload: unknown): Fake {
  const replaced: string[] = [];
  const stored = new Map<string, string>();
  const removed: string[] = [];
  const fetched: Array<{ url: string; init?: RequestInit }> = [];
  const location: FakeLocation = { hash, pathname: "/", search: "" };
  GLOBALS.location = location;
  GLOBALS.history = {
    replaceState: (_state: unknown, _title: string, url: string) => {
      replaced.push(url);
    },
  };
 const sessionStorageFake = {
    getItem: (key: string) => (stored.has(key) ? stored.get(key) : null),
    setItem: (key: string, value: string) => {
      if (sessionStorageFake.refused) throw new Error("storage denied");
      stored.set(key, value);
    },
    removeItem: (key: string) => {
      removed.push(key);
      // A context can refuse the cleanup as well as the write; removeRefused models that, so a test
      // can leave the old value in storage and prove the in-memory token still wins.
      if (sessionStorageFake.removeRefused) throw new Error("storage denied");
      stored.delete(key);
    },
    refused: false,
    removeRefused: false,
  };
  GLOBALS.sessionStorage = sessionStorageFake;
  GLOBALS.fetch = async (url: string, init?: RequestInit) => {
    fetched.push({ url, init });
    return new Response(JSON.stringify(payload), { status });
  };
  const fake = { location, replaced, stored, removed, fetched, storageRefused: false, storageRemoveRefused: false };
  Object.defineProperty(fake, "storageRefused", {
    get: () => sessionStorageFake.refused,
    set: (value: boolean) => {
      sessionStorageFake.refused = value;
    },
  });
  Object.defineProperty(fake, "storageRemoveRefused", {
    get: () => sessionStorageFake.removeRefused,
    set: (value: boolean) => {
      sessionStorageFake.removeRefused = value;
    },
  });
  return fake;
}

function uninstall(): void {
  delete GLOBALS.location;
  delete GLOBALS.history;
  delete GLOBALS.sessionStorage;
  delete GLOBALS.fetch;
}

function headersOf(fake: Fake, index = 0): Record<string, string> {
  return (fake.fetched[index]?.init?.headers ?? {}) as Record<string, string>;
}

test("the fragment token is stored and stripped from the address", async () => {
  const api = await freshApi();
  const fake = install("#token=s3cret-token", 200, {});
  try {
    api.bootstrapToken();
    assert.equal(fake.stored.get(api.TOKEN_STORAGE_KEY), "s3cret-token");
    assert.equal(fake.replaced.length, 1);
    assert.ok(!fake.replaced[0].includes("s3cret-token"));
  } finally {
    uninstall();
  }
});

test("the token is stored byte for byte and other fragment parameters survive", async () => {
  const api = await freshApi();
  const fake = install("#token=a+b%2Fc&view=1", 200, {});
  try {
    api.bootstrapToken();
    // URLSearchParams would have decoded the plus to a space and the %2F to a slash;
    // the token alphabet belongs to the server, so the client must not alter it.
    assert.equal(fake.stored.get(api.TOKEN_STORAGE_KEY), "a+b%2Fc");
    assert.equal(fake.replaced.length, 1);
    assert.equal(fake.replaced[0], "/#view=1");
  } finally {
    uninstall();
  }
});

test("a hash without a token is left alone and bootstrapToken is idempotent", async () => {
  const api = await freshApi();
  const fake = install("#/policy", 200, {});
  try {
    api.bootstrapToken();
    api.bootstrapToken();
    assert.equal(fake.replaced.length, 0);
    assert.equal(fake.location.hash, "#/policy");
    assert.equal(api.getToken(), null);
  } finally {
    uninstall();
  }
});

test("a storage that refuses the write still leaves this tab able to write", async () => {
  const api = await freshApi();
  const fake = install("#token=blocked-token", 200, {});
  // sessionStorage.setItem throws (a storage-denied context). The token must survive in
  // memory: stripping it from the address without keeping a copy would make every later
  // write go out unauthenticated.
  fake.storageRefused = true;
  try {
    api.bootstrapToken();
    assert.equal(fake.stored.has(api.TOKEN_STORAGE_KEY), false);
    assert.equal(api.getToken(), "blocked-token");
    assert.equal(fake.replaced.length, 1);
    assert.ok(!fake.replaced[0].includes("blocked-token"));
    await api.request("/api/policy", { method: "POST", body: "{}" });
    assert.equal(headersOf(fake)["X-CRW-Token"], "blocked-token");
  } finally {
    uninstall();
  }
});

test("a read request carries neither the token nor a JSON content type", async () => {
  const api = await freshApi();
  const fake = install("", 200, { ok: true });
  try {
    fake.stored.set(api.TOKEN_STORAGE_KEY, "stored-token");
    const body = await api.request<{ ok: boolean }>("/api/status");
    assert.deepEqual(body, { ok: true });
    assert.equal(fake.fetched.length, 1);
    assert.equal(headersOf(fake)["X-CRW-Token"], undefined);
    assert.equal(headersOf(fake)["Content-Type"], undefined);
  } finally {
    uninstall();
  }
});

test("a write request carries the token and the JSON content type", async () => {
  const api = await freshApi();
  const fake = install("", 200, { saved: true });
  try {
    fake.stored.set(api.TOKEN_STORAGE_KEY, "stored-token");
    await api.request("/api/policy", { method: "POST", body: JSON.stringify({ a: 1 }) });
    assert.equal(fake.fetched.length, 1);
    assert.equal(headersOf(fake)["X-CRW-Token"], "stored-token");
    assert.equal(headersOf(fake)["Content-Type"], "application/json");
  } finally {
    uninstall();
  }
});

test("a HEAD response resolves without parsing a body", async () => {
  const api = await freshApi();
  const fake = install("", 200, {});
  try {
    // isWriteMethod() classifies HEAD as a read, and a HEAD response has no body; parsing
    // one would reject a successful call with a SyntaxError.
    const result = await api.request("/api/status", { method: "HEAD" });
    assert.equal(result, undefined);
    assert.equal(fake.fetched.length, 1);
    assert.equal(headersOf(fake)["X-CRW-Token"], undefined);
  } finally {
    uninstall();
  }
});

test("a non-2xx response throws the status and the server error", async () => {
  const api = await freshApi();
  const fake = install("", 409, { error: "conflict" });
  try {
    let caught: unknown = null;
    try {
      await api.request("/api/policy", { method: "POST", body: "{}" });
    } catch (err) {
      caught = err;
    }
    // The issue states the throw as {status, error}; the test pins exactly that shape.
    assert.deepEqual(caught, { status: 409, error: "conflict" } as ApiError);
    assert.equal(fake.fetched.length, 1);
  } finally {
    uninstall();
  }
});

// C8, the issue's decided answer: a token this page load received in the address fragment beats an
// older stored token even when sessionStorage refuses the write, and the failed write also clears
// the old stored value so a later load cannot fall back to the previous run's token.
test("a fragment token wins over an old stored token when the storage write fails", async () => {
  const api = await freshApi();
  const fake = install("#token=new-token", 200, {});
  fake.stored.set(api.TOKEN_STORAGE_KEY, "old-token");
  fake.storageRefused = true;
  try {
    api.bootstrapToken();
    // Before the fix getToken answered "old-token" here: it read storage first and only fell back
    // to the in-memory copy when storage held nothing, so the write went out with the dead token.
    assert.equal(api.getToken(), "new-token");
    await api.request("/api/policy", { method: "POST", body: "{}" });
    assert.equal(headersOf(fake)["X-CRW-Token"], "new-token");
    assert.ok(fake.removed.includes(api.TOKEN_STORAGE_KEY), "the failed write also clears the old stored token");
  } finally {
    uninstall();
  }
});

// The precedence rule on its own, with the cleanup failing too: a context that refuses both the
// write and the removal leaves the old token in storage, so a storage-first getToken would still
// answer it. This is the case the first regression could not see, because its fake always deleted
// the old value on removeItem.
test("the fragment token wins even when the old stored token could not be cleared", async () => {
  const api = await freshApi();
  const fake = install("#token=new-token", 200, {});
  fake.stored.set(api.TOKEN_STORAGE_KEY, "old-token");
  fake.storageRefused = true;
  fake.storageRemoveRefused = true;
  try {
    api.bootstrapToken();
    assert.equal(fake.stored.get(api.TOKEN_STORAGE_KEY), "old-token", "the old value really is still in storage");
    assert.equal(api.getToken(), "new-token", "the token this page load received still wins");
    await api.request("/api/policy", { method: "POST", body: "{}" });
    assert.equal(headersOf(fake)["X-CRW-Token"], "new-token");
  } finally {
    uninstall();
  }
});

// The execution-policy client. Its two writes must carry the same guard the rest of the GUI uses -
// the JSON content type and the per-run token - because the server refuses a write without them.
test("the policy writes carry the token and the JSON content type, and the read carries neither", async () => {
  const api = await freshApi();
  const fake = install("", 200, { ok: true });
  try {
    fake.stored.set(api.TOKEN_STORAGE_KEY, "stored-token");
    await api.getPolicy();
    assert.equal(fake.fetched[0].url, "/api/policy");
    assert.equal(headersOf(fake)["X-CRW-Token"], undefined);
    const written = await api.writePolicy({ expectedDigest: "d", change: { kind: "removeException", id: "legacy" } });
    assert.equal(written.status, 200);
    assert.deepEqual(JSON.parse(fake.fetched[1].init?.body as string), { expectedDigest: "d", change: { kind: "removeException", id: "legacy" } });
    assert.equal(headersOf(fake, 1)["X-CRW-Token"], "stored-token");
    assert.equal(headersOf(fake, 1)["Content-Type"], "application/json");
    await api.checkPolicy({ expectedDigest: "d", change: { kind: "removeException", id: "legacy" } });
    assert.equal(fake.fetched[2].url, "/api/policy/check");
  } finally {
    uninstall();
  }
});

test("a failed policy read throws with the server's message rather than a fabricated answer", async () => {
  const api = await freshApi();
  install("", 500, { error: "the policy could not be read" });
  try {
    await assert.rejects(api.getPolicy(), /the policy could not be read/);
  } finally {
    uninstall();
  }
});
