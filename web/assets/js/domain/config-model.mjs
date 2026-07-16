export const VALID_STRATEGIES = Object.freeze({
  priority: true,
  round_robin: true,
  least_used: true,
  quota_reset: true,
  highest_balance: true,
  lowest_balance: true,
});

export const NEW_API_AUTH_MODES = Object.freeze({
  api_key: true,
  password: true,
  access_token: true,
});

export const ACCOUNT_AUTH_TYPES = Object.freeze({
  api_key: true,
  codex_oauth: true,
});

export const API_KEY_PROVIDERS = Object.freeze({
  auto: true,
  new_api: true,
  sub2api: true,
  openai_responses: true,
});

export const QUOTA_RESET_PERIODS = Object.freeze({
  never: true,
  daily: true,
  weekly: true,
  monthly: true,
  custom: true,
});

export const QUOTA_RESET_UNITS = Object.freeze({
  hour: true,
  day: true,
  week: true,
  month: true,
});

function quotaResetUsesTimezone(period, unit) {
  return period === "daily" || period === "weekly" || period === "monthly" ||
    (period === "custom" && unit === "month");
}

export function createEmptyConfig() {
  return {
    strategy: "priority",
    allowInsecureHttp: false,
    allowPublicAccess: false,
    publicBaseUrl: "",
    allowPublicAdmin: false,
    adminPasswordConfigured: false,
    gatewayTokenCount: 0,
    accounts: [],
  };
}

export function createEmptyStatus() {
  return {
    strategy: "priority",
    effectiveStrategy: "priority",
    fallbackReason: "",
    lastRoutedAccountId: "",
    lastRoutedAccountName: "",
    availableAccounts: 0,
    totalAccounts: 0,
    accounts: [],
  };
}

export function numberOrZero(value) {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

export function rfc3339ToLocalInput(value) {
  if (typeof value !== "string" || !value.trim()) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime()) || date.getUTCFullYear() <= 1) {
    return "";
  }
  const pad = (number) => String(number).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function localInputToRFC3339(value) {
  if (typeof value !== "string" || !value.trim()) {
    return "";
  }
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? "" : date.toISOString();
}

export function newAccountId() {
  const crypto = globalThis.crypto;
  if (crypto && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  if (crypto && typeof crypto.getRandomValues === "function") {
    const bytes = new Uint32Array(4);
    crypto.getRandomValues(bytes);
    return `account-${Array.from(bytes, (item) => item.toString(16).padStart(8, "0")).join("")}`;
  }
  return `account-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

export function accountUIID(account) {
  return String(account._uiId || account.id);
}

export function validDateValue(value) {
  if (!value) {
    return false;
  }
  const date = new Date(value);
  return !Number.isNaN(date.getTime()) && date.getUTCFullYear() > 1;
}

export function formatDateTime(value, fallback) {
  if (!value) {
    return fallback;
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return String(value);
  }
  return new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  }).format(date);
}

export function formatOptionalDateTime(value, fallback) {
  return validDateValue(value) ? formatDateTime(value, fallback) : fallback;
}

export function formatInteger(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return "0";
  }
  return new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 0 }).format(value);
}

export function boundedPercent(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return 0;
  }
  return Math.max(0, Math.min(100, value));
}

export function formatPercent(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return "—";
  }
  return `${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(Math.max(0, value))}%`;
}

export function formatUsageDuration(seconds) {
  if (typeof seconds !== "number" || !Number.isFinite(seconds) || seconds <= 0) {
    return "";
  }
  const rounded = Math.round(seconds);
  if (rounded % 604800 === 0) return `${rounded / 604800} 周`;
  if (rounded % 86400 === 0) return `${rounded / 86400} 天`;
  if (rounded % 3600 === 0) return `${rounded / 3600} 小时`;
  if (rounded % 60 === 0) return `${rounded / 60} 分钟`;
  return `${rounded} 秒`;
}

export function validateURL(value) {
  let parsed;
  try {
    parsed = new URL(value);
  } catch (_error) {
    return "请输入完整的 http:// 或 https:// 地址";
  }
  if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || !parsed.host) {
    return "地址必须使用 http:// 或 https://";
  }
  if (parsed.username || parsed.password || parsed.search || parsed.hash) {
    return "地址不能包含用户信息、查询参数或片段";
  }
  return "";
}

export function newAPIAuthIdentifierChanged(account) {
  if (account.newApiAuthMode !== account._savedNewApiAuthMode) {
    return true;
  }
  if (account.newApiAuthMode === "password") {
    return account.newApiUsername.trim() !== account._savedNewApiUsername;
  }
  if (account.newApiAuthMode === "access_token") {
    return account.newApiUserId !== account._savedNewApiUserId;
  }
  return false;
}

function accountHasValidProvider(account) {
  return Boolean(API_KEY_PROVIDERS[account.provider]) &&
    (account.provider !== "auto" || (Boolean(account.id) && account.apiKey.trim() === ""));
}

export function accountUsesNewAPISettings(account) {
  return account.authType === "api_key" &&
    (account.provider === "new_api" || (account.provider === "auto" && Boolean(account.id)));
}

export function accountHasUsableModelConfig(account) {
  return account.authType === "api_key" && accountHasValidProvider(account) &&
    !validateURL(account.baseUrl.trim()) &&
    (account.apiKey.trim() !== "" || (account.keyConfigured && !account.clearApiKey));
}

export function savedAccountModelsReady(account) {
  return Boolean(account.id) && accountHasUsableModelConfig(account) &&
    account.provider === account._savedProvider &&
    account.baseUrl.trim() === account._savedBaseUrl &&
    account.apiKey.trim() === "" && !account.clearApiKey;
}

export function accountRuntimeConfigChanged(account) {
  if (!account.id || account.authType !== account._savedAuthType || account.enabled !== account._savedEnabled) {
    return true;
  }
  if (account.authType === "codex_oauth") {
    return account.disableCodexCredits !== account._savedDisableCodexCredits;
  }
  const newAPISettings = accountUsesNewAPISettings(account);
  const manualReset = newAPISettings && account.newApiAuthMode === "api_key";
  const resetPeriod = manualReset && QUOTA_RESET_PERIODS[account.quotaResetPeriod]
    ? account.quotaResetPeriod
    : "never";
  const customReset = resetPeriod === "custom";
  const resetEvery = customReset && Number.isSafeInteger(account.quotaResetEvery) ? account.quotaResetEvery : 0;
  const resetUnit = customReset ? account.quotaResetUnit : "";
  const resetAnchor = customReset ? account.quotaResetAnchorAt : "";
  const resetTimezone = manualReset && quotaResetUsesTimezone(resetPeriod, resetUnit)
    ? account.quotaResetTimezone.trim()
    : "";
  return account.provider !== account._savedProvider ||
    account.baseUrl.trim() !== account._savedBaseUrl ||
    account.apiKey.trim() !== "" ||
    account.clearApiKey ||
    resetPeriod !== account._savedQuotaResetPeriod ||
    resetEvery !== account._savedQuotaResetEvery ||
    resetUnit !== account._savedQuotaResetUnit ||
    resetAnchor !== account._savedQuotaResetAnchorAt ||
    resetTimezone !== account._savedQuotaResetTimezone ||
    (newAPISettings && (
      account.newApiAuthMode !== account._savedNewApiAuthMode ||
      account.newApiUsername.trim() !== account._savedNewApiUsername ||
      account.newApiUserId !== account._savedNewApiUserId ||
      account.newApiSecret.trim() !== "" ||
      account.clearNewApiSecret
    ));
}

export function createNewAccount() {
  return {
    id: "",
    _uiId: newAccountId(),
    name: "",
    authType: "api_key",
    _savedAuthType: "api_key",
    provider: "",
    _savedProvider: "",
    baseUrl: "",
    _savedBaseUrl: "",
    enabled: true,
    _savedEnabled: true,
    keyConfigured: false,
    apiKey: "",
    clearApiKey: false,
    codexAuthenticated: false,
    codexEmail: "",
    codexPlanType: "",
    codexExpiresAt: "",
    disableCodexCredits: false,
    _savedDisableCodexCredits: false,
    quotaResetPeriod: "never",
    _savedQuotaResetPeriod: "never",
    quotaResetEvery: 0,
    _savedQuotaResetEvery: 0,
    quotaResetUnit: "day",
    _savedQuotaResetUnit: "",
    quotaResetAnchorAt: "",
    _savedQuotaResetAnchorAt: "",
    _savedQuotaResetAnchorRFC3339: "",
    quotaResetTimezone: "Asia/Shanghai",
    _savedQuotaResetTimezone: "",
    newApiAuthMode: "api_key",
    _savedNewApiAuthMode: "api_key",
    newApiUsername: "",
    _savedNewApiUsername: "",
    newApiUserId: 0,
    _savedNewApiUserId: 0,
    newApiSecretConfigured: false,
    newApiSecret: "",
    clearNewApiSecret: false,
    _models: [],
    _modelsState: "incomplete",
    _modelsError: "",
    _modelsRequestId: 0,
  };
}

export function normalizeConfig(value) {
  const source = value && typeof value === "object" ? value : {};
  const accounts = Array.isArray(source.accounts) ? source.accounts : [];
  return {
    strategy: VALID_STRATEGIES[source.strategy] ? source.strategy : "priority",
    allowInsecureHttp: source.allowInsecureHttp === true,
    allowPublicAccess: source.allowPublicAccess === true,
    publicBaseUrl: typeof source.publicBaseUrl === "string" ? source.publicBaseUrl : "",
    allowPublicAdmin: source.allowPublicAdmin === true,
    adminPasswordConfigured: source.adminPasswordConfigured === true,
    gatewayTokenCount: Number.isSafeInteger(source.gatewayTokenCount) ? source.gatewayTokenCount : 0,
    accounts: accounts.map((account) => {
      const id = account.id == null ? "" : String(account.id);
      const authType = ACCOUNT_AUTH_TYPES[account.authType] ? account.authType : "api_key";
      const provider = authType === "codex_oauth"
        ? "codex_oauth"
        : (API_KEY_PROVIDERS[account.provider] ? account.provider : "auto");
      const newAPISettings = authType === "api_key" &&
        (provider === "new_api" || (provider === "auto" && Boolean(id)));
      const configuredNewAPIAuthMode = NEW_API_AUTH_MODES[account.newApiAuthMode]
        ? account.newApiAuthMode
        : "api_key";
      const newApiAuthMode = newAPISettings ? configuredNewAPIAuthMode : "api_key";
      const configuredNewAPIUserId = typeof account.newApiUserId === "number" && Number.isSafeInteger(account.newApiUserId)
        ? account.newApiUserId
        : 0;
      const newApiUserId = newAPISettings ? configuredNewAPIUserId : 0;
      const configuredQuotaResetEvery = Number.isSafeInteger(account.quotaResetEvery) && account.quotaResetEvery >= 0
        ? account.quotaResetEvery
        : 0;
      const configuredQuotaResetPeriod = QUOTA_RESET_PERIODS[account.quotaResetPeriod]
        ? account.quotaResetPeriod
        : (configuredQuotaResetEvery > 0 ? "custom" : "never");
      const manualReset = newAPISettings && newApiAuthMode === "api_key";
      const quotaResetPeriod = manualReset ? configuredQuotaResetPeriod : "never";
      const quotaResetEvery = quotaResetPeriod === "custom" ? configuredQuotaResetEvery : 0;
      const quotaResetUnit = QUOTA_RESET_UNITS[account.quotaResetUnit] ? account.quotaResetUnit : "day";
      const quotaResetAnchorAt = quotaResetPeriod === "custom"
        ? rfc3339ToLocalInput(account.quotaResetAnchorAt)
        : "";
      const quotaResetTimezone = typeof account.quotaResetTimezone === "string" && account.quotaResetTimezone.trim()
        ? account.quotaResetTimezone.trim()
        : "Asia/Shanghai";
      const savedQuotaResetTimezone = manualReset && quotaResetUsesTimezone(quotaResetPeriod, quotaResetUnit)
        ? quotaResetTimezone
        : "";
      const normalized = {
        id,
        _uiId: id || newAccountId(),
        name: typeof account.name === "string" ? account.name : "",
        authType,
        _savedAuthType: authType,
        provider,
        _savedProvider: provider,
        baseUrl: typeof account.baseUrl === "string" ? account.baseUrl : "",
        enabled: account.enabled !== false,
        _savedBaseUrl: typeof account.baseUrl === "string" ? account.baseUrl.trim() : "",
        _savedEnabled: account.enabled !== false,
        keyConfigured: account.keyConfigured === true,
        apiKey: "",
        clearApiKey: false,
        codexAuthenticated: account.codexAuthenticated === true,
        codexEmail: typeof account.codexEmail === "string" ? account.codexEmail : "",
        codexPlanType: typeof account.codexPlanType === "string" ? account.codexPlanType : "",
        codexExpiresAt: typeof account.codexExpiresAt === "string" ? account.codexExpiresAt : "",
        disableCodexCredits: authType === "codex_oauth" && account.disableCodexCredits === true,
        _savedDisableCodexCredits: authType === "codex_oauth" && account.disableCodexCredits === true,
        quotaResetPeriod,
        _savedQuotaResetPeriod: quotaResetPeriod,
        quotaResetEvery,
        _savedQuotaResetEvery: quotaResetEvery,
        quotaResetUnit,
        _savedQuotaResetUnit: quotaResetPeriod === "custom" ? quotaResetUnit : "",
        quotaResetAnchorAt,
        _savedQuotaResetAnchorAt: quotaResetPeriod === "custom" ? quotaResetAnchorAt : "",
        _savedQuotaResetAnchorRFC3339: quotaResetPeriod === "custom" && typeof account.quotaResetAnchorAt === "string"
          ? account.quotaResetAnchorAt
          : "",
        quotaResetTimezone,
        _savedQuotaResetTimezone: savedQuotaResetTimezone,
        newApiAuthMode,
        _savedNewApiAuthMode: newApiAuthMode,
        newApiUsername: newAPISettings && typeof account.newApiUsername === "string" ? account.newApiUsername : "",
        _savedNewApiUsername: newAPISettings && typeof account.newApiUsername === "string"
          ? account.newApiUsername.trim()
          : "",
        newApiUserId,
        _savedNewApiUserId: newApiUserId,
        newApiSecretConfigured: newAPISettings && account.newApiSecretConfigured === true,
        newApiSecret: "",
        clearNewApiSecret: false,
        _models: [],
        _modelsState: "idle",
        _modelsError: "",
        _modelsRequestId: 0,
      };
      normalized._modelsState = savedAccountModelsReady(normalized) ? "idle" : "incomplete";
      return normalized;
    }),
  };
}

export function normalizeStatus(value) {
  const source = value && typeof value === "object" ? value : {};
  return {
    strategy: VALID_STRATEGIES[source.strategy] ? source.strategy : "priority",
    effectiveStrategy: VALID_STRATEGIES[source.effectiveStrategy]
      ? source.effectiveStrategy
      : (VALID_STRATEGIES[source.strategy] ? source.strategy : "priority"),
    fallbackReason: typeof source.fallbackReason === "string" ? source.fallbackReason : "",
    lastRoutedAccountId: source.lastRoutedAccountId == null ? "" : String(source.lastRoutedAccountId),
    lastRoutedAccountName: typeof source.lastRoutedAccountName === "string" ? source.lastRoutedAccountName : "",
    availableAccounts: numberOrZero(source.availableAccounts),
    totalAccounts: numberOrZero(source.totalAccounts),
    accounts: Array.isArray(source.accounts) ? source.accounts : [],
  };
}

export function validateAccountData(account, options) {
  const errors = [];
  const requireKey = options && options.requireKey === true;
  const validateBalanceAuth = !(options && options.skipNewAPIAuth === true);
  const add = (field, message) => errors.push({ field, message });

  if (!account.name.trim()) {
    add("name", "请填写账号名称");
  }
  if (!ACCOUNT_AUTH_TYPES[account.authType]) {
    add("authType", "请选择有效的账号登录方式");
  }
  if (account.authType !== "api_key") {
    return errors;
  }

  if (!accountHasValidProvider(account)) {
    add("provider", account.provider === "auto"
      ? "填写或更换 API Key 时必须明确选择上游平台"
      : "请选择 New API、Sub2API 或 OpenAI Responses 兼容上游");
  }

  const urlError = validateURL(account.baseUrl.trim());
  if (urlError) {
    add("baseUrl", urlError);
  }
  const hasUsableKey = account.apiKey.trim() !== "" || (account.keyConfigured && !account.clearApiKey);
  if ((requireKey || account.enabled || !account.id) && !hasUsableKey) {
    add("apiKey", account.id
      ? "已启用账号必须保留已保存的 Key，或填写一个新 Key"
      : "新账号必须填写 API Key");
  }

  const newAPISettings = accountUsesNewAPISettings(account);
  if (newAPISettings && account.newApiAuthMode === "api_key") {
    if (!QUOTA_RESET_PERIODS[account.quotaResetPeriod]) {
      add("quotaResetPeriod", "请选择有效的套餐重置规则");
    } else if (account.quotaResetPeriod === "custom") {
      const resetEveryMaximum = account.quotaResetUnit === "week" ? 15250 : 100000;
      const resetEveryValid = Number.isSafeInteger(account.quotaResetEvery) &&
        account.quotaResetEvery >= 1 && account.quotaResetEvery <= resetEveryMaximum;
      if (!resetEveryValid) {
        add("quotaResetEvery", `自定义重置周期必须是 1 到 ${resetEveryMaximum} 的整数`);
      }
      if (!QUOTA_RESET_UNITS[account.quotaResetUnit]) {
        add("quotaResetUnit", "请选择小时、天、周或月");
      }
      if (!localInputToRFC3339(account.quotaResetAnchorAt)) {
        add("quotaResetAnchorAt", "请填写有效的重置基准时间");
      }
    }
    if (QUOTA_RESET_PERIODS[account.quotaResetPeriod] &&
        quotaResetUsesTimezone(account.quotaResetPeriod, account.quotaResetUnit) &&
        !account.quotaResetTimezone.trim()) {
      add("quotaResetTimezone", "请填写服务时区，例如 Asia/Shanghai");
    }
  }

  if (newAPISettings && validateBalanceAuth && !NEW_API_AUTH_MODES[account.newApiAuthMode]) {
    add("newApiAuthMode", "请选择有效的 New API 余额认证方式");
  } else if (newAPISettings && validateBalanceAuth &&
      (account.newApiAuthMode === "password" || account.newApiAuthMode === "access_token")) {
    if (account.newApiAuthMode === "password" && !account.newApiUsername.trim()) {
      add("newApiUsername", "请填写 New API 登录用户名");
    }
    if (account.newApiAuthMode === "access_token" &&
        (!Number.isSafeInteger(account.newApiUserId) || account.newApiUserId <= 0)) {
      add("newApiUserId", "请填写正整数 New API 用户 ID");
    }
    const authIdentifierChanged = newAPIAuthIdentifierChanged(account);
    const hasUsableSecret = account.newApiSecret.trim() !== "" ||
      (account.newApiSecretConfigured && !account.clearNewApiSecret && !authIdentifierChanged);
    if (!hasUsableSecret) {
      add("newApiSecret", account.clearNewApiSecret
        ? "已选择清除认证秘密；请取消清除并填写新值，或改用 API Key 方式"
        : (authIdentifierChanged
          ? "认证方式、用户名或用户 ID 变化后必须重新填写对应秘密"
          : "请填写此认证方式需要的秘密"));
    }
  }
  return errors;
}

export function httpURLs(accounts) {
  return accounts.filter((account) => account.authType === "api_key")
    .map((account) => account.baseUrl.trim())
    .filter((value, index, list) => value.toLowerCase().startsWith("http://") && list.indexOf(value) === index);
}

export function saveAccountPayload(account) {
  const apiKeyAccount = account.authType === "api_key";
  const newAPISettings = apiKeyAccount && accountUsesNewAPISettings(account);
  const manualReset = newAPISettings && account.newApiAuthMode === "api_key";
  const resetPeriod = manualReset && QUOTA_RESET_PERIODS[account.quotaResetPeriod]
    ? account.quotaResetPeriod
    : "never";
  const customReset = resetPeriod === "custom";
  const resetEvery = customReset && Number.isSafeInteger(account.quotaResetEvery) ? account.quotaResetEvery : 0;
  const resetUnit = customReset ? account.quotaResetUnit : "";
  let resetAnchor = "";
  if (customReset) {
    resetAnchor = account.quotaResetAnchorAt === account._savedQuotaResetAnchorAt && account._savedQuotaResetAnchorRFC3339
      ? account._savedQuotaResetAnchorRFC3339
      : localInputToRFC3339(account.quotaResetAnchorAt);
  }
  return {
    id: account.id,
    name: account.name.trim(),
    authType: account.authType,
    provider: apiKeyAccount ? account.provider : "codex_oauth",
    baseUrl: apiKeyAccount ? account.baseUrl.trim() : "",
    enabled: account.enabled,
    apiKey: apiKeyAccount ? account.apiKey : "",
    clearApiKey: apiKeyAccount && account.clearApiKey,
    disableCodexCredits: !apiKeyAccount && account.disableCodexCredits === true,
    quotaResetPeriod: resetPeriod,
    quotaResetEvery: resetEvery,
    quotaResetUnit: resetUnit,
    quotaResetAnchorAt: resetAnchor,
    quotaResetTimezone: manualReset && quotaResetUsesTimezone(resetPeriod, resetUnit)
      ? account.quotaResetTimezone.trim()
      : "",
    newApiAuthMode: newAPISettings ? account.newApiAuthMode : "api_key",
    newApiUsername: newAPISettings ? account.newApiUsername.trim() : "",
    newApiUserId: newAPISettings ? account.newApiUserId : 0,
    newApiSecret: newAPISettings && account.newApiAuthMode !== "api_key" ? account.newApiSecret : "",
    clearNewApiSecret: newAPISettings && account.clearNewApiSecret,
  };
}
