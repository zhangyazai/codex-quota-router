import {
  API_KEY_PROVIDERS,
  NEW_API_AUTH_MODES,
  accountHasUsableModelConfig,
  accountRuntimeConfigChanged,
  accountUIID,
  accountUsesNewAPISettings,
  createNewAccount,
  formatInteger,
  formatOptionalDateTime,
  newAPIAuthIdentifierChanged,
  numberOrZero,
  validateAccountData,
} from "../domain/config-model.mjs";

const strategyLabels = {
  priority: "按优先级顺序",
  round_robin: "轮询使用",
  least_used: "最少使用优先",
  quota_reset: "套餐重置优先",
  highest_balance: "余额最多优先",
  lowest_balance: "余额最少优先",
};

const strategyDescriptions = {
  priority: "从列表顶部开始选择第一个可参与路由的账号；请求明确失败或进入冷却后继续向后尝试。",
  round_robin: "在可参与路由的账号之间依次轮换，让请求数量更均匀。",
  least_used: "从当前可参与路由的账号中选择本次服务启动以来已分配请求数最少的账号，连续分配 30 次后重新比较；批次内账号一旦不可用会立即切换。服务重启后计数重新开始。",
  quota_reset: "优先使用下次套餐额度重置时间最早的账号。Codex OAuth 使用缓存的官方用量窗口；登录 New API 的账号自动读取订阅信息，不登录的账号使用手工规则。没有重置时间的账号排在最后，时间相同按列表顺序。",
  highest_balance: "只有全部路由候选的账户、订阅与 API Key 可用余额都已验证、新鲜、可读取且单位可比较时才按余额选择；短暂网络、限流或超时失败会在最近成功值仍处于 5 分钟有效期时继续使用并显示警告。认证失败、仅有 API Key 额度、余额未知或过期、没有成功值、单位不同，都会整体回退到最少使用优先。",
  lowest_balance: "只有全部路由候选的账户、订阅与 API Key 可用余额都已验证、新鲜、可读取且单位可比较时，才优先选择有限余额最低的账号；无限额度排在有限额度之后，余额相同按列表顺序。短暂网络、限流或超时失败会在最近成功值仍处于 5 分钟有效期时继续使用并显示警告；认证失败、仅有 API Key 额度、余额未知或过期、没有成功值、单位不同，都会整体回退到最少使用优先。",
};

const stateLabels = {
  available: "可参与路由",
  recent_success: "近期成功",
  recent_failure: "最近失败",
  ready: "可参与路由",
  active: "正在使用",
  cooldown: "冷却中",
  cooling_down: "冷却中",
  recovery_pending: "待真实请求验证",
  temporarily_disabled: "暂时停用",
  incomplete: "配置不完整",
  blocked: "已阻止",
  pending_changes: "待保存",
  exhausted: "额度耗尽",
  quota_exhausted: "额度耗尽",
  unavailable: "暂不可用",
  invalid: "Key 无效",
  disabled: "已停用",
  unverified: "待验证",
  unknown: "状态未知",
};

const fallbackReasonLabels = {
  balance_stale: "部分路由候选的余额数据已过期",
  balance_unavailable: "部分路由候选的余额未知或不可用",
  balance_account_unverified: "部分路由候选仅有 API Key 额度，尚未核对账户及订阅余额",
  balance_unit_unknown: "部分余额缺少可比较的单位",
  balance_unit_mismatch: "账号余额单位不同，无法直接比较",
};

export function formatSubscriptionQuota(subscription) {
  if (!subscription || typeof subscription !== "object") return "";
  const resetAt = formatOptionalDateTime(subscription.resetAt, "");
  let result = "";
  if (subscription.unlimited === true) {
    result = "订阅额度：无限";
  } else if (typeof subscription.total === "number" && Number.isFinite(subscription.total) && subscription.total >= 0 &&
      typeof subscription.remaining === "number" && Number.isFinite(subscription.remaining) && subscription.remaining >= 0) {
    const formatter = new Intl.NumberFormat("zh-CN", { minimumFractionDigits: 2, maximumFractionDigits: 6 });
    const unit = subscription.displayLabel || subscription.unit || "";
    result = `订阅额度：剩余 ${formatter.format(subscription.remaining)} / ${formatter.format(subscription.total)}`;
    if (unit) result += ` ${String(unit)}`;
  }
  if (!result) return "";
  return resetAt ? `${result} · ${resetAt} 重置` : result;
}

export function splitAccountSummaryText(value) {
  const text = typeof value === "string" ? value.trim() : "";
  const noteStart = text.indexOf("（");
  if (noteStart <= 0 || !text.endsWith("）")) return { value: text, note: "" };
  return {
    value: text.slice(0, noteStart).trim(),
    note: text.slice(noteStart + 1, -1).replace(/）（/g, "；").trim(),
  };
}

function formatAutomaticQuotaReset(balance) {
  const subscription = balance && balance.subscription;
  if (!subscription || typeof subscription !== "object") {
    return "自动读取：尚未获取到有效订阅，当前不参与套餐重置优先。";
  }
  const preference = typeof subscription.billingPreference === "string"
    ? subscription.billingPreference.trim().toLowerCase()
    : "";
  const preferenceLabel = {
    "": "订阅优先（上游默认）",
    subscription_first: "订阅优先",
    subscription_only: "仅使用订阅",
    wallet_first: "钱包优先",
    wallet_only: "仅使用钱包",
  }[preference] || `未知计费偏好 ${preference}`;
  const resetAt = formatOptionalDateTime(subscription.resetAt, "");
  const priorityResetAt = formatOptionalDateTime(subscription.priorityResetAt, "");
  const remaining = typeof subscription.remaining === "number" && Number.isFinite(subscription.remaining)
    ? subscription.remaining
    : null;
  const remainingText = remaining === null ? "未知" : new Intl.NumberFormat("zh-CN", {
    maximumFractionDigits: 6,
  }).format(Math.max(0, remaining));
  const prefix = `自动读取：计费偏好为${preferenceLabel}，订阅剩余 ${remainingText}。`;
  if (!balance || balance.status !== "ok" || balance.fresh !== true ||
      !["", "ok", "partial"].includes(balance.refreshStatus || "")) {
    return `${prefix}余额数据当前不可用或已过期，不参与套餐重置优先。`;
  }
  if (preference === "wallet_only") {
    return `${prefix}当前仅使用钱包，订阅重置不会恢复钱包，不参与套餐重置优先。`;
  }
  if (preference === "wallet_first") {
    return `${prefix}当前请求先消耗钱包，不能保证优先消耗即将清零的订阅，不参与套餐重置优先${resetAt ? `；套餐额度预计 ${resetAt} 重置` : ""}。`;
  }
  if (!["", "subscription_first", "subscription_only"].includes(preference)) {
    return `${prefix}无法确认请求会优先消耗订阅额度，不参与套餐重置优先。`;
  }
  if (subscription.unlimited === true) {
    return `${prefix}无限订阅不会发生有限额度清零，不参与套餐重置优先。`;
  }
  if (remaining === null) {
    return `${prefix}无法确认可用订阅额度，不参与套餐重置优先。`;
  }
  if (remaining <= 0) {
    return `${prefix}订阅额度已用完，不参与优先排序${resetAt ? `；预计 ${resetAt} 重置后恢复` : "，且尚无有效恢复时间"}。`;
  }
  if (!priorityResetAt) {
    return `${prefix}没有可用于优先排序的未来重置时间${resetAt ? `；订阅恢复时间为 ${resetAt}` : ""}。`;
  }
  return `${prefix}参与套餐重置优先，排序重置时间为 ${priorityResetAt}。`;
}

export function createAccountsView({ shell, getConfig, getStatus, codex, formatBalance, onAccountAction, showView }) {
  const elements = {
    summaryStrategy: document.getElementById("summaryStrategy"),
    summaryAccounts: document.getElementById("summaryAccounts"),
    summaryLastAccount: document.getElementById("summaryLastAccount"),
    strategy: document.getElementById("strategy"),
    strategyHelp: document.getElementById("strategyHelp"),
    effectiveStrategyNotice: document.getElementById("effectiveStrategyNotice"),
    fallbackWarning: document.getElementById("fallbackWarning"),
    accountList: document.getElementById("accountList"),
    emptyState: document.getElementById("emptyState"),
    accountTemplate: document.getElementById("accountTemplate"),
    addAccountButton: document.getElementById("addAccountButton"),
    emptyAddButton: document.getElementById("emptyAddButton"),
    httpRiskDialog: document.getElementById("httpRiskDialog"),
    httpRiskList: document.getElementById("httpRiskList"),
  };

  function getStatusAccount(id) {
    if (!id) return null;
    const target = String(id);
    return getStatus().accounts.find((account) => String(account.id) === target) || null;
  }

  function getAccount(id) {
    const target = String(id);
    return getConfig().accounts.find((account) => accountUIID(account) === target) || null;
  }

  function getAccountIndex(id) {
    const target = String(id);
    return getConfig().accounts.findIndex((account) => accountUIID(account) === target);
  }

  function findCard(id) {
    return Array.from(elements.accountList.querySelectorAll(".account-card"))
      .find((card) => card.dataset.accountId === String(id)) || null;
  }

  function fieldDomId(field, index) {
    return `account-${index + 1}-${field}`;
  }

  function setFieldIdentity(card, index, field) {
    const input = card.querySelector(`[data-field="${field}"]`);
    const label = card.querySelector(`[data-label="${field}"]`);
    const helper = card.querySelector(`[data-helper="${field}"]`);
    const error = card.querySelector(`[data-error="${field}"]`);
    const id = fieldDomId(field, index);
    if (input) input.id = id;
    if (label) label.htmlFor = id;
    const describedBy = [];
    if (helper) {
      helper.id = `${id}-help`;
      describedBy.push(helper.id);
    }
    if (error) {
      error.id = `${id}-error`;
      describedBy.push(error.id);
    }
    if (input && describedBy.length) input.setAttribute("aria-describedby", describedBy.join(" "));
  }

  function updateAccountControlLabels(card, account, index) {
    const name = account.name.trim() || "未命名账号";
    const enabled = card.querySelector('[data-field="enabled"]');
    const clearKey = card.querySelector('[data-field="clearApiKey"]');
    const clearNewAPISecret = card.querySelector('[data-field="clearNewApiSecret"]');
    const disableCodexCredits = card.querySelector('[data-field="disableCodexCredits"]');
    const labels = {
      edit: `编辑账号 ${name}`,
      "move-up": `上移账号 ${name}，当前第 ${index + 1} 位`,
      "move-down": `下移账号 ${name}，当前第 ${index + 1} 位`,
      "test-balance": `查询账号 ${name} 当前填写内容的余额`,
      models: `刷新账号 ${name} 当前填写内容支持的模型`,
      resume: `立即唤醒账号 ${name} 的真实请求验证`,
      reset: `验证账号 ${name}，成功后清除阻止或冷却状态`,
      "codex-login": `登录或重新授权 Codex OAuth 账号 ${name}`,
      "codex-usage": `刷新 Codex OAuth 账号 ${name} 的用量`,
      "codex-logout": `退出 Codex OAuth 账号 ${name}`,
      delete: `删除账号 ${name}`,
      "close-editor": `${account.id ? "完成编辑账号 " : "完成添加账号 "}${name}`,
    };
    enabled.setAttribute("aria-label", `启用账号 ${name}`);
    clearKey.setAttribute("aria-label", `清除账号 ${name} 已保存的 API Key`);
    clearNewAPISecret.setAttribute("aria-label", `清除账号 ${name} 已保存的 New API 余额认证秘密`);
    disableCodexCredits.setAttribute("aria-label", `禁止 Codex OAuth 账号 ${name} 使用额外点数`);
    Object.entries(labels).forEach(([action, label]) => {
      const button = card.querySelector(`[data-action="${action}"]`);
      if (button) button.setAttribute("aria-label", label);
    });
  }

  function updateProviderField(card, account) {
    const input = card.querySelector('[data-field="provider"]');
    const helper = card.querySelector('[data-helper="provider"]');
    const legacyAutoOption = input.querySelector('[data-provider-option="legacy-auto"]');
    const codexOAuthOption = input.querySelector('[data-provider-option="codex-oauth"]');
    const isOAuth = account.authType === "codex_oauth";
    const legacyAuto = !isOAuth && account.provider === "auto" && Boolean(account.id);
    const replacingLegacyKey = legacyAuto && account.apiKey.trim() !== "";
    legacyAutoOption.hidden = !legacyAuto;
    legacyAutoOption.disabled = !legacyAuto;
    codexOAuthOption.hidden = !isOAuth;
    codexOAuthOption.disabled = !isOAuth;
    input.value = isOAuth ? "codex_oauth" : (API_KEY_PROVIDERS[account.provider] ? account.provider : "");
    shell.setDisabledPreservingBusy(input, isOAuth);
    helper.textContent = isOAuth
      ? "固定为 Codex 官方 OAuth（codex_oauth），由登录方式决定。"
      : (legacyAuto
        ? (replacingLegacyKey
          ? "已填写新的 API Key，必须先选择实际平台。"
          : "此旧账号尚未明确平台，可原样保存；请选择实际平台以停止使用兼容状态。")
        : "请选择实际平台；不会根据域名猜测。修改已保存账号的平台后，原有活跃会话将不能继续。");
  }

  function updateNewAPIAuthFields(card, account) {
    const mode = NEW_API_AUTH_MODES[account.newApiAuthMode] ? account.newApiAuthMode : "api_key";
    const active = accountUsesNewAPISettings(account);
    const settings = card.querySelector('[data-slot="new-api-auth-settings"]');
    const modeInput = card.querySelector('[data-field="newApiAuthMode"]');
    const mustReplaceSecret = mode !== "api_key" && newAPIAuthIdentifierChanged(account);
    const secretGroup = card.querySelector("[data-auth-secret]");
    const secretInput = card.querySelector('[data-field="newApiSecret"]');
    const secretLabel = card.querySelector('[data-label="newApiSecret"]');
    const secretHelp = card.querySelector('[data-slot="new-api-secret-help"]');
    const clearSecret = card.querySelector('[data-field="clearNewApiSecret"]');
    const clearSecretRow = card.querySelector('[data-slot="clear-new-api-secret"]');
    settings.hidden = !active;
    modeInput.disabled = !active;
    card.querySelectorAll("[data-auth-panel]").forEach((panel) => {
      const hidden = !active || panel.dataset.authPanel !== mode;
      panel.hidden = hidden;
      panel.querySelectorAll("input, select").forEach((control) => {
        control.disabled = hidden;
      });
    });
    secretGroup.hidden = !active || mode === "api_key";
    secretLabel.textContent = mode === "access_token" ? "用户 Access Token" : "New API 登录密码";
    secretInput.setAttribute("autocomplete", mode === "password" ? "current-password" : "off");
    clearSecretRow.hidden = !active || mode === "api_key";
    clearSecret.disabled = !active || mode === "api_key" || !account.newApiSecretConfigured;
    secretInput.disabled = !active || mode === "api_key";
    secretInput.placeholder = account.newApiSecretConfigured && !mustReplaceSecret
      ? "已保存；留空则保留"
      : (mode === "access_token" ? "输入上游生成的用户 Access Token" : "输入上游登录密码");
    secretHelp.textContent = account.clearNewApiSecret
      ? "已选择清除。若要继续使用此认证方式，请取消清除并填写新的秘密。"
      : (account.newApiSecret.trim()
        ? "已填写新秘密；保存后不会在页面中回显。"
        : (mustReplaceSecret
          ? "认证方式、用户名或用户 ID 已变化，必须填写与新身份匹配的秘密。"
          : (account.newApiSecretConfigured
            ? "秘密不会回显。留空将保留原值，输入新值将替换原值。"
            : "该认证方式尚未保存秘密；保存前必须填写。")));
  }

  function updateQuotaResetFields(card, account) {
    const periodInput = card.querySelector('[data-field="quotaResetPeriod"]');
    const everyInput = card.querySelector('[data-field="quotaResetEvery"]');
    const unitInput = card.querySelector('[data-field="quotaResetUnit"]');
    const anchorInput = card.querySelector('[data-field="quotaResetAnchorAt"]');
    const timezoneInput = card.querySelector('[data-field="quotaResetTimezone"]');
    const manualPanel = card.querySelector('[data-slot="quota-reset-manual"]');
    const automaticPanel = card.querySelector('[data-slot="quota-reset-auto"]');
    const customPanel = card.querySelector('[data-slot="quota-reset-custom"]');
    const timezonePanel = card.querySelector('[data-slot="quota-reset-timezone"]');
    const rule = card.querySelector('[data-slot="quota-reset-rule"]');
    const settings = card.querySelector('[data-slot="quota-reset-settings"]');
    const active = accountUsesNewAPISettings(account);
    const manual = active && account.newApiAuthMode === "api_key";
    const custom = manual && account.quotaResetPeriod === "custom";
    const timezoneRequired = manual && (["daily", "weekly", "monthly"].includes(account.quotaResetPeriod) ||
      (custom && account.quotaResetUnit === "month"));
    const maximum = account.quotaResetUnit === "week" ? 15250 : 100000;
    everyInput.max = String(maximum);
    card.querySelector('[data-helper="quotaResetEvery"]').textContent = account.quotaResetUnit === "week"
      ? "填写 1 到 15250 的整数。"
      : "填写 1 到 100000 的整数。";
    settings.hidden = !active;
    manualPanel.hidden = !manual;
    automaticPanel.hidden = !active || manual;
    customPanel.hidden = !custom;
    timezonePanel.hidden = !timezoneRequired;
    periodInput.disabled = !manual;
    everyInput.disabled = !custom;
    unitInput.disabled = !custom;
    anchorInput.disabled = !custom;
    timezoneInput.disabled = !timezoneRequired;
    const timezone = String(account.quotaResetTimezone || "").trim() || "未填写时区";
    rule.textContent = {
      never: "规则说明：不设置手工重置时间。",
      daily: `规则说明：每天 00:00（${timezone}）重置。`,
      weekly: `规则说明：每周一 00:00（${timezone}）重置。`,
      monthly: `规则说明：每月 1 日 00:00（${timezone}）重置。`,
      custom: `规则说明：从基准时间起，每隔 ${Number.isSafeInteger(account.quotaResetEvery) && account.quotaResetEvery > 0 ? account.quotaResetEvery : "?"} ${{
        hour: "小时",
        day: "天",
        week: "周",
        month: "个月",
      }[account.quotaResetUnit] || "个周期"}重置${account.quotaResetUnit === "month" ? `（${timezone}）` : ""}。`,
    }[account.quotaResetPeriod] || "规则说明：请选择有效的套餐重置规则。";
    const accountStatus = getStatusAccount(account.id);
    const next = card.querySelector('[data-slot="quota-reset-next"]');
    if (!active) {
      next.textContent = "此平台不使用 New API 套餐重置规则。";
    } else if (!manual) {
      next.textContent = accountRuntimeConfigChanged(account)
        ? "账户登录配置尚未保存；保存并刷新余额后自动判断是否参与套餐重置优先。"
        : formatAutomaticQuotaReset(accountStatus && accountStatus.balance);
    } else if (account.id && accountRuntimeConfigChanged(account)) {
      next.textContent = `当前编辑尚未保存；服务器已保存配置的下次重置时间：${formatOptionalDateTime(accountStatus && accountStatus.quotaResetAt, "尚无数据")}。`;
    } else if (account.quotaResetPeriod === "never") {
      next.textContent = "未设置手工重置规则；套餐重置优先策略会把此账号排在有重置时间的账号之后。";
    } else if (accountRuntimeConfigChanged(account)) {
      next.textContent = "当前手工规则尚未保存；保存后由服务端计算下一次重置时间。";
    } else {
      next.textContent = `服务端计算的下次重置时间：${formatOptionalDateTime(accountStatus && accountStatus.quotaResetAt, "尚无数据")}。`;
    }
  }

  function accountAPIKeyState(account) {
    return (account.keyConfigured ? "服务器已保存 API Key" : "尚未保存 API Key") +
      (accountUsesNewAPISettings(account) && account.newApiSecretConfigured ? " · 已保存余额认证秘密" : "");
  }

  function updateAccountTypeFields(card, account) {
    const isOAuth = account.authType === "codex_oauth";
    card.querySelectorAll("[data-account-auth]").forEach((panel) => {
      const hidden = panel.dataset.accountAuth !== account.authType;
      panel.hidden = hidden;
      panel.querySelectorAll("input, select, button").forEach((control) => {
        shell.setDisabledPreservingBusy(control, hidden);
      });
    });
    const keyInput = card.querySelector('[data-field="apiKey"]');
    const clearKey = card.querySelector('[data-field="clearApiKey"]');
    keyInput.disabled = isOAuth || account.clearApiKey;
    clearKey.disabled = isOAuth || !account.keyConfigured;
    updateProviderField(card, account);
    updateNewAPIAuthFields(card, account);
    updateQuotaResetFields(card, account);
    card.querySelector(".account-models").hidden = isOAuth;
    card.querySelector('[data-slot="balance-label"]').textContent = isOAuth ? "Codex 用量 / 点数" : "账面余额";
    card.querySelector('[data-slot="url"]').textContent = isOAuth ? "OpenAI Codex OAuth" :
      (account.baseUrl.trim() || "尚未填写 URL");
    card.querySelector('[data-slot="key-state"]').textContent = isOAuth
      ? (account.codexAuthenticated
        ? `已登录${account.codexEmail ? ` · ${account.codexEmail}` : ""}${account.codexPlanType ? ` · ${account.codexPlanType}` : ""}`
        : "尚未登录 OpenAI")
      : accountAPIKeyState(account);
    card.querySelectorAll("button[data-api-action]").forEach((button) => {
      button.hidden = isOAuth;
      shell.setDisabledPreservingBusy(button, isOAuth);
    });
    card.querySelector('[data-field="authType"]').disabled = codex.accountBusy(account);
    codex.updatePanel(card, account);
  }

  function updateAccountHTTPWarning(card, account) {
    const warning = card.querySelector('[data-slot="http-warning"]');
    const insecure = account.authType === "api_key" && account.baseUrl.trim().toLowerCase().startsWith("http://");
    shell.setTextIfChanged(warning, insecure
      ? `安全风险：${account.name.trim() || "此账号"} 使用明文 HTTP。路由请求时，API Key、余额认证凭据、提示词、代码和响应可能被窃听；建议尽快改用 HTTPS。`
      : "");
    shell.setHiddenIfChanged(warning, !insecure);
  }

  function accountModelsText(account) {
    if (account._modelsState === "loading") return "正在获取…";
    if (account._modelsState === "ready") return account._models.join("、");
    if (account._modelsState === "empty") return "上游未返回模型";
    if (account._modelsState === "error") return `获取失败${account._modelsError ? `：${account._modelsError}` : ""}`;
    if (account._modelsState === "stale") return "配置已修改，需刷新";
    if (account._modelsState === "incomplete") return account.id ? "配置未完整，无法获取" : "配置未完整，保存后自动获取";
    return "尚未获取";
  }

  function updateAccountModels(card, account) {
    const element = card && card.querySelector('[data-slot="models"]');
    if (element) shell.setTextIfChanged(element, accountModelsText(account));
  }

  function invalidateAccountModels(account) {
    account._models = [];
    account._modelsError = "";
    account._modelsRequestId = (account._modelsRequestId || 0) + 1;
    account._modelsState = accountHasUsableModelConfig(account) ? "stale" : "incomplete";
    const card = findCard(accountUIID(account));
    const result = card && card.querySelector('[data-slot="models-result"]');
    if (result) {
      result.className = "test-result";
      result.textContent = "";
    }
  }

  function renderAccounts() {
    elements.accountList.replaceChildren();
    elements.emptyState.hidden = getConfig().accounts.length !== 0;
    getConfig().accounts.forEach((account, index) => {
      const card = elements.accountTemplate.content.firstElementChild.cloneNode(true);
      const editor = card.querySelector('[data-slot="editor"]');
      const editorTitle = card.querySelector('[data-slot="editor-title"]');
      const editButton = card.querySelector('[data-action="edit"]');
      const enabledInput = card.querySelector('[data-field="enabled"]');
      const nameInput = card.querySelector('[data-field="name"]');
      const authTypeInput = card.querySelector('[data-field="authType"]');
      const providerInput = card.querySelector('[data-field="provider"]');
      const urlInput = card.querySelector('[data-field="baseUrl"]');
      const keyInput = card.querySelector('[data-field="apiKey"]');
      const clearInput = card.querySelector('[data-field="clearApiKey"]');
      const quotaResetPeriodInput = card.querySelector('[data-field="quotaResetPeriod"]');
      const quotaResetEveryInput = card.querySelector('[data-field="quotaResetEvery"]');
      const quotaResetUnitInput = card.querySelector('[data-field="quotaResetUnit"]');
      const quotaResetAnchorInput = card.querySelector('[data-field="quotaResetAnchorAt"]');
      const quotaResetTimezoneInput = card.querySelector('[data-field="quotaResetTimezone"]');
      const newAPIAuthModeInput = card.querySelector('[data-field="newApiAuthMode"]');
      const newAPIUsernameInput = card.querySelector('[data-field="newApiUsername"]');
      const newAPIUserIDInput = card.querySelector('[data-field="newApiUserId"]');
      const newAPISecretInput = card.querySelector('[data-field="newApiSecret"]');
      const clearNewAPISecretInput = card.querySelector('[data-field="clearNewApiSecret"]');
      const disableCodexCreditsInput = card.querySelector('[data-field="disableCodexCredits"]');
      const editorId = `account-${index + 1}-editor`;
      const editorTitleId = `${editorId}-title`;
      const uiID = accountUIID(account);

      card.dataset.accountId = uiID;
      card.querySelector('[data-slot="priority"]').textContent = String(index + 1);
      card.querySelector('[data-slot="priority"]').setAttribute("aria-label", `优先级 ${index + 1}`);
      card.querySelector('[data-slot="name"]').textContent = account.name.trim() || "未命名账号";
      card.querySelector('[data-slot="url"]').textContent = account.baseUrl.trim() || "尚未填写 URL";
      card.querySelector('[data-slot="key-state"]').textContent = accountAPIKeyState(account);

      enabledInput.checked = account.enabled;
      enabledInput.dataset.accountId = uiID;
      nameInput.value = account.name;
      nameInput.dataset.accountId = uiID;
      authTypeInput.value = account.authType;
      authTypeInput.dataset.accountId = uiID;
      providerInput.dataset.accountId = uiID;
      urlInput.value = account.baseUrl;
      urlInput.dataset.accountId = uiID;
      keyInput.value = account.apiKey;
      keyInput.dataset.accountId = uiID;
      clearInput.checked = account.clearApiKey;
      clearInput.dataset.accountId = uiID;
      clearInput.disabled = !account.keyConfigured;
      keyInput.disabled = account.authType !== "api_key" || account.clearApiKey;
      keyInput.placeholder = account.keyConfigured ? "已保存；留空则保留" : "输入此账号的 API Key";
      card.querySelector('[data-slot="key-help"]').textContent = account.keyConfigured
        ? "Key 不会回显。留空将保留原值，输入新值将替换原值。"
        : "此账号尚未配置 Key；保存新账号前必须填写。";

      quotaResetPeriodInput.value = account.quotaResetPeriod || "never";
      quotaResetPeriodInput.dataset.accountId = uiID;
      quotaResetEveryInput.value = account.quotaResetEvery > 0 ? String(account.quotaResetEvery) : "";
      quotaResetEveryInput.dataset.accountId = uiID;
      quotaResetUnitInput.value = account.quotaResetUnit || "day";
      quotaResetUnitInput.dataset.accountId = uiID;
      quotaResetAnchorInput.value = account.quotaResetAnchorAt || "";
      quotaResetAnchorInput.dataset.accountId = uiID;
      quotaResetTimezoneInput.value = account.quotaResetTimezone || "Asia/Shanghai";
      quotaResetTimezoneInput.dataset.accountId = uiID;
      newAPIAuthModeInput.value = account.newApiAuthMode;
      newAPIAuthModeInput.dataset.accountId = uiID;
      newAPIUsernameInput.value = account.newApiUsername;
      newAPIUsernameInput.dataset.accountId = uiID;
      newAPIUserIDInput.value = account.newApiUserId > 0 ? String(account.newApiUserId) : "";
      newAPIUserIDInput.dataset.accountId = uiID;
      newAPISecretInput.value = account.newApiSecret;
      newAPISecretInput.dataset.accountId = uiID;
      clearNewAPISecretInput.checked = account.clearNewApiSecret;
      clearNewAPISecretInput.dataset.accountId = uiID;
      disableCodexCreditsInput.checked = account.disableCodexCredits;
      disableCodexCreditsInput.dataset.accountId = uiID;

      ["name", "authType", "provider", "baseUrl", "apiKey", "quotaResetPeriod", "quotaResetEvery", "quotaResetUnit",
        "quotaResetAnchorAt", "quotaResetTimezone", "newApiAuthMode", "newApiUsername", "newApiUserId", "newApiSecret",
        "disableCodexCredits"]
        .forEach((field) => setFieldIdentity(card, index, field));
      updateAccountTypeFields(card, account);

      editor.id = editorId;
      editorTitle.id = editorTitleId;
      editorTitle.textContent = account.id ? "编辑账号" : "添加账号";
      editor.setAttribute("aria-labelledby", editorTitleId);
      editor.addEventListener("close", () => {
        window.setTimeout(() => {
          const active = document.activeElement;
          if (!active || active === document.body || active.closest("[hidden], dialog:not([open])")) {
            const currentCard = findCard(uiID);
            const returnButton = currentCard && currentCard.querySelector('[data-action="edit"]');
            if (returnButton) returnButton.focus();
          }
        }, 0);
      });
      editButton.setAttribute("aria-controls", editorId);
      editButton.setAttribute("aria-haspopup", "dialog");
      card.querySelectorAll("button[data-action]").forEach((button) => {
        button.dataset.accountId = uiID;
      });
      card.querySelector('[data-action="move-up"]').disabled = index === 0;
      card.querySelector('[data-action="move-down"]').disabled = index === getConfig().accounts.length - 1;
      card.querySelector('[data-action="reset"]').disabled = getStatusAccount(account.id) === null;
      card.querySelector('[data-action="resume"]').disabled = getStatusAccount(account.id) === null;
      card.querySelector('[data-action="clear-statistics"]').disabled = getStatusAccount(account.id) === null;
      updateAccountControlLabels(card, account, index);
      updateAccountHTTPWarning(card, account);
      elements.accountList.appendChild(card);
      updateAccountStatus(card, account);
      updateAccountModels(card, account);
    });
    shell.updatePageInteractivity();
  }

  function completedRequestCount(accountStatus) {
    return numberOrZero(accountStatus && accountStatus.successfulRequests) +
      numberOrZero(accountStatus && accountStatus.failedRequests);
  }

  function formatRequestHealth(accountStatus) {
    const successful = numberOrZero(accountStatus && accountStatus.successfulRequests);
    const completed = completedRequestCount(accountStatus);
    if (completed === 0) return "—";
    return `${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 }).format(successful / completed * 100)}%`;
  }

  function updateSubscriptionQuota(card, account, accountStatus, suffix = "") {
    const text = accountUsesNewAPISettings(account)
      ? formatSubscriptionQuota(accountStatus && accountStatus.balance && accountStatus.balance.subscription)
      : "";
    const value = card.querySelector('[data-slot="subscription-quota"]');
    value.hidden = !text;
    value.textContent = text ? `${text}${suffix}` : "";
  }

  function updateBalanceSummary(card, text) {
    const summary = splitAccountSummaryText(text);
    const value = card.querySelector('[data-slot="balance"]');
    const note = card.querySelector('[data-slot="balance-note"]');
    value.textContent = summary.value;
    note.textContent = summary.note;
    note.hidden = !summary.note;
  }

  function updateAccountStatus(card, account) {
    const accountStatus = getStatusAccount(account.id);
    const stateElement = card.querySelector('[data-slot="state"]');
    let state = account.enabled ? "unknown" : "disabled";
    updateAccountTypeFields(card, account);
    if (accountRuntimeConfigChanged(account)) {
      const isNewAccount = !account.id;
      stateElement.textContent = stateLabels.pending_changes;
      stateElement.className = "tag warning";
      stateElement.title = isNewAccount ? "新账号尚未保存" :
        "登录方式、平台、URL、Key、套餐重置规则、余额认证、点数使用策略或启用状态尚未保存；运行状态仍对应服务器中的旧配置";
      updateBalanceSummary(card, isNewAccount ? "保存后获取" :
        (account.authType === "codex_oauth"
          ? `${codex.formatAccountSummary(account, accountStatus)}（服务器已保存配置）`
          : `${formatBalance(accountStatus && accountStatus.balance)}（服务器已保存配置）`));
      updateSubscriptionQuota(card, account, accountStatus, isNewAccount ? "" : "（服务器已保存配置）");
      card.querySelector('[data-slot="requests"]').textContent = isNewAccount ? "—" :
        formatInteger(completedRequestCount(accountStatus));
      card.querySelector('[data-slot="successful-requests"]').textContent = isNewAccount ? "—" :
        formatInteger(accountStatus && accountStatus.successfulRequests);
      card.querySelector('[data-slot="failed-requests"]').textContent = isNewAccount ? "—" :
        formatInteger(accountStatus && accountStatus.failedRequests);
      card.querySelector('[data-slot="request-health"]').textContent = isNewAccount ? "—" : formatRequestHealth(accountStatus);
      const pendingResetButton = card.querySelector('[data-action="reset"]');
      if (pendingResetButton) {
        shell.setDisabledPreservingBusy(pendingResetButton, true);
        pendingResetButton.title = "请先保存当前账号配置";
      }
      const pendingResumeButton = card.querySelector('[data-action="resume"]');
      if (pendingResumeButton) {
        pendingResumeButton.hidden = true;
        shell.setDisabledPreservingBusy(pendingResumeButton, true);
      }
      const pendingClearStatisticsButton = card.querySelector('[data-action="clear-statistics"]');
      if (pendingClearStatisticsButton) {
        shell.setDisabledPreservingBusy(pendingClearStatisticsButton, isNewAccount || !accountStatus);
      }
      return;
    }
    if (account.enabled && accountStatus && typeof accountStatus.state === "string") state = accountStatus.state;
    const recoveryPending = Boolean(accountStatus && !accountStatus.cooldownUntil &&
      ((accountStatus.blockedReason === "quota" || accountStatus.blockedReason === "unauthorized") || accountStatus.cooldownReason));
    const nextProbeDetail = accountStatus && accountStatus.nextProbeAt
      ? `；最早受控验证：${formatOptionalDateTime(accountStatus.nextProbeAt, String(accountStatus.nextProbeAt))}`
      : "";
    if (recoveryPending) state = "recovery_pending";
    stateElement.textContent = stateLabels[state] || state || "状态未知";
    stateElement.className = "tag";
    if (state === "recent_success" || state === "active") stateElement.classList.add("success");
    else if (["cooldown", "cooling_down", "recovery_pending", "unverified", "unknown", "incomplete"].includes(state)) {
      stateElement.classList.add("warning");
    } else if (["recent_failure", "temporarily_disabled", "exhausted", "quota_exhausted", "unavailable", "invalid", "blocked"].includes(state)) {
      stateElement.classList.add("danger");
    }
    stateElement.title = accountStatus
      ? `路由状态：${stateLabels[accountStatus.state] || accountStatus.state || "状态未知"}` +
        `；近期连通性：${stateLabels[accountStatus.healthState] || accountStatus.healthState || "待验证"}` +
        nextProbeDetail +
        (recoveryPending ? "；下一次匹配的真实请求将进行单次验证" :
          (accountStatus.state === "available" ? "；可参与路由不代表模型请求当前可用" : ""))
      : `服务端状态：${state || "unknown"}`;
    updateBalanceSummary(card, account.authType === "codex_oauth"
      ? codex.formatAccountSummary(account, accountStatus)
      : formatBalance(accountStatus && accountStatus.balance));
    updateSubscriptionQuota(card, account, accountStatus);
    card.querySelector('[data-slot="requests"]').textContent = formatInteger(completedRequestCount(accountStatus));
    card.querySelector('[data-slot="successful-requests"]').textContent = formatInteger(accountStatus && accountStatus.successfulRequests);
    card.querySelector('[data-slot="failed-requests"]').textContent = formatInteger(accountStatus && accountStatus.failedRequests);
    card.querySelector('[data-slot="request-health"]').textContent = formatRequestHealth(accountStatus);
    const resetButton = card.querySelector('[data-action="reset"]');
    if (resetButton) {
      shell.setDisabledPreservingBusy(resetButton, !accountStatus);
      resetButton.title = accountStatus
        ? "不发送测试请求；唤醒后由下一次匹配的真实请求进行单次验证，成功后才恢复"
        : "此账号尚未保存";
    }
    const resumeButton = card.querySelector('[data-action="resume"]');
    if (resumeButton) {
      const canResume = Boolean(accountStatus && accountStatus.cooldownUntil &&
        accountStatus.cooldownReason === "upstream_failures" && accountStatus.upstreamFailures > 0);
      resumeButton.hidden = !canResume;
      shell.setDisabledPreservingBusy(resumeButton, !canResume);
      resumeButton.title = canResume ? "不清除失败状态；立即允许下一次匹配的真实请求进行单次验证" :
        "账号当前未因请求失败过多而暂时停用";
    }
    const clearStatisticsButton = card.querySelector('[data-action="clear-statistics"]');
    if (clearStatisticsButton) {
      shell.setDisabledPreservingBusy(clearStatisticsButton, !accountStatus);
      clearStatisticsButton.title = accountStatus ? "仅清空该账号的成功、失败和总请求统计" : "此账号尚未保存";
    }
  }

  function updateStatusUI() {
    const status = getStatus();
    const config = getConfig();
    const effective = status.effectiveStrategy || status.strategy || "priority";
    const configured = status.strategy || "priority";
    elements.summaryStrategy.textContent = strategyLabels[effective] || effective;
    elements.summaryAccounts.textContent = `${status.availableAccounts} / ${status.totalAccounts}`;
    elements.summaryLastAccount.textContent = status.lastRoutedAccountName || status.lastRoutedAccountId || "尚无请求";
    elements.summaryLastAccount.title = status.lastRoutedAccountId ? `账号 ID：${status.lastRoutedAccountId}` : "";
    const selected = config.strategy;
    const savedStrategyDiffers = selected !== configured;
    const configuredUsesBalance = configured === "highest_balance" || configured === "lowest_balance";
    const selectedUsesBalance = selected === "highest_balance" || selected === "lowest_balance";
    const usesQuotaReset = configured === "quota_reset" || selected === "quota_reset";
    let effectiveText = `服务当前实际执行：${strategyLabels[effective] || effective}。`;
    if (savedStrategyDiffers) effectiveText += ` 页面中选择的“${strategyLabels[selected] || selected}”尚未保存。`;
    if (configuredUsesBalance || selectedUsesBalance) {
      effectiveText += " 只有全部路由候选的账户、订阅与 API Key 可用余额都已验证且可比较时才执行余额策略；否则整体回退到最少使用优先，仅 API Key 或未知余额不会按 0 处理。账面余额仅用于排序，不代表模型请求当前可用。";
    }
    if (usesQuotaReset) {
      effectiveText += " OAuth 账号使用缓存的官方用量重置时间；登录 New API 的账号自动读取订阅重置时间，不登录的账号使用手工规则。没有重置时间的账号排在最后。";
    }
    shell.setTextIfChanged(elements.effectiveStrategyNotice, effectiveText);
    const hasFallback = configuredUsesBalance && (effective !== configured || status.fallbackReason);
    shell.setHiddenIfChanged(elements.fallbackWarning, !hasFallback);
    if (hasFallback) {
      const reason = fallbackReasonLabels[status.fallbackReason] || status.fallbackReason || "当前没有足够的已知余额数据";
      shell.setTextIfChanged(elements.fallbackWarning,
        `“${strategyLabels[configured] || configured}”当前无法直接决策，已实际回退为“${strategyLabels[effective] || effective}”。原因：${reason}。未知余额没有被当作 0。`);
    }
    config.accounts.forEach((account) => {
      const card = findCard(accountUIID(account));
      if (card) updateAccountStatus(card, account);
    });
  }

  function updateStrategyDescription() {
    elements.strategyHelp.textContent = strategyDescriptions[getConfig().strategy] || "";
    updateStatusUI();
  }

  function setStrategyValue() {
    elements.strategy.value = getConfig().strategy;
  }

  function clearFieldErrors(card) {
    card.querySelectorAll("[aria-invalid]").forEach((field) => field.removeAttribute("aria-invalid"));
    card.querySelectorAll(".field-error").forEach((error) => {
      error.hidden = true;
      error.textContent = "";
    });
  }

  function setFieldError(card, fieldName, message) {
    const field = card.querySelector(`[data-field="${fieldName}"]`);
    const error = card.querySelector(`[data-error="${fieldName}"]`);
    if (field) field.setAttribute("aria-invalid", "true");
    if (error) {
      error.textContent = message;
      error.hidden = false;
    }
  }

  function validateAccount(account, options) {
    const card = findCard(accountUIID(account));
    if (card) clearFieldErrors(card);
    const errors = validateAccountData(account, options);
    errors.forEach((error) => {
      if (card) setFieldError(card, error.field, error.message);
    });
    return errors.length && card ? card.querySelector(`[data-field="${errors[0].field}"]`) : null;
  }

  function validateAllAccounts() {
    let firstInvalid = null;
    getConfig().accounts.forEach((account) => {
      const invalid = validateAccount(account);
      firstInvalid = firstInvalid || invalid;
    });
    if (!firstInvalid) return getConfig().accounts;
    showView("accounts", { focus: false });
    const card = firstInvalid.closest(".account-card");
    shell.showGlobalError("部分账号配置不完整，请修正标出的字段。");
    if (card) {
      const account = getAccount(card.dataset.accountId);
      if (account) openAccountEditor(account);
    }
    firstInvalid.focus();
    return null;
  }

  function confirmHTTPRisk(urls) {
    if (!urls.length) return Promise.resolve(true);
    elements.httpRiskList.replaceChildren();
    urls.forEach((url) => {
      const item = document.createElement("li");
      item.textContent = url;
      elements.httpRiskList.appendChild(item);
    });
    return new Promise((resolve) => {
      const onClose = () => resolve(elements.httpRiskDialog.returnValue === "confirm");
      elements.httpRiskDialog.addEventListener("close", onClose, { once: true });
      if (typeof elements.httpRiskDialog.showModal === "function") {
        elements.httpRiskDialog.returnValue = "";
        elements.httpRiskDialog.showModal();
      } else {
        resolve(window.confirm("检测到明文 HTTP 地址。API Key、余额认证凭据、提示词、代码和响应可能被窃听。是否仍要继续？"));
      }
    });
  }

  function addAccount() {
    const account = createNewAccount();
    getConfig().accounts.push(account);
    shell.markDirty();
    renderAccounts();
    updateStatusUI();
    openAccountEditor(account);
    shell.announce("已添加新账号，请填写账号名称并选择上游平台");
  }

  function deleteAccount(account) {
    const name = account.name.trim() || "未命名账号";
    if (!window.confirm(`确认删除“${name}”吗？保存全部配置后才会从服务器删除。`)) return;
    const index = getAccountIndex(accountUIID(account));
    if (index < 0) return;
    const nextFocusAccount = getConfig().accounts[index + 1] || getConfig().accounts[index - 1] || null;
    const nextFocusID = nextFocusAccount ? accountUIID(nextFocusAccount) : "";
    closeAccountEditor(account);
    getConfig().accounts.splice(index, 1);
    shell.markDirty();
    renderAccounts();
    updateStatusUI();
    const nextCard = nextFocusID ? findCard(nextFocusID) : null;
    const nextButton = nextCard && nextCard.querySelector('[data-action="edit"]');
    if (nextButton) nextButton.focus();
    else elements.addAccountButton.focus();
    shell.announce(`已从待保存配置中删除 ${name}，当前有未保存更改`);
  }

  function moveAccount(account, direction, sourceAction) {
    const index = getAccountIndex(accountUIID(account));
    const target = index + direction;
    if (index < 0 || target < 0 || target >= getConfig().accounts.length) return;
    getConfig().accounts.splice(index, 1);
    getConfig().accounts.splice(target, 0, account);
    shell.markDirty();
    renderAccounts();
    updateStatusUI();
    const card = findCard(accountUIID(account));
    const focusTarget = card && card.querySelector(`[data-action="${sourceAction}"]`);
    if (focusTarget && !focusTarget.disabled) focusTarget.focus();
    else if (card) card.querySelector('[data-action="edit"]').focus();
    shell.announce(`${account.name || "未命名账号"} 已移动到第 ${target + 1} 位，当前有未保存更改`);
  }

  function openAccountEditor(account) {
    showView("accounts", { focus: false });
    const card = findCard(accountUIID(account));
    const editor = card && card.querySelector('[data-slot="editor"]');
    if (!editor || editor.open) return;
    if (typeof editor.showModal === "function") editor.showModal();
    else editor.setAttribute("open", "");
    const nameInput = editor.querySelector('[data-field="name"]');
    if (nameInput) nameInput.focus();
  }

  function closeAccountEditor(account) {
    const card = findCard(accountUIID(account));
    const editor = card && card.querySelector('[data-slot="editor"]');
    if (!editor || !editor.open) return;
    if (typeof editor.close === "function") editor.close();
    else editor.removeAttribute("open");
  }

  function handleAccountInput(event) {
    const field = event.target.dataset.field;
    const id = event.target.dataset.accountId;
    if (!field || !id) return;
    const account = getAccount(id);
    if (!account) return;
    const card = findCard(id);
    if (field === "authType" || field === "provider") clearFieldErrors(card);
    event.target.removeAttribute("aria-invalid");
    const relatedError = card.querySelector(`[data-error="${field}"]`);
    if (relatedError) {
      relatedError.hidden = true;
      relatedError.textContent = "";
    }
    if (field === "enabled" || field === "clearApiKey") {
      const keyError = card.querySelector('[data-error="apiKey"]');
      const keyField = card.querySelector('[data-field="apiKey"]');
      keyField.removeAttribute("aria-invalid");
      keyError.hidden = true;
      keyError.textContent = "";
    }
    if (["provider", "newApiAuthMode", "newApiUsername", "newApiUserId", "clearNewApiSecret"].includes(field)) {
      ["newApiUsername", "newApiUserId", "newApiSecret"].forEach((relatedField) => {
        const input = card.querySelector(`[data-field="${relatedField}"]`);
        const error = card.querySelector(`[data-error="${relatedField}"]`);
        input.removeAttribute("aria-invalid");
        error.hidden = true;
        error.textContent = "";
      });
    }
    if (["provider", "newApiAuthMode", "quotaResetPeriod", "quotaResetEvery", "quotaResetUnit", "quotaResetAnchorAt",
      "quotaResetTimezone"].includes(field)) {
      ["quotaResetPeriod", "quotaResetEvery", "quotaResetUnit", "quotaResetAnchorAt", "quotaResetTimezone"]
        .forEach((relatedField) => {
          const input = card.querySelector(`[data-field="${relatedField}"]`);
          const error = card.querySelector(`[data-error="${relatedField}"]`);
          input.removeAttribute("aria-invalid");
          error.hidden = true;
          error.textContent = "";
        });
    }
    if (field === "enabled") account.enabled = event.target.checked;
    else if (field === "disableCodexCredits") account.disableCodexCredits = event.target.checked;
    else if (field === "clearApiKey") {
      account.clearApiKey = event.target.checked;
      const keyInput = card.querySelector('[data-field="apiKey"]');
      if (account.clearApiKey) {
        account.apiKey = "";
        keyInput.value = "";
      }
      keyInput.disabled = account.authType !== "api_key" || account.clearApiKey;
    } else if (field === "clearNewApiSecret") {
      account.clearNewApiSecret = event.target.checked;
      if (account.clearNewApiSecret) {
        account.newApiSecret = "";
        card.querySelector('[data-field="newApiSecret"]').value = "";
      }
    } else if (field === "newApiUserId") account.newApiUserId = event.target.value === "" ? 0 : Number(event.target.value);
    else if (field === "quotaResetEvery") account.quotaResetEvery = event.target.value === "" ? 0 : Number(event.target.value);
    else {
      account[field] = event.target.value;
      if (field === "apiKey" && event.target.value) {
        account.clearApiKey = false;
        card.querySelector('[data-field="clearApiKey"]').checked = false;
      }
      if (field === "newApiSecret" && event.target.value) {
        account.clearNewApiSecret = false;
        card.querySelector('[data-field="clearNewApiSecret"]').checked = false;
      }
    }
    if (field === "name") {
      card.querySelector('[data-slot="name"]').textContent = account.name.trim() || "未命名账号";
      updateAccountControlLabels(card, account, getAccountIndex(id));
    }
    if (field === "baseUrl") card.querySelector('[data-slot="url"]').textContent = account.baseUrl.trim() || "尚未填写 URL";
    if (["authType", "provider", "baseUrl", "apiKey", "clearApiKey"].includes(field)) {
      invalidateAccountModels(account);
      updateAccountModels(card, account);
    }
    if (["newApiAuthMode", "newApiUsername", "newApiUserId", "clearNewApiSecret", "newApiSecret"].includes(field)) {
      updateNewAPIAuthFields(card, account);
    }
    updateAccountHTTPWarning(card, account);
    updateAccountStatus(card, account);
    shell.markDirty();
  }

  function handleAccountAction(event) {
    const button = event.target.closest("button[data-action]");
    if (!button || !elements.accountList.contains(button)) return;
    const account = getAccount(button.dataset.accountId);
    if (!account) return;
    const action = button.dataset.action;
    if (action === "edit") openAccountEditor(account);
    else if (action === "close-editor") closeAccountEditor(account);
    else if (action === "move-up") moveAccount(account, -1, "move-up");
    else if (action === "move-down") moveAccount(account, 1, "move-down");
    else if (action === "delete") deleteAccount(account);
    else onAccountAction(action, account, button);
  }

  function updateAccount(account) {
    if (getAccount(accountUIID(account)) !== account) return;
    const card = findCard(accountUIID(account));
    if (card) updateAccountStatus(card, account);
  }

  function init() {
    elements.strategy.addEventListener("change", () => {
      getConfig().strategy = elements.strategy.value;
      shell.markDirty();
      updateStrategyDescription();
    });
    elements.accountList.addEventListener("input", handleAccountInput);
    elements.accountList.addEventListener("change", handleAccountInput);
    elements.accountList.addEventListener("click", handleAccountAction);
    elements.addAccountButton.addEventListener("click", addAccount);
    elements.emptyAddButton.addEventListener("click", addAccount);
  }

  return {
    init,
    getAccount,
    getStatusAccount,
    findCard,
    renderAccounts,
    updateStatusUI,
    updateStrategyDescription,
    setStrategyValue,
    updateAccount,
    updateAccountModels,
    validateAccount,
    validateAllAccounts,
    confirmHTTPRisk,
    openAccountEditor,
    formatRequestHealth,
  };
}
