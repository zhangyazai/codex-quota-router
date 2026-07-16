import { createApiClient } from "./core/api-client.mjs";
import { createAppShell } from "./core/app-shell.mjs";
import {
  createEmptyConfig,
  createEmptyStatus,
  httpURLs,
  normalizeConfig,
  normalizeStatus,
  saveAccountPayload,
} from "./domain/config-model.mjs";
import { createAccessFeature } from "./features/access.mjs";
import { createAccountRuntime } from "./features/account-runtime.mjs";
import { createAccountsView } from "./features/accounts-view.mjs";
import { createCodexOAuth } from "./features/codex-oauth.mjs";
import { createRuntimeStatus, formatBalance } from "./features/runtime-status.mjs";

const state = {
  config: createEmptyConfig(),
  status: createEmptyStatus(),
};
const api = createApiClient();
const shell = createAppShell();
const getConfig = () => state.config;
const getStatus = () => state.status;
const setConfig = (value) => {
  state.config = normalizeConfig(value);
};
const setStatus = (value) => {
  state.status = normalizeStatus(value);
};

let accountsView;
let accountRuntime;

const runtimeStatus = createRuntimeStatus({
  api,
  shell,
  getStatus,
  setStatus,
  findAccount: (id) => accountsView ? accountsView.getAccount(id) : null,
  renderStatus: () => accountsView && accountsView.updateStatusUI(),
});

const codex = createCodexOAuth({
  api,
  shell,
  getAccount: (id) => accountsView ? accountsView.getAccount(id) : null,
  getStatusAccount: (id) => accountsView ? accountsView.getStatusAccount(id) : null,
  ensureAccountSaved: saveAccountForOAuth,
  setStatus,
  renderStatus: () => accountsView && accountsView.updateStatusUI(),
  renderAccount: (account) => accountsView && accountsView.updateAccount(account),
});

accountsView = createAccountsView({
  shell,
  getConfig,
  getStatus,
  codex,
  formatBalance,
  showView: shell.showView,
  onAccountAction(action, account, button) {
    if (action === "test-balance") accountRuntime.testAccountBalance(account, button);
    else if (action === "models") accountRuntime.refreshAccountModels(account, button);
    else if (action === "resume") accountRuntime.resumeAccount(account, button);
    else if (action === "reset") accountRuntime.resetAccount(account, button);
    else if (action === "clear-statistics") accountRuntime.clearRequestStatistics(account, button);
    else if (action === "codex-login") codex.startBrowserAuth(account);
    else if (action === "codex-copy-link") codex.copyAuthorizationLink(account, button);
    else if (action === "codex-usage") codex.refreshUsage(account, button);
    else if (action === "codex-logout") codex.logout(account, button);
  },
});

accountRuntime = createAccountRuntime({
  api,
  shell,
  getAccount: accountsView.getAccount,
  findCard: accountsView.findCard,
  validateAccount: accountsView.validateAccount,
  confirmHTTPRisk: accountsView.confirmHTTPRisk,
  updateAccountModels: accountsView.updateAccountModels,
  setStatus,
  renderStatus: accountsView.updateStatusUI,
  refreshStatus: runtimeStatus.refreshStatus,
  showView: shell.showView,
});

const access = createAccessFeature({
  api,
  shell,
  getConfig,
  setConfig,
  showView: shell.showView,
});

async function loadBootstrap(options) {
  const opts = options || {};
  const wasReady = shell.isReady();
  let failure = "";
  shell.clearGlobalError();
  shell.setBusy(true);
  shell.setServiceState("loading", "正在连接");
  try {
    const data = await api.request("/admin/bootstrap");
    api.setCsrfToken(data.csrfToken);
    shell.setTextIfChanged(document.getElementById("appVersion"), data.version ? ` · v${data.version}` : "");
    setConfig(data.config);
    setStatus(data.status);
    shell.setReady(true);
    accountsView.setStrategyValue();
    access.syncSecurityFields();
    access.resetPagination();
    await access.loadGatewayTokens();
    accountsView.renderAccounts();
    accountsView.updateStrategyDescription();
    shell.markClean();
    shell.setServiceState("online", "服务在线");
    shell.elements.logoutButton.hidden = window.location.protocol !== "https:" && !getConfig().allowPublicAccess;
    accountRuntime.refreshSavedAccountModels(getConfig().accounts);
  } catch (error) {
    shell.setReady(wasReady);
    if (error.status === 401) {
      shell.setServiceState("loading", "需要登录");
      shell.showLoginDialog("");
    } else {
      shell.setServiceState("offline", "连接失败");
      failure = `无法加载管理配置：${error.message}`;
    }
  } finally {
    shell.setBusy(false);
    access.syncControls();
  }
  if (failure) shell.showGlobalError(failure);
  else if (opts.announce) shell.announce("已重新加载服务器配置");
}

async function saveAll(options) {
  const opts = options || {};
  shell.clearGlobalError();
  const pendingOAuth = getConfig().accounts.find((account) =>
    account !== opts.ignorePendingOAuthAccount && codex.accountBusy(account));
  if (pendingOAuth) {
    shell.showView("accounts", { focus: false });
    shell.showGlobalError(`请等待“${pendingOAuth.name.trim() || "未命名账号"}”当前的 OpenAI 操作完成，再保存配置。`);
    accountsView.openAccountEditor(pendingOAuth);
    shell.announce("存在尚未完成的 OpenAI 操作");
    return false;
  }
  const accounts = accountsView.validateAllAccounts();
  if (!accounts || !access.validateSecuritySettings()) return false;
  const insecureURLs = httpURLs(accounts);
  const restoreButton = shell.setButtonBusy(shell.elements.saveButton, "正在保存…");
  let outcome = "";
  let failure = "";
  shell.setBusy(true);
  try {
    const data = await api.request("/admin/config", {
      method: "PUT",
      body: JSON.stringify(Object.assign({
        strategy: getConfig().strategy,
        allowInsecureHttp: insecureURLs.length > 0,
        accounts: accounts.map(saveAccountPayload),
      }, access.securityPayload())),
    });
    setConfig(data.config);
    setStatus(data.status);
    accountsView.setStrategyValue();
    access.syncSecurityFields();
    await access.loadGatewayTokens();
    accountsView.renderAccounts();
    accountsView.updateStrategyDescription();
    shell.markClean();
    shell.setServiceState("online", "服务在线");
    accountRuntime.refreshSavedAccountModels(getConfig().accounts);
    outcome = data.message || "全部配置已保存";
  } catch (error) {
    failure = `保存失败：${error.message}`;
  } finally {
    shell.setBusy(false);
    restoreButton();
    access.syncControls();
  }
  if (failure) {
    shell.showGlobalError(failure);
    shell.announce("保存失败");
    return false;
  }
  if (opts.refreshBalances === false) {
    shell.announce(outcome);
    return true;
  }
  shell.setTextIfChanged(document.getElementById("balanceRefreshResult"), `${outcome}，正在查询余额…`);
  shell.announce(`${outcome}，正在查询余额`);
  await runtimeStatus.refreshBalances({ trigger: "save" });
  return true;
}

async function saveAccountForOAuth(account) {
  const accountIndex = getConfig().accounts.indexOf(account);
  if (accountIndex < 0) return null;
  const saved = await saveAll({
    ignorePendingOAuthAccount: account,
    refreshBalances: false,
  });
  if (!saved) return null;
  const savedAccount = getConfig().accounts[accountIndex] || null;
  if (!savedAccount || !savedAccount.id || savedAccount.authType !== "codex_oauth") return null;
  accountsView.openAccountEditor(savedAccount);
  return savedAccount;
}

function initAuthentication() {
  shell.elements.loginForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    shell.elements.loginError.hidden = true;
    const restoreButton = shell.setButtonBusy(shell.elements.loginButton, "正在登录…");
    try {
      await api.request("/admin/login", {
        method: "POST",
        body: JSON.stringify({ password: shell.elements.loginPassword.value }),
      });
      shell.elements.loginDialog.close();
      await loadBootstrap();
    } catch (error) {
      shell.showLoginDialog(error.message);
    } finally {
      restoreButton();
    }
  });

  shell.elements.logoutButton.addEventListener("click", async () => {
    try {
      await api.request("/admin/logout", { method: "POST", body: "{}" });
    } catch (_error) {
    }
    shell.setReady(false);
    api.clearCsrfToken();
    shell.showLoginDialog("");
  });
}

function initPageLifecycle() {
  shell.elements.saveButton.addEventListener("click", () => saveAll());
  shell.elements.reloadButton.addEventListener("click", () => {
    if (shell.isDirty() && !window.confirm("放弃所有未保存更改并重新加载服务器配置吗？")) return;
    loadBootstrap({ announce: true }).then(() => {
      if (shell.isReady() && !document.hidden) runtimeStatus.refreshBalances({ trigger: "poll" });
    });
  });
  window.addEventListener("beforeunload", (event) => {
    if (!shell.isDirty()) return;
    event.preventDefault();
    event.returnValue = "";
  });
}

shell.initUI();
accountsView.init();
access.init();
runtimeStatus.init();
initAuthentication();
initPageLifecycle();
shell.updatePageInteractivity();
loadBootstrap().then(() => {
  if (shell.isReady() && !document.hidden) runtimeStatus.refreshBalances({ trigger: "poll" });
});
runtimeStatus.startStatusPoll();
