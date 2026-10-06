// Original CRW test (no CXC counterpart): the api.ts foundation this issue writes new -
// the token moves out of the address, the token and JSON headers are attached to writes
// only, and a non-2xx response throws {status, error}.
import { test } from "node:test";
import assert from "node:assert/strict";
import { TOKEN_STORAGE_KEY, bootstrapToken, getToken, request } from "../src/api.ts";

interface FakeLocation {
  hash: string;
  pathname: string;
  search: string;
}

interface Fake {
  location: FakeLocation;
  replaced: string[];
  stored: Map<string, string>;
  fetched: Array<{ url: string; init?: RequestInit }>;
}

const GLOBALS = globalThis as unknown as Record<string, unknown>;

function install(hash: string, status: number, payload: unknown): Fake {
  const replaced: string[] = [];
  const stored = new Map<string, string>();
  const fetched: Array<{ url: string; init?: RequestInit }> = [];
  const location: FakeLocation = { hash, pathname: "/", search: "" };
  GLOBALS.location = location;
  GLOBALS.history = {
    replaceState: (_state: unknown, _title: string, url: string) => {
      replaced.push(url);
    },
  };
  GLOBALS.sessionStorage = {
    getItem: (key: string) => (stored.has(key) ? stored.get(key) : null),
    setItem: (key: string, value: string) => {
      stored.set(key, value);
    },
  };
  GLOBALS.fetch = async (url: string, init?: RequestInit) => {
    fetched.push({ url, init });
    return new Response(JSON.stringify(payload), { status });
  };
  return { location, replaced, stored, fetched };
}

function uninstall(): void {
  delete GLOBALS.location;
  delete GLOBALS.history;
  delete GLOBALS.sessionStorage;
  delete GLOBALS.fetch;
}

function headersOf(fake: Fake): Record<string, string> {
  return (fake.fetched[0]?.init?.headers ?? {}) as Record<string, string>;
}

test("the fragment token is stored and stripped from the address", () => {
  const fake = install("#token=s3cret-token", 200, {});
  try {
    bootstrapToken();
    assert.equal(fake.stored.get(TOKEN_STORAGE_KEY), "s3cret-token");
    assert.equal(fake.replaced.length, 1);
    assert.ok(!fake.replaced[0].includes("s3cret-token"));
  } finally {
    uninstall();
  }
});

test("the token is stored byte for byte and other fragment parameters survive", () => {
  const fake = install("#token=a+b%2Fc&view=1", 200, {});
  try {
    bootstrapToken();
    // URLSearchParams would have decoded the plus to a space and the %2F to a slash;
    // the token's alphabet belongs to the server, so the client must not alter it.
    assert.equal(fake.stored.get(TOKEN_STORAGE_KEY), "a+b%2Fc");
    assert.equal(fake.replaced.length, 1);
    assert.equal(fake.replaced[0], "/#view=1");
  } finally {
    uninstall();
  }
});

test("a hash without a token is left alone and bootstrapToken is idempotent", () => {
  const fake = install("#/policy", 200, {});
  try {
    bootstrapToken();
    bootstrapToken();
    assert.equal(fake.replaced.length, 0);
    assert.equal(fake.location.hash, "#/policy");
    assert.equal(getToken(), null);
  } finally {
    uninstall();
  }
});

test("a read request carries neither the token nor a JSON content type", async () => {
  const fake = install("", 200, { ok: true });
  try {
    fake.stored.set(TOKEN_STORAGE_KEY, "stored-token");
    const body = await request<{ ok: boolean }>("/api/status");
    assert.deepEqual(body, { ok: true });
    assert.equal(fake.fetched.length, 1);
    assert.equal(headersOf(fake)["X-CRW-Token"], undefined);
    assert.equal(headersOf(fake)["Content-Type"], undefined);
  } finally {
    uninstall();
  }
});

test("a write request carries the token and the JSON content type", async () => {
  const fake = install("", 200, { saved: true });
  try {
    fake.stored.set(TOKEN_STORAGE_KEY, "stored-token");
    await request("/api/policy", { method: "POST", body: JSON.stringify({ a: 1 }) });
    assert.equal(fake.fetched.length, 1);
    assert.equal(headersOf(fake)["X-CRW-Token"], "stored-token");
    assert.equal(headersOf(fake)["Content-Type"], "application/json");
  } finally {
    uninstall();
  }
});

test("a non-2xx response throws the status and the server error", async () => {
  const fake = install("", 409, { error: "conflict" });
  try {
    let caught: unknown = null;
    try {
      await request("/api/policy", { method: "POST", body: "{}" });
    } catch (err) {
      caught = err;
    }
    // The issue states the throw as {status, error}; the test pins exactly that shape.
    assert.deepEqual(caught, { status: 409, error: "conflict" });
    assert.equal(fake.fetched.length, 1);
  } finally {
    uninstall();
  }
});
