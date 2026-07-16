import {
  accountRuntimeConfigChanged,
  accountUIID,
  boundedPercent,
  formatOptionalDateTime,
  formatPercent,
  formatUsageDuration,
  validDateValue,
} from "../domain/config-model.mjs";

function codexUsageFor(accountStatus) {
  const usage = accountStatus && accountStatus.codexUsage && typeof accountStatus.codexUsage === "object"
    ? accountStatus.codexUsage
    : null;
  if (!usage) return null;
  const rateLimit = usage.rateLimit && typeof usage.rateLimit === "object" ? usage.rateLimit : {};
  const credits = usage.credits && typeof usage.credits === "object" ? usage.credits : {};
  return validDateValue(usage.checkedAt) ||
    (rateLimit.primaryWindow && rateLimit.primaryWindow.present === true) ||
    (rateLimit.secondaryWindow && rateLimit.secondaryWindow.present === true) || credits.present === true
    ? usage
    : null;
}

function formatCodexCredits(usage) {
  if (!usage) return "点数状态尚未获取。";
  const credits = usage.credits && typeof usage.credits === "object" ? usage.credits : null;
  if (!credits || credits.present !== true) return "点数状态未提供。";
  if (credits.unlimitedKnown === true && credits.unlimited === true) return "额外点数：无限。";
  if (credits.balanceKnown === true && typeof credits.balance === "number" && Number.isFinite(credits.balance)) {
    return `额外点数余额：${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 }).format(credits.balance)}。`;
  }
  if (credits.hasCreditsKnown === true) return credits.hasCredits ? "额外点数：可用。" : "额外点数：无可用余额。";
  return "额外点数状态未知。";
}

function planWindowExhausted(rateLimit) {
  return [rateLimit.primaryWindow, rateLimit.secondaryWindow]
    .some((windowData) => windowData && windowData.present === true && windowData.usedPercent >= 100);
}

function updateCodexUsageWindow(card, name, windowData, limitReached) {
  const present = windowData && windowData.present === true;
  const percent = card.querySelector(`[data-slot="${name}-percent"]`);
  const progress = card.querySelector(`[data-slot="${name}-progress"]`);
  const detail = card.querySelector(`[data-slot="${name}-detail"]`);
  const value = present ? boundedPercent(windowData.usedPercent) : 0;
  percent.textContent = present ? formatPercent(windowData.usedPercent) : "—";
  progress.value = value;
  progress.hidden = !present;
  progress.dataset.limitReached = String(limitReached === true && value >= 100);
  progress.setAttribute("aria-valuetext", present ? `${formatPercent(windowData.usedPercent)} 已使用` : "暂无数据");
  if (!present) {
    detail.textContent = "暂无数据";
    return;
  }
  const parts = [];
  const duration = formatUsageDuration(windowData.windowSeconds);
  if (duration) parts.push(`窗口 ${duration}`);
  if (validDateValue(windowData.resetAt)) {
    parts.push(`重置于 ${formatOptionalDateTime(windowData.resetAt, String(windowData.resetAt))}`);
  } else if (typeof windowData.resetAfterSeconds === "number" && windowData.resetAfterSeconds > 0) {
    parts.push(`${formatUsageDuration(windowData.resetAfterSeconds)}后重置`);
  }
  detail.textContent = parts.length ? parts.join(" · ") : "已获取用量，未提供重置时间";
}

function safeCodexAuthorizationURL(value) {
  try {
    const parsed = new URL(value);
    if (parsed.origin === "https://auth.openai.com" && parsed.pathname === "/oauth/authorize" &&
        !parsed.username && !parsed.password && !parsed.hash) {
      return parsed.href;
    }
  } catch (_error) {
  }
  return "";
}

export function createCodexOAuth({
  api,
  shell,
  getAccount,
  getStatusAccount,
  ensureAccountSaved,
  setStatus,
  renderStatus,
  renderAccount,
}) {
  function formatAccountSummary(account, accountStatus) {
    if (!account.codexAuthenticated) return "尚未登录 OpenAI";
    const usage = codexUsageFor(accountStatus);
    if (!usage) return "已登录，尚未刷新用量";
    const parts = [];
    const primary = usage.rateLimit && usage.rateLimit.primaryWindow;
    if (primary && primary.present === true) parts.push(`套餐额度已用 ${formatPercent(primary.usedPercent)}`);
    const credits = usage.credits;
    if (credits && credits.present === true) {
      if (credits.unlimitedKnown === true && credits.unlimited === true) parts.push("点数无限");
      else if (credits.balanceKnown === true && typeof credits.balance === "number" && Number.isFinite(credits.balance)) {
        parts.push(`点数 ${new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 2 }).format(credits.balance)}`);
      }
    }
    if (accountStatus && validDateValue(accountStatus.quotaResetAt)) {
      parts.push(`重置 ${formatOptionalDateTime(accountStatus.quotaResetAt, String(accountStatus.quotaResetAt))}`);
    }
    return parts.length ? parts.join(" · ") : "用量已刷新";
  }

  function accountBusy(account) {
    return account._codexAuthSaving === true || account._codexAuthStarting === true || Boolean(account._codexOAuthSession) ||
      account._codexUsageBusy === true || account._codexLogoutBusy === true;
  }

  function operationReady(account) {
    return account.authType === "codex_oauth" && Boolean(account.id) &&
      !accountRuntimeConfigChanged(account) && !accountBusy(account);
  }

  function updatePanel(card, account) {
    const panel = card.querySelector('[data-slot="codex-oauth-panel"]');
    if (!panel) return;
    const accountStatus = getStatusAccount(account.id);
    const usage = codexUsageFor(accountStatus);
    const rateLimit = usage && usage.rateLimit && typeof usage.rateLimit === "object" ? usage.rateLimit : {};
    const officialLimitReached = rateLimit.limitReached === true ||
      (rateLimit.allowedKnown === true && rateLimit.allowed === false);
    const protectedPlanExhausted = account.disableCodexCredits === true && planWindowExhausted(rateLimit);
    const usageExhausted = officialLimitReached || protectedPlanExhausted;
    const session = account._codexOAuthSession || null;
    const saving = account._codexAuthSaving === true;
    const starting = account._codexAuthStarting === true;
    const operationBusy = account._codexUsageBusy === true || account._codexLogoutBusy === true;
    const runtimeChanged = accountRuntimeConfigChanged(account);
    const savedReady = Boolean(account.id) && !runtimeChanged && account.authType === "codex_oauth";

    card.querySelector('[data-slot="codex-login-state"]').textContent = account.codexAuthenticated ? "已登录" : "未登录";
    card.querySelector('[data-slot="codex-email"]').textContent = account.codexEmail || "—";
    card.querySelector('[data-slot="codex-plan"]').textContent = (usage && usage.planType) || account.codexPlanType || "未知";
    card.querySelector('[data-slot="codex-expires"]').textContent = formatOptionalDateTime(account.codexExpiresAt, "—");
    card.querySelector('[data-slot="codex-reset-at"]').textContent =
      formatOptionalDateTime(accountStatus && accountStatus.quotaResetAt, "尚无数据");
    card.querySelector('[data-slot="codex-checked-at"]').textContent =
      formatOptionalDateTime(usage && usage.checkedAt, "尚未刷新");
    card.querySelector('[data-slot="codex-credits"]').textContent = formatCodexCredits(usage);
    updateCodexUsageWindow(card, "primary", rateLimit.primaryWindow, usageExhausted);
    updateCodexUsageWindow(card, "secondary", rateLimit.secondaryWindow, usageExhausted);

    const saveHint = panel.querySelector('[data-slot="codex-save-hint"]');
    if (saving) saveHint.textContent = "正在保存账号配置，随后会生成 OpenAI 授权链接…";
    else if (starting) saveHint.textContent = "账号配置已保存，正在生成 OpenAI 授权链接…";
    else if (!account.id || runtimeChanged || shell.isDirty()) saveHint.textContent = "点击登录会先自动保存账号配置，再生成 OpenAI 授权链接。";
    else if (!account.codexAuthenticated) saveHint.textContent = "登录后才会参与路由；OAuth 凭据不会在页面中回显。";
    else if (protectedPlanExhausted) saveHint.textContent = "套餐额度已达上限，已停止路由并禁止使用额外点数；额度重置并刷新后恢复。";
    else if (usageExhausted) saveHint.textContent = "当前额度已达上限；可刷新用量，或等待真实 Responses 请求在重置后恢复。";
    else saveHint.textContent = "页面展示缓存状态；路由不会为每个请求额外查询用量。";

    const oauthBox = panel.querySelector('[data-slot="codex-oauth-box"]');
    oauthBox.hidden = !session;
    if (session) {
      const authorizationURL = safeCodexAuthorizationURL(session.authorizationUrl);
      const authorizationLink = panel.querySelector('[data-slot="codex-authorization-link"]');
      const copyAuthorizationButton = panel.querySelector('[data-action="codex-copy-link"]');
      if (authorizationURL) {
        authorizationLink.href = authorizationURL;
        authorizationLink.removeAttribute("aria-disabled");
      } else {
        authorizationLink.removeAttribute("href");
        authorizationLink.setAttribute("aria-disabled", "true");
      }
      shell.setDisabledPreservingBusy(copyAuthorizationButton, !authorizationURL);
      panel.querySelector('[data-slot="codex-oauth-expiry"]').textContent =
        `登录会话有效期至 ${formatOptionalDateTime(session.expiresAt, "未知")}`;
      panel.querySelector('[data-slot="codex-oauth-status"]').textContent =
        session.message || "等待你在 OpenAI 官方页面完成登录…";
    }

    const result = panel.querySelector('[data-slot="codex-auth-result"]');
    result.textContent = account._codexAuthMessage || "";
    result.className = account._codexAuthMessage
      ? `test-result ${account._codexAuthMessageKind === "danger" ? "danger" : "success"}`
      : "test-result";

    const loginButton = panel.querySelector('[data-action="codex-login"]');
    const usageButton = card.querySelector('[data-action="codex-usage"]');
    const logoutButton = panel.querySelector('[data-action="codex-logout"]');
    loginButton.textContent = session ? "正在等待登录…" : (saving ? "正在保存账号…" : (starting ? "正在生成登录链接…" :
      (account.codexAuthenticated ? "重新登录 OpenAI" : "登录 OpenAI")));
    loginButton.disabled = saving || starting || Boolean(session) || operationBusy;
    if (saving || starting || session) loginButton.setAttribute("aria-busy", "true");
    else loginButton.removeAttribute("aria-busy");
    loginButton.title = "自动保存账号配置并生成 OpenAI 官方登录链接";
    shell.setDisabledPreservingBusy(usageButton,
      !savedReady || !account.codexAuthenticated || saving || starting || Boolean(session) || account._codexLogoutBusy === true);
    usageButton.title = account.codexAuthenticated ? "刷新官方用量并同步额度冷却状态" : "请先登录 OpenAI";
    logoutButton.hidden = !account.codexAuthenticated;
    shell.setDisabledPreservingBusy(logoutButton,
      !savedReady || saving || starting || Boolean(session) || account._codexUsageBusy === true);
  }

  function findPublicAccount(publicConfig, id) {
    const accounts = publicConfig && Array.isArray(publicConfig.accounts) ? publicConfig.accounts : [];
    const target = String(id || "");
    return accounts.find((account) => String(account.id || "") === target) || null;
  }

  function mergeAccountMetadata(account, publicConfig) {
    const saved = findPublicAccount(publicConfig, account.id);
    if (!saved) return false;
    account.codexAuthenticated = saved.codexAuthenticated === true;
    account.codexEmail = typeof saved.codexEmail === "string" ? saved.codexEmail : "";
    account.codexPlanType = typeof saved.codexPlanType === "string" ? saved.codexPlanType : "";
    account.codexExpiresAt = typeof saved.codexExpiresAt === "string" ? saved.codexExpiresAt : "";
    return true;
  }

  function updateAccountUI(account) {
    if (getAccount(accountUIID(account)) === account) renderAccount(account);
  }

  function pollSeconds(value, fallback) {
    const seconds = typeof value === "number" && Number.isFinite(value) ? value : fallback;
    return Math.max(1, Math.min(60, Math.ceil(seconds || 5)));
  }

  function waitForPoll(seconds) {
    return new Promise((resolve) => window.setTimeout(resolve, pollSeconds(seconds, 5) * 1000));
  }

  function finishBrowserAuth(account, kind, message) {
    if (getAccount(accountUIID(account)) !== account) return;
    account._codexOAuthSession = null;
    account._codexAuthSaving = false;
    account._codexAuthStarting = false;
    account._codexAuthMessageKind = kind;
    account._codexAuthMessage = message;
    updateAccountUI(account);
    shell.announce(message);
  }

  async function pollBrowserAuth(account, session) {
    let delaySeconds = session.pollIntervalSeconds;
    while (true) {
      await waitForPoll(delaySeconds);
      if (!shell.isReady() || getAccount(accountUIID(account)) !== account || account._codexOAuthSession !== session) return;
      if (validDateValue(session.expiresAt) && Date.now() >= new Date(session.expiresAt).getTime()) {
        finishBrowserAuth(account, "danger", "OpenAI 登录会话已过期，请重新点击登录 OpenAI");
        return;
      }
      let data;
      try {
        data = await api.request("/admin/accounts/codex/oauth/status", {
          method: "POST",
          body: JSON.stringify({ sessionId: session.sessionId }),
        });
      } catch (error) {
        if (error.status >= 400 && error.status < 500) {
          finishBrowserAuth(account, "danger", `OpenAI 登录状态确认失败：${error.message}；请重新点击登录 OpenAI`);
          return;
        }
        delaySeconds = Math.min(60, Math.max(5, delaySeconds * 2));
        session.message = `登录状态查询暂时失败，将在 ${delaySeconds} 秒后重试；授权链接仍可继续使用。`;
        updateAccountUI(account);
        continue;
      }
      if (getAccount(accountUIID(account)) !== account || account._codexOAuthSession !== session) return;
      const oauthStatus = typeof data.oauthStatus === "string" ? data.oauthStatus : "";
      if (oauthStatus === "pending") {
        delaySeconds = pollSeconds(data.pollIntervalSeconds ?? data.retryAfterSeconds, delaySeconds);
        if (validDateValue(data.expiresAt)) session.expiresAt = data.expiresAt;
        session.message = `等待你在 OpenAI 官方页面完成登录，本页将在 ${delaySeconds} 秒后继续确认…`;
        updateAccountUI(account);
        continue;
      }
      if (oauthStatus !== "complete") {
        finishBrowserAuth(account, "danger", "OpenAI 登录返回了未知状态，请重新点击登录 OpenAI");
        return;
      }
      if (!data.config || !mergeAccountMetadata(account, data.config)) {
        account.codexAuthenticated = true;
        account.codexEmail = typeof data.email === "string" ? data.email : account.codexEmail;
        account.codexPlanType = typeof data.planType === "string" ? data.planType : account.codexPlanType;
      }
      const returnedStatus = data.routerStatus || (data.status && typeof data.status === "object" ? data.status : null);
      if (returnedStatus) setStatus(returnedStatus);
      finishBrowserAuth(account, "success", `OpenAI 登录成功${account.codexEmail ? `：${account.codexEmail}` : ""}`);
      renderStatus();
      return;
    }
  }

  async function startBrowserAuth(account) {
    if (account.authType !== "codex_oauth" || accountBusy(account)) return;
    shell.clearGlobalError();
    account._codexAuthMessage = "";
    let activeAccount = account;
    try {
      if (!account.id || accountRuntimeConfigChanged(account) || shell.isDirty()) {
        account._codexAuthSaving = true;
        updateAccountUI(account);
        const savedAccount = await ensureAccountSaved(account);
        account._codexAuthSaving = false;
        if (!savedAccount) {
          account._codexAuthMessageKind = "danger";
          account._codexAuthMessage = "账号配置尚未保存，未启动 OpenAI 登录";
          updateAccountUI(account);
          shell.announce(account._codexAuthMessage);
          return;
        }
        activeAccount = savedAccount;
      }
      if (!operationReady(activeAccount)) {
        activeAccount._codexAuthMessageKind = "danger";
        activeAccount._codexAuthMessage = "账号配置尚未就绪，无法启动 OpenAI 登录";
        updateAccountUI(activeAccount);
        shell.announce(activeAccount._codexAuthMessage);
        return;
      }
      activeAccount._codexAuthStarting = true;
      activeAccount._codexAuthMessage = "";
      updateAccountUI(activeAccount);
      const data = await api.request("/admin/accounts/codex/oauth/start", {
        method: "POST",
        body: JSON.stringify({ accountId: activeAccount.id }),
      });
      if (getAccount(accountUIID(activeAccount)) !== activeAccount || activeAccount._codexAuthStarting !== true) return;
      const authorizationURL = safeCodexAuthorizationURL(data.authorizationUrl);
      if (!data.sessionId || !authorizationURL || !validDateValue(data.expiresAt)) {
        throw new Error("服务返回的浏览器登录信息不完整或登录地址不安全");
      }
      const session = {
        sessionId: String(data.sessionId),
        authorizationUrl: authorizationURL,
        expiresAt: data.expiresAt,
        pollIntervalSeconds: pollSeconds(data.pollIntervalSeconds, 5),
        message: "授权链接已生成，请点击“打开 OpenAI 登录页面”；完成登录后本页会自动确认结果。",
      };
      activeAccount._codexOAuthSession = session;
      activeAccount._codexAuthStarting = false;
      updateAccountUI(activeAccount);
      shell.announce("OpenAI 授权链接已生成");
      pollBrowserAuth(activeAccount, session).catch((error) => {
        if (getAccount(accountUIID(activeAccount)) === activeAccount && activeAccount._codexOAuthSession === session) {
          finishBrowserAuth(activeAccount, "danger", `OpenAI 登录失败：${error.message}；请重新点击登录 OpenAI`);
        }
      });
    } catch (error) {
      account._codexAuthSaving = false;
      if (getAccount(accountUIID(activeAccount)) !== activeAccount) return;
      activeAccount._codexAuthStarting = false;
      activeAccount._codexAuthMessageKind = "danger";
      activeAccount._codexAuthMessage = `无法启动 OpenAI 登录：${error.message}`;
      updateAccountUI(activeAccount);
      shell.announce(activeAccount._codexAuthMessage);
    }
  }

  async function copyAuthorizationLink(account, button) {
    const session = account._codexOAuthSession || null;
    const authorizationURL = safeCodexAuthorizationURL(session && session.authorizationUrl);
    if (!authorizationURL) return;
    const restoreButton = shell.setButtonBusy(button, "正在复制…");
    try {
      await shell.copyText(authorizationURL);
      shell.announce("OpenAI 授权链接已复制");
    } catch (error) {
      shell.showGlobalError(`复制授权链接失败：${error.message}`);
      shell.announce("复制授权链接失败");
    } finally {
      restoreButton();
    }
  }

  function usageErrorText(code) {
    const labels = {
      usage_unavailable: "用量服务不可用",
      authentication_failed: "身份验证失败，请重新登录",
      refresh_failed: "OpenAI 用量查询失败",
      canceled: "用量查询已取消",
    };
    return labels[code] || code || "用量刷新失败";
  }

  async function refreshUsage(account, button) {
    if (!operationReady(account) || !account.codexAuthenticated) {
      account._codexAuthMessageKind = "danger";
      account._codexAuthMessage = account.codexAuthenticated ? "请先保存全部配置" : "请先登录 OpenAI";
      updateAccountUI(account);
      shell.announce(account._codexAuthMessage);
      return;
    }
    shell.clearGlobalError();
    const restoreButton = shell.setButtonBusy(button, "正在刷新…");
    account._codexUsageBusy = true;
    updateAccountUI(account);
    try {
      const data = await api.request("/admin/accounts/codex/usage", {
        method: "POST",
        body: JSON.stringify({ accountIds: [account.id] }),
      });
      if (getAccount(accountUIID(account)) !== account) return;
      if (data.status) setStatus(data.status);
      const reports = Array.isArray(data.reports) ? data.reports : [];
      const report = reports.find((item) => item && String(item.accountId || "") === String(account.id));
      if (data.ok === false || !report || report.ok !== true) {
        throw new Error(usageErrorText(report && report.error));
      }
      try {
        const publicConfig = await api.request("/admin/config");
        if (getAccount(accountUIID(account)) !== account) return;
        mergeAccountMetadata(account, publicConfig);
      } catch (_metadataError) {
      }
      account._codexAuthMessageKind = "success";
      account._codexAuthMessage = "Codex 用量已刷新";
      renderStatus();
      shell.announce(`${account.name || "该账号"} 用量已刷新`);
    } catch (error) {
      if (getAccount(accountUIID(account)) !== account) return;
      account._codexAuthMessageKind = "danger";
      account._codexAuthMessage = `用量刷新失败：${error.message}`;
      shell.announce(account._codexAuthMessage);
    } finally {
      account._codexUsageBusy = false;
      restoreButton();
      updateAccountUI(account);
    }
  }

  async function logout(account, button) {
    if (!operationReady(account) || !account.codexAuthenticated) return;
    if (!window.confirm(`确认退出“${account.name.trim() || "该账号"}”的 OpenAI 登录吗？退出后该账号将停止参与路由。`)) return;
    shell.clearGlobalError();
    const restoreButton = shell.setButtonBusy(button, "正在退出…");
    account._codexLogoutBusy = true;
    updateAccountUI(account);
    try {
      const data = await api.request("/admin/accounts/codex/logout", {
        method: "POST",
        body: JSON.stringify({ accountId: account.id }),
      });
      if (getAccount(accountUIID(account)) !== account) return;
      if (data.config) mergeAccountMetadata(account, data.config);
      else {
        account.codexAuthenticated = false;
        account.codexEmail = "";
        account.codexPlanType = "";
        account.codexExpiresAt = "";
      }
      if (data.status) setStatus(data.status);
      account._codexAuthMessageKind = "success";
      account._codexAuthMessage = "已退出 OpenAI 账号";
      renderStatus();
      shell.announce(account._codexAuthMessage);
    } catch (error) {
      if (getAccount(accountUIID(account)) !== account) return;
      account._codexAuthMessageKind = "danger";
      account._codexAuthMessage = `退出 OpenAI 账号失败：${error.message}`;
      shell.announce(account._codexAuthMessage);
    } finally {
      account._codexLogoutBusy = false;
      restoreButton();
      updateAccountUI(account);
    }
  }

  return {
    formatAccountSummary,
    accountBusy,
    updatePanel,
    startBrowserAuth,
    copyAuthorizationLink,
    refreshUsage,
    logout,
  };
}
