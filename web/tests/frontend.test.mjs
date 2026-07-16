import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  accountRuntimeConfigChanged,
  accountUsesNewAPISettings,
  createNewAccount,
  httpURLs,
  localInputToRFC3339,
  normalizeConfig,
  normalizeStatus,
  saveAccountPayload,
  savedAccountModelsReady,
  validateAccountData,
  validateURL,
} from "../assets/js/domain/config-model.mjs";
import { createCodexOAuth } from "../assets/js/features/codex-oauth.mjs";
import { createAccessFeature } from "../assets/js/features/access.mjs";
import { formatSubscriptionQuota, splitAccountSummaryText } from "../assets/js/features/accounts-view.mjs";
import { formatBalance } from "../assets/js/features/runtime-status.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const jsRoot = resolve(here, "../assets/js");
const htmlPath = resolve(here, "../index.html");
const modules = [
  "main.mjs",
  "core/api-client.mjs",
  "core/app-shell.mjs",
  "domain/config-model.mjs",
  "features/accounts-view.mjs",
  "features/account-runtime.mjs",
  "features/codex-oauth.mjs",
  "features/runtime-status.mjs",
  "features/access.mjs",
];

const startupNoop = () => {};

function createStartupElementStub() {
  const attributes = new Map();
  const listeners = new Map();
  return {
    listeners,
    dataset: {},
    classList: {
      add: startupNoop,
      remove: startupNoop,
      contains: () => false,
      toggle: startupNoop,
    },
    hidden: false,
    disabled: false,
    checked: false,
    value: "",
    textContent: "",
    type: "text",
    addEventListener(name, listener) { listeners.set(name, listener); },
    setAttribute(name, value) { attributes.set(name, String(value)); },
    getAttribute(name) { return attributes.get(name) ?? null; },
    hasAttribute(name) { return attributes.has(name); },
    removeAttribute(name) { attributes.delete(name); },
    querySelectorAll: () => [],
    querySelector: () => null,
    replaceChildren: startupNoop,
    appendChild: startupNoop,
    append: startupNoop,
    focus: startupNoop,
  };
}

let startupSmokeSequence = 0;

async function withStartupEnvironment(fetchImplementation, verify, hidden = false) {
  const globalNames = ["document", "window", "localStorage", "fetch"];
  const originalGlobals = new Map(globalNames.map((name) => [
    name,
    Object.getOwnPropertyDescriptor(globalThis, name),
  ]));
  const setGlobal = (name, value) => Object.defineProperty(globalThis, name, {
    configurable: true,
    writable: true,
    value,
  });
  const elements = new Map();
  const requests = [];

  try {
    setGlobal("document", {
      hidden,
      title: "",
      body: createStartupElementStub(),
      documentElement: createStartupElementStub(),
      getElementById(id) {
        if (!elements.has(id)) elements.set(id, createStartupElementStub());
        return elements.get(id);
      },
      createElement: createStartupElementStub,
      querySelectorAll: () => [],
      addEventListener: startupNoop,
    });
    setGlobal("window", {
      location: { protocol: "http:", hash: "" },
      history: { replaceState: startupNoop },
      addEventListener: startupNoop,
      setInterval: () => 1,
      clearInterval: startupNoop,
      setTimeout: () => 1,
      clearTimeout: startupNoop,
      confirm: () => false,
    });
    setGlobal("localStorage", {
      getItem: () => null,
      setItem: startupNoop,
    });
    setGlobal("fetch", async (path, options) => {
      requests.push([path, options]);
      return fetchImplementation(path, options);
    });

    startupSmokeSequence += 1;
    await import(`../assets/js/main.mjs?startup-smoke=${startupSmokeSequence}`);
    await new Promise(setImmediate);
    await new Promise(setImmediate);
    await verify({ elements, requests });
  } finally {
    for (const [name, descriptor] of originalGlobals) {
      if (descriptor) Object.defineProperty(globalThis, name, descriptor);
      else delete globalThis[name];
    }
  }
}

function jsonResponse(payload) {
  return {
    ok: true,
    status: 200,
    async text() { return JSON.stringify(payload); },
  };
}

test("配置与状态正规化保留安全默认值", () => {
  const config = normalizeConfig({
    strategy: "invalid",
    gatewayTokenCount: 2,
    accounts: [{
      id: 7,
      name: " A ",
      authType: "unknown",
      baseUrl: "https://example.com/v1",
      enabled: true,
      keyConfigured: true,
      quotaResetEvery: -1,
      newApiAuthMode: "bad",
    }],
  });
  assert.equal(config.strategy, "priority");
  assert.equal(config.gatewayTokenCount, 2);
  assert.equal(config.accounts[0].id, "7");
  assert.equal(config.accounts[0].authType, "api_key");
  assert.equal(config.accounts[0].provider, "auto");
  assert.equal(config.accounts[0].newApiAuthMode, "api_key");
  assert.equal(config.accounts[0].quotaResetEvery, 0);
  assert.equal(config.accounts[0]._modelsState, "idle");

  const status = normalizeStatus({ strategy: "round_robin", effectiveStrategy: "bad", availableAccounts: 2 });
  assert.equal(status.strategy, "round_robin");
  assert.equal(status.effectiveStrategy, "round_robin");
  assert.equal(status.availableAccounts, 2);
  assert.deepEqual(status.accounts, []);
});

test("Provider 要求新账号显式选择并兼容旧 auto 账号", () => {
  const draft = createNewAccount();
  draft.name = "Draft";
  draft.baseUrl = "https://example.com/v1";
  draft.apiKey = "key";
  assert.equal(draft.provider, "");
  assert.deepEqual(validateAccountData(draft).map((error) => error.field), ["provider"]);
  draft.provider = "auto";
  assert.deepEqual(validateAccountData(draft).map((error) => error.field), ["provider"]);
  draft.provider = "new_api";
  assert.deepEqual(validateAccountData(draft), []);

  const legacy = normalizeConfig({
    accounts: [{
      id: "legacy",
      name: "Legacy",
      authType: "api_key",
      baseUrl: "https://legacy.example/v1",
      enabled: true,
      keyConfigured: true,
      newApiAuthMode: "password",
      newApiUsername: "user",
      newApiSecretConfigured: true,
    }],
  }).accounts[0];
  assert.equal(legacy.provider, "auto");
  assert.equal(accountUsesNewAPISettings(legacy), true);
  assert.equal(saveAccountPayload(legacy).provider, "auto");
  legacy.apiKey = "replacement";
  assert.deepEqual(validateAccountData(legacy).map((error) => error.field), ["provider"]);
  legacy.apiKey = "";

  const compatible = normalizeConfig({
    accounts: [{
      id: "compatible",
      name: "Compatible",
      authType: "api_key",
      provider: "openai_responses",
      baseUrl: "https://compatible.example/v1",
      enabled: true,
      keyConfigured: true,
      newApiAuthMode: "password",
      newApiUsername: "must-not-send",
      newApiUserId: 7,
      newApiSecretConfigured: true,
      quotaResetPeriod: "monthly",
    }],
  }).accounts[0];
  assert.equal(accountUsesNewAPISettings(compatible), false);
  assert.equal(compatible.newApiAuthMode, "api_key");
  assert.equal(compatible.newApiUsername, "");
  assert.equal(compatible.newApiUserId, 0);
  assert.equal(compatible.newApiSecretConfigured, false);
  const compatiblePayload = saveAccountPayload(compatible);
  assert.equal(compatiblePayload.newApiUsername, "");
  assert.equal(compatiblePayload.newApiUserId, 0);
  assert.equal(compatiblePayload.newApiSecret, "");
  assert.equal(compatiblePayload.quotaResetPeriod, "never");
  assert.equal(accountRuntimeConfigChanged(compatible), false);
  assert.equal(savedAccountModelsReady(compatible), true);
  compatible.provider = "sub2api";
  assert.equal(accountRuntimeConfigChanged(compatible), true);
  assert.equal(savedAccountModelsReady(compatible), false);

  const oauth = normalizeConfig({
    accounts: [{
      id: "oauth",
      name: "OAuth",
      authType: "codex_oauth",
      provider: "new_api",
      disableCodexCredits: true,
    }],
  }).accounts[0];
  assert.equal(oauth.provider, "codex_oauth");
  assert.equal(oauth.disableCodexCredits, true);
  assert.equal(saveAccountPayload(oauth).provider, "codex_oauth");
  assert.equal(saveAccountPayload(oauth).disableCodexCredits, true);
  assert.equal(accountRuntimeConfigChanged(oauth), false);
  oauth.disableCodexCredits = false;
  assert.equal(accountRuntimeConfigChanged(oauth), true);
});

test("URL、账号验证和明文 HTTP 去重保持原契约", () => {
  assert.equal(validateURL("https://example.com/v1"), "");
  assert.match(validateURL("ftp://example.com"), /http/);
  assert.match(validateURL("https://user@example.com/v1"), /用户信息/);

  const account = createNewAccount();
  const fields = validateAccountData(account).map((error) => error.field);
  assert.deepEqual(fields, ["name", "provider", "baseUrl", "apiKey"]);

  account.authType = "codex_oauth";
  assert.deepEqual(validateAccountData(account).map((error) => error.field), ["name"]);
  account.name = "OAuth";
  assert.deepEqual(validateAccountData(account), []);
  assert.deepEqual(httpURLs([
    { authType: "api_key", baseUrl: "http://one.example/v1" },
    { authType: "api_key", baseUrl: "http://one.example/v1" },
    { authType: "api_key", baseUrl: "https://two.example/v1" },
    { authType: "codex_oauth", baseUrl: "http://ignored.example" },
  ]), ["http://one.example/v1"]);
});

test("保存 payload 与运行配置变更判断不泄露无关字段", () => {
  const account = createNewAccount();
  account.id = "account-1";
  account._uiId = "account-1";
  account.name = " Primary ";
  account.provider = "new_api";
  account._savedProvider = "new_api";
  account.baseUrl = "https://example.com/v1 ";
  account._savedBaseUrl = "https://example.com/v1";
  account.keyConfigured = true;
  account.quotaResetPeriod = "custom";
  account.quotaResetEvery = 1;
  account.quotaResetUnit = "day";
  account.quotaResetAnchorAt = "2026-07-14T10:00";
  account.quotaResetTimezone = "Asia/Shanghai";
  account.newApiAuthMode = "password";
  account.newApiUsername = " user ";
  account.newApiSecret = "secret";

  assert.equal(accountRuntimeConfigChanged(account), true);
  const payload = saveAccountPayload(account);
  assert.equal(payload.name, "Primary");
  assert.equal(payload.provider, "new_api");
  assert.equal(payload.baseUrl, "https://example.com/v1");
  assert.equal(payload.newApiUsername, "user");
  assert.equal(payload.newApiSecret, "secret");
  assert.equal(payload.quotaResetPeriod, "never");
  assert.equal(payload.quotaResetEvery, 0);
  assert.equal(payload.quotaResetUnit, "");
  assert.equal(payload.quotaResetAnchorAt, "");
  assert.equal(payload.quotaResetTimezone, "");
  assert.equal(localInputToRFC3339("invalid"), "");

  const manual = createNewAccount();
  manual.name = "Manual";
  manual.provider = "new_api";
  manual.baseUrl = "https://manual.example/v1";
  manual.apiKey = "key";
  manual.quotaResetPeriod = "custom";
  manual.quotaResetEvery = 3;
  manual.quotaResetUnit = "month";
  manual.quotaResetAnchorAt = "2026-07-14T10:00";
  manual.quotaResetTimezone = "Asia/Shanghai";
  const manualPayload = saveAccountPayload(manual);
  assert.equal(manualPayload.quotaResetPeriod, "custom");
  assert.equal(manualPayload.quotaResetEvery, 3);
  assert.equal(manualPayload.quotaResetUnit, "month");
  assert.equal(manualPayload.quotaResetTimezone, "Asia/Shanghai");
  assert.match(manualPayload.quotaResetAnchorAt, /^\d{4}-\d{2}-\d{2}T/);

  account.authType = "codex_oauth";
  account.disableCodexCredits = true;
  const oauthPayload = saveAccountPayload(account);
  assert.equal(oauthPayload.baseUrl, "");
  assert.equal(oauthPayload.provider, "codex_oauth");
  assert.equal(oauthPayload.apiKey, "");
  assert.equal(oauthPayload.newApiSecret, "");
  assert.equal(oauthPayload.disableCodexCredits, true);
  assert.equal(oauthPayload.quotaResetEvery, 0);
});

test("手工重置规则兼容旧配置并按登录方式切换", () => {
  const normalized = normalizeConfig({
    accounts: [{
      id: "legacy",
      name: "Legacy",
      authType: "api_key",
      baseUrl: "https://example.com/v1",
      enabled: true,
      keyConfigured: true,
      quotaResetEvery: 12,
      quotaResetUnit: "month",
      quotaResetAnchorAt: "2026-07-14T02:00:00Z",
      newApiAuthMode: "api_key",
    }, {
      id: "login",
      name: "Login",
      authType: "api_key",
      baseUrl: "https://example.com/v1",
      enabled: true,
      keyConfigured: true,
      quotaResetPeriod: "monthly",
      quotaResetTimezone: "Asia/Shanghai",
      newApiAuthMode: "access_token",
      newApiUserId: 7,
      newApiSecretConfigured: true,
    }],
  });
  assert.equal(normalized.accounts[0].quotaResetPeriod, "custom");
  assert.equal(normalized.accounts[0].quotaResetUnit, "month");
  assert.equal(normalized.accounts[0].quotaResetTimezone, "Asia/Shanghai");
  assert.equal(normalized.accounts[1].quotaResetPeriod, "never");
  assert.equal(normalized.accounts[1].quotaResetEvery, 0);
  assert.equal(accountRuntimeConfigChanged(normalized.accounts[1]), false);

  const account = createNewAccount();
  account.name = "Manual";
  account.provider = "new_api";
  account.baseUrl = "https://example.com/v1";
  account.apiKey = "key";
  account.quotaResetPeriod = "daily";
  account.quotaResetTimezone = "";
  assert.equal(validateAccountData(account).some((error) => error.field === "quotaResetTimezone"), true);
  account.quotaResetTimezone = "Asia/Shanghai";
  assert.equal(validateAccountData(account).some((error) => error.field.startsWith("quotaReset")), false);
});

test("新 OAuth 账号登录会先保存并使用服务端分配的账号 ID", async () => {
  const draft = createNewAccount();
  draft.name = "OAuth";
  draft.authType = "codex_oauth";
  const saved = createNewAccount();
  saved.id = "saved-account";
  saved._uiId = saved.id;
  saved.name = draft.name;
  saved.authType = "codex_oauth";
  saved._savedAuthType = "codex_oauth";

  const calls = [];
  let current = draft;
  const originalWindow = globalThis.window;
  globalThis.window = { setTimeout: () => 0 };
  try {
    const codex = createCodexOAuth({
      api: {
        async request(path, options) {
          calls.push(["request", path, JSON.parse(options.body)]);
          return {
            sessionId: "session-1",
            authorizationUrl: "https://auth.openai.com/oauth/authorize?state=test",
            expiresAt: new Date(Date.now() + 60_000).toISOString(),
            pollIntervalSeconds: 5,
          };
        },
      },
      shell: {
        clearGlobalError() {},
        isDirty: () => true,
        isReady: () => true,
        announce(message) { calls.push(["announce", message]); },
      },
      getAccount(id) {
        return String(id) === String(current._uiId || current.id) ? current : null;
      },
      getStatusAccount: () => null,
      async ensureAccountSaved(account) {
        calls.push(["save", account]);
        current = saved;
        return saved;
      },
      setStatus() {},
      renderStatus() {},
      renderAccount() {},
    });

    await codex.startBrowserAuth(draft);
    assert.equal(calls[0][0], "save");
    assert.deepEqual(calls[1], [
      "request",
      "/admin/accounts/codex/oauth/start",
      { accountId: "saved-account" },
    ]);
    assert.equal(saved._codexAuthStarting, false);
    assert.equal(saved._codexOAuthSession.sessionId, "session-1");
    assert.match(saved._codexOAuthSession.authorizationUrl, /^https:\/\/auth\.openai\.com\/oauth\/authorize/);
  } finally {
    if (originalWindow === undefined) delete globalThis.window;
    else globalThis.window = originalWindow;
  }
});

test("余额展示不把未知值当作零", () => {
  assert.match(formatBalance(null), /未知/);
  assert.match(formatBalance({ status: "auth_error" }), /认证失败/);
  assert.equal(formatBalance({ status: "known", remaining: 12.5, unit: "USD" }), "12.50 USD");
  const keyOnly = formatBalance({ status: "known", remaining: 12.5, unit: "USD", scope: "token_only" });
  assert.match(keyOnly, /仅 API Key 额度/);
  assert.match(keyOnly, /未登录 New API 账户/);
  assert.match(keyOnly, /显示值可能不准确/);
  assert.match(keyOnly, /余额策略可能无法按预期生效/);
  assert.match(formatBalance({ status: "known", unlimited: true, scope: "token_only" }), /API Key 无限.*未登录 New API 账户/);
  const verified = formatBalance({ status: "known", remaining: 12.5, unit: "USD", scope: "actual" });
  assert.doesNotMatch(verified, /未登录|可能不准确|余额策略/);
});

test("订阅额度只在存在时格式化展示", () => {
  assert.equal(formatSubscriptionQuota(null), "");
  const formatted = formatSubscriptionQuota({
    total: 200,
    remaining: 0.049754,
    unit: "USD",
    resetAt: "2026-07-14T16:00:00Z",
  });
  assert.match(formatted, /订阅额度：剩余 0\.049754 \/ 200\.00 USD/);
  assert.match(formatted, /重置/);
});

test("账号摘要将主值和括号说明分层", () => {
  assert.deepEqual(
    splitAccountSummaryText("100.00 USD（仅 API Key 额度；显示值可能不准确）（服务器已保存配置）"),
    { value: "100.00 USD", note: "仅 API Key 额度；显示值可能不准确；服务器已保存配置" },
  );
  assert.deepEqual(splitAccountSummaryText("余额获取失败"), { value: "余额获取失败", note: "" });
});

test("访问功能工厂可以完成实例化且只公开已定义函数", () => {
  const originalDocument = globalThis.document;
  globalThis.document = { getElementById: () => null };
  try {
    const feature = createAccessFeature({
      api: {},
      shell: {},
      getConfig: () => ({}),
      setConfig() {},
      showView() {},
    });
    assert.deepEqual(Object.keys(feature).sort(), [
      "init",
      "loadGatewayTokens",
      "renderGatewayTokens",
      "resetPagination",
      "securityPayload",
      "syncControls",
      "syncSecurityFields",
      "validateSecuritySettings",
    ]);
    for (const [name, value] of Object.entries(feature)) {
      assert.equal(typeof value, "function", `${name} 不是函数`);
    }
  } finally {
    if (originalDocument === undefined) delete globalThis.document;
    else globalThis.document = originalDocument;
  }
});

test("Token 轮换操作按目标 ID 调用后端并复制新配置", async () => {
  const originalDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
  const originalWindow = Object.getOwnPropertyDescriptor(globalThis, "window");
  const elements = new Map();
  const calls = [];
  const copied = [];
  const announcements = [];
  let config = { allowPublicAccess: false };

  try {
    Object.defineProperty(globalThis, "document", {
      configurable: true,
      writable: true,
      value: {
        getElementById(id) {
          if (!elements.has(id)) elements.set(id, createStartupElementStub());
          return elements.get(id);
        },
        createElement: createStartupElementStub,
      },
    });
    Object.defineProperty(globalThis, "window", {
      configurable: true,
      writable: true,
      value: {
        confirm: () => true,
        clearTimeout: startupNoop,
        setTimeout: () => 1,
      },
    });
    const feature = createAccessFeature({
      api: {
        async request(path, options) {
          calls.push([path, options]);
          if (path === "/admin/config") {
            return { config, codexSnippet: "rotated snippet" };
          }
          if (String(path).startsWith("/admin/gateway-tokens?")) {
            return { tokens: [], total: 0 };
          }
          throw new Error(`unexpected request: ${path}`);
        },
      },
      shell: {
        isReady: () => true,
        isBusy: () => false,
        setButtonBusy: () => startupNoop,
        copyText: async (value) => copied.push(value),
        announce: (value) => announcements.push(value),
        showGlobalError(error) { throw new Error(error); },
        markDirty: startupNoop,
      },
      getConfig: () => config,
      setConfig(value) { config = value; },
      showView: startupNoop,
    });
    feature.init();
    const click = elements.get("gatewayTokenList").listeners.get("click");
    click({
      target: {
        closest: () => ({ dataset: { tokenAction: "rotate", tokenId: "tok_target" } }),
      },
    });
    await new Promise(setImmediate);
    await new Promise(setImmediate);

    assert.equal(calls[0][0], "/admin/config");
    assert.equal(calls[0][1].method, "PUT");
    assert.deepEqual(JSON.parse(calls[0][1].body), { rotateGatewayTokenId: "tok_target" });
    assert.match(calls[1][0], /^\/admin\/gateway-tokens\?/);
    assert.deepEqual(copied, ["rotated snippet"]);
    assert.deepEqual(announcements, ["Token 已轮换，新本机配置已复制"]);
  } finally {
    if (originalDocument) Object.defineProperty(globalThis, "document", originalDocument);
    else delete globalThis.document;
    if (originalWindow) Object.defineProperty(globalThis, "window", originalWindow);
    else delete globalThis.window;
  }
});

test("页面入口能执行到 bootstrap 请求", async () => {
  await withStartupEnvironment(
    async () => { throw new Error("expected offline"); },
    ({ elements, requests }) => {
      assert.deepEqual(requests.map(([path]) => path), ["/admin/bootstrap"]);
      assert.equal(elements.get("serviceState").dataset.state, "offline");
    },
  );
});

test("页面入口成功加载 bootstrap 与网关 Token", async () => {
  await withStartupEnvironment(
    async (path) => {
      if (path === "/admin/bootstrap") {
        return jsonResponse({
          csrfToken: "csrf",
          version: "0.3.5",
          config: { strategy: "priority", accounts: [], gatewayTokenCount: 0 },
          status: { strategy: "priority", accounts: [] },
        });
      }
      if (String(path).startsWith("/admin/gateway-tokens?")) {
        return jsonResponse({ tokens: [], total: 0 });
      }
      throw new Error(`unexpected request: ${path}`);
    },
    ({ elements, requests }) => {
      assert.deepEqual(requests.map(([path]) => String(path).split("?")[0]), [
        "/admin/bootstrap",
        "/admin/gateway-tokens",
      ]);
      assert.equal(elements.get("serviceState").dataset.state, "online");
      assert.equal(elements.get("serviceState").textContent, "服务在线");
      assert.equal(elements.get("appVersion").textContent, " · v0.3.5");
    },
    true,
  );
});

test("九个原生模块存在、依赖无环且保留业务与 DOM 契约", async () => {
  const sources = new Map();
  for (const name of modules) {
    sources.set(name, await readFile(resolve(jsRoot, name), "utf8"));
  }

  const graph = new Map(modules.map((name) => [name, []]));
  for (const [name, source] of sources) {
    for (const match of source.matchAll(/from\s+["'](\.[^"']+)["']/g)) {
      const dependency = resolve(dirname(resolve(jsRoot, name)), match[1]).slice(jsRoot.length + 1).replaceAll("\\", "/");
      if (graph.has(dependency)) graph.get(name).push(dependency);
    }
  }
  const visiting = new Set();
  const visited = new Set();
  function visit(name) {
    assert.equal(visiting.has(name), false, `模块依赖出现循环：${name}`);
    if (visited.has(name)) return;
    visiting.add(name);
    graph.get(name).forEach(visit);
    visiting.delete(name);
    visited.add(name);
  }
  modules.forEach(visit);

  const all = Array.from(sources.values()).join("\n");
  const mainSource = sources.get("main.mjs");
  const oauthSource = sources.get("features/codex-oauth.mjs");
  const accountsViewSource = sources.get("features/accounts-view.mjs");
  const accountRuntimeSource = sources.get("features/account-runtime.mjs");
  [
    "/admin/bootstrap",
    "/admin/config",
    "/admin/status",
    "/admin/models",
    "/admin/balances/test",
    "/admin/balances/refresh",
    "/admin/accounts/codex/oauth/start",
    "/admin/accounts/codex/oauth/status",
    "/admin/accounts/codex/usage",
    "/admin/accounts/codex/logout",
    "/admin/accounts/statistics/clear",
    "/admin/gateway-tokens/snippet",
  ].forEach((path) => assert.match(all, new RegExp(path.replaceAll("/", "\\/"))));
  [
    "available", "recent_success", "recent_failure", "recovery_pending",
    "formatRequestHealth", "balancePollInterval = 60000", "?automatic=1",
    "data-view-target", "data-theme-choice", "sidebarCollapseButton", "codex-copy-link",
    "data-account-auth", "data-auth-panel", "data-api-action", "startBrowserAuth", "copyAuthorizationLink",
    "clearRequestStatistics", "nextProbeAt", "最早受控验证",
    "rotateGatewayTokenId", '["rotate", "轮换", "secondary"]',
    "provider", "accountUsesNewAPISettings", "quotaResetPeriod", "quotaResetTimezone", "priorityResetAt", "billingPreference",
  ].forEach((contract) => assert.equal(all.includes(contract), true, `缺少契约：${contract}`));
  assert.match(accountRuntimeSource, /provider:\s*account\.provider/);
  assert.match(accountsViewSource, /\["authType", "provider", "baseUrl"[^\n]+\n\s+invalidateAccountModels/);
  assert.match(mainSource, /ensureAccountSaved:\s*saveAccountForOAuth/);
  assert.match(mainSource, /ignorePendingOAuthAccount/);
  assert.match(mainSource, /refreshBalances:\s*false/);
  assert.match(oauthSource, /_codexAuthSaving/);
  assert.match(oauthSource, /await ensureAccountSaved\(account\)/);
  assert.doesNotMatch(oauthSource, /loginButton\.disabled\s*=\s*!savedReady/);
  assert.doesNotMatch(oauthSource, /window\.open\("about:blank"/);
  assert.equal(all.includes("/admin/accounts/deviceauth/"), false);
  assert.equal(all.includes("testModel"), false);
  assert.equal(/console\.(log|info|warn|error|debug)/.test(all), false);
});

test("HTML 入口保持外置资源、唯一 ID 与模块 DOM 契约", async () => {
  const html = await readFile(htmlPath, "utf8");
  const sources = await Promise.all(modules.map((name) => readFile(resolve(jsRoot, name), "utf8")));
  const allJavaScript = sources.join("\n");
  const ids = Array.from(html.matchAll(/\sid="([^"]+)"/g), (match) => match[1]);
  const uniqueIds = new Set(ids);

  assert.equal(uniqueIds.size, ids.length, "HTML 存在重复 ID");
  assert.equal((html.match(/data-action="codex-usage"/g) || []).length, 1, "刷新用量按钮必须唯一");
  assert.ok(html.indexOf('data-action="codex-usage"') < html.indexOf('<dialog class="account-dialog"'),
    "刷新用量按钮应位于账号卡片面板而不是编辑弹窗");
  assert.match(html, /<h4>套餐额度<\/h4>/);
  assert.match(html, /<h4>周额度<\/h4>/);
  for (const match of allJavaScript.matchAll(/getElementById\("([^"]+)"\)/g)) {
    assert.equal(uniqueIds.has(match[1]), true, `缺少模块引用的 DOM ID：${match[1]}`);
  }

  const views = new Set(Array.from(html.matchAll(/data-view="([^"]+)"/g), (match) => match[1]));
  for (const match of html.matchAll(/data-view-target="([^"]+)"/g)) {
    assert.equal(views.has(match[1]), true, `导航目标没有对应视图：${match[1]}`);
  }

  assert.doesNotMatch(html, /<style\b/i);
  assert.doesNotMatch(html, /\sstyle\s*=/i);
  const scripts = Array.from(html.matchAll(/<script\b([^>]*)>/gi), (match) => match[1]);
  assert.equal(scripts.length, 1);
  assert.match(scripts[0], /type="module"/);
  assert.match(scripts[0], /src="\/assets\/js\/main\.mjs"/);
  assert.equal((html.match(/<link\b[^>]*rel="stylesheet"/gi) || []).length, 5);
  assert.match(html, /id="sidebarCollapseButton"[^>]*aria-label="收起侧栏"[^>]*aria-expanded="true"/);
  assert.equal((html.match(/data-theme-choice=/g) || []).length, 3);
  assert.equal((html.match(/data-theme-choice=[^>]*aria-pressed=/g) || []).length, 3);
  assert.match(html, /data-action="codex-copy-link"/);
  assert.match(html, /点击登录会先自动保存账号配置并生成授权链接/);
  assert.equal(html.indexOf('data-field="provider"') < html.indexOf('data-field="baseUrl"'), true,
    "应先选择上游平台，再填写平台地址");
  ["new_api", "sub2api", "openai_responses"].forEach((provider) => {
    assert.match(html, new RegExp(`<option value="${provider}">`));
  });
  assert.match(html, /<option value="auto"[^>]*data-provider-option="legacy-auto"[^>]*disabled[^>]*hidden>/);
  assert.match(html, /<option value="codex_oauth"[^>]*data-provider-option="codex-oauth"[^>]*disabled[^>]*hidden>/);
  assert.equal((html.match(/data-new-api-settings/g) || []).length, 2);
  assert.equal(html.indexOf('data-field="newApiAuthMode"') < html.indexOf('data-field="quotaResetPeriod"'), true,
    "应先选择套餐余额读取方式，再展示手工重置规则");
  ["never", "daily", "weekly", "monthly", "custom"].forEach((period) => {
    assert.match(html, new RegExp(`<option value="${period}">`));
  });
  assert.match(html, /<option value="month">月<\/option>/);
});
