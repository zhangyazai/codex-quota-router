import {
  accountRuntimeConfigChanged,
  accountUIID,
  httpURLs,
  saveAccountPayload,
  savedAccountModelsReady,
} from "../domain/config-model.mjs";
import {
  balanceErrorText,
  balanceRefreshStatusValue,
  balanceStatusFailed,
  balanceStatusValue,
  formatBalance,
} from "./runtime-status.mjs";

export function createAccountRuntime({
  api,
  shell,
  getAccount,
  findCard,
  validateAccount,
  confirmHTTPRisk,
  updateAccountModels,
  setStatus,
  renderStatus,
  refreshStatus,
  showView,
}) {
  async function testAccountBalance(account, button) {
    showView("accounts", { focus: false });
    if (account.authType !== "api_key") {
      shell.announce("Codex OAuth 账号请使用“刷新用量”");
      return;
    }
    shell.clearGlobalError();
    const firstInvalid = validateAccount(account, { requireKey: true });
    if (firstInvalid) {
      firstInvalid.focus();
      shell.announce(`请先修正 ${account.name || "该账号"} 的配置`);
      return;
    }
    const urls = httpURLs([account]);
    if (!await confirmHTTPRisk(urls)) {
      shell.announce("已取消余额查询");
      return;
    }

    const card = findCard(accountUIID(account));
    const resultElement = card && card.querySelector('[data-slot="test-result"]');
    const restoreButton = shell.setButtonBusy(button, "正在查询…");
    if (resultElement) {
      resultElement.className = "test-result";
      resultElement.textContent = "正在查询当前填写内容对应的余额…";
    }
    try {
      const data = await api.request("/admin/balances/test", {
        method: "POST",
        body: JSON.stringify({
          accountId: account.id,
          candidate: saveAccountPayload(account),
          allowInsecureHttp: urls.length > 0,
        }),
      });
      const report = data.report && typeof data.report === "object"
        ? data.report
        : (Array.isArray(data.reports) && data.reports[0] && typeof data.reports[0] === "object" ? data.reports[0] : {});
      const reportBalance = report.balance && typeof report.balance === "object" ? report.balance : {};
      const responseBalance = data.balance && typeof data.balance === "object" ? data.balance : {};
      const balance = Object.assign({}, report, reportBalance, responseBalance);
      const refreshState = balanceRefreshStatusValue(balance);
      const partial = refreshState === "partial" && data.ok !== false;
      const failed = data.ok === false || (!partial &&
        (balanceStatusFailed(balanceStatusValue(balance)) || balanceStatusFailed(refreshState)));
      const displayBalance = Object.assign({}, balance, { retryable: false });
      const message = partial
        ? `余额查询部分成功：${formatBalance(displayBalance)}`
        : (failed ? `余额查询失败：${balanceErrorText(balance, false)}` : `余额查询成功：${formatBalance(displayBalance)}`);
      if (resultElement) {
        resultElement.className = failed ? "test-result danger" : "test-result success";
        resultElement.textContent = message;
      }
      shell.announce(`${account.name || "该账号"}：${message}`);
      if (account.id && !accountRuntimeConfigChanged(account)) await refreshStatus(true);
    } catch (error) {
      const errorData = error.data && typeof error.data === "object" ? error.data : {};
      const errorReport = errorData.report && typeof errorData.report === "object"
        ? errorData.report
        : (Array.isArray(errorData.reports) && errorData.reports[0] && typeof errorData.reports[0] === "object"
          ? errorData.reports[0]
          : errorData);
      const failure = `余额查询失败：${errorReport.errorCode || errorReport.errorStage
        ? balanceErrorText(errorReport, false)
        : error.message}`;
      if (resultElement) {
        resultElement.className = "test-result danger";
        resultElement.textContent = failure;
      }
      shell.announce(`${account.name || "该账号"} ${failure}`);
    } finally {
      restoreButton();
    }
  }

  async function refreshAccountModels(account, button, options) {
    const savedConfig = options && options.savedConfig === true;
    if (account.authType !== "api_key") return;
    if (!savedConfig) {
      showView("accounts", { focus: false });
      shell.clearGlobalError();
    }
    let urls = [];
    if (savedConfig) {
      if (getAccount(accountUIID(account)) !== account || account._modelsState !== "idle" ||
          !savedAccountModelsReady(account)) return;
    } else {
      const firstInvalid = validateAccount(account, { requireKey: true, skipNewAPIAuth: true });
      if (firstInvalid) {
        firstInvalid.focus();
        shell.announce(`请先修正 ${account.name || "该账号"} 的配置`);
        return;
      }
      urls = httpURLs([account]);
      if (!await confirmHTTPRisk(urls)) {
        shell.announce("已取消支持模型获取");
        return;
      }
    }

    const card = findCard(accountUIID(account));
    const resultElement = !savedConfig && card && card.querySelector('[data-slot="models-result"]');
    const restoreButton = button ? shell.setButtonBusy(button, "正在获取…") : () => {};
    const requestId = (account._modelsRequestId || 0) + 1;
    account._modelsRequestId = requestId;
    account._models = [];
    account._modelsError = "";
    account._modelsState = "loading";
    updateAccountModels(card, account);
    if (resultElement) {
      resultElement.className = "test-result";
      resultElement.textContent = "正在获取当前填写内容支持的模型…";
    }
    try {
      const payload = { accountId: account.id };
      if (!savedConfig) {
        payload.candidate = {
          id: account.id,
          name: account.name.trim(),
          provider: account.provider,
          baseUrl: account.baseUrl.trim(),
          apiKey: account.apiKey.trim(),
          clearApiKey: account.clearApiKey,
        };
        payload.allowInsecureHttp = urls.length > 0;
      }
      const data = await api.request("/admin/models", {
        method: "POST",
        body: JSON.stringify(payload),
      });
      if (getAccount(accountUIID(account)) !== account || account._modelsRequestId !== requestId) return;
      account._models = Array.isArray(data.models) ? data.models
        .filter((model) => typeof model === "string" && model.trim())
        .map((model) => model.trim()) : [];
      account._modelsState = account._models.length ? "ready" : "empty";
      updateAccountModels(findCard(accountUIID(account)), account);
      const message = account._models.length ? `支持模型已刷新，共 ${account._models.length} 个。` : "上游未返回模型。";
      if (resultElement) {
        resultElement.className = "test-result success";
        resultElement.textContent = message;
      }
      if (!savedConfig) shell.announce(`${account.name || "该账号"}：${message}`);
    } catch (error) {
      if (getAccount(accountUIID(account)) !== account || account._modelsRequestId !== requestId) return;
      account._models = [];
      account._modelsState = "error";
      account._modelsError = error.message;
      updateAccountModels(findCard(accountUIID(account)), account);
      const failure = `支持模型获取失败：${error.message}`;
      if (resultElement) {
        resultElement.className = "test-result danger";
        resultElement.textContent = failure;
      }
      if (!savedConfig) shell.announce(`${account.name || "该账号"} ${failure}`);
    } finally {
      restoreButton();
    }
  }

  function refreshSavedAccountModels(accounts) {
    const queue = accounts.filter((account) => account.authType === "api_key");
    const workers = [];
    for (let index = 0; index < Math.min(3, queue.length); index += 1) {
      workers.push((async () => {
        while (queue.length) await refreshAccountModels(queue.shift(), null, { savedConfig: true });
      })());
    }
    return Promise.all(workers);
  }

  async function resetAccount(account, button) {
    showView("accounts", { focus: false });
    shell.clearGlobalError();
    const card = findCard(accountUIID(account));
    const resultElement = card && card.querySelector('[data-slot="test-result"]');
    const restoreButton = shell.setButtonBusy(button, "正在唤醒…");
    if (resultElement) {
      resultElement.className = "test-result";
      resultElement.textContent = "正在允许下一次匹配的真实请求进行单次验证…";
    }
    try {
      const data = await api.request("/admin/accounts/reset", {
        method: "POST",
        body: JSON.stringify({ id: account.id }),
      });
      if (!data.status) throw new Error("服务未返回账号状态");
      setStatus(data.status);
      renderStatus();
      const message = data.message || `${account.name || "账号"} 已唤醒，等待下一次真实请求验证`;
      if (resultElement) {
        resultElement.className = data.pending === false ? "test-result success" : "test-result";
        resultElement.textContent = message;
      }
      shell.announce(message);
    } catch (error) {
      const errorData = error.data && typeof error.data === "object" ? error.data : {};
      if (errorData.status) {
        setStatus(errorData.status);
        renderStatus();
      }
      const failure = `唤醒失败：${String(errorData.message || error.message)}`;
      if (resultElement) {
        resultElement.className = "test-result danger";
        resultElement.textContent = failure;
      }
      shell.announce(failure);
    } finally {
      restoreButton();
    }
  }

  async function resumeAccount(account, button) {
    showView("accounts", { focus: false });
    shell.clearGlobalError();
    const card = findCard(accountUIID(account));
    const resultElement = card && card.querySelector('[data-slot="test-result"]');
    const restoreButton = shell.setButtonBusy(button, "正在唤醒…");
    try {
      const data = await api.request("/admin/accounts/resume", {
        method: "POST",
        body: JSON.stringify({ id: account.id }),
      });
      if (!data.status) throw new Error("服务未返回账号状态");
      setStatus(data.status);
      renderStatus();
      const message = data.message || `${account.name || "账号"} 已唤醒，下一次真实请求将进行单次验证`;
      if (resultElement) {
        resultElement.className = "test-result";
        resultElement.textContent = message;
      }
      shell.announce(message);
    } catch (error) {
      const failure = `恢复失败：${error.message}`;
      if (resultElement) {
        resultElement.className = "test-result danger";
        resultElement.textContent = failure;
      }
      shell.announce(failure);
    } finally {
      restoreButton();
    }
  }

  async function clearRequestStatistics(account, button) {
    showView("accounts", { focus: false });
    if (!account.id || !window.confirm(`确认清空“${account.name.trim() || "该账号"}”的请求统计吗？`)) return;
    shell.clearGlobalError();
    const restoreButton = shell.setButtonBusy(button, "正在清空…");
    try {
      const data = await api.request("/admin/accounts/statistics/clear", {
        method: "POST",
        body: JSON.stringify({ id: account.id }),
      });
      if (!data.status) throw new Error("服务未返回账号状态");
      setStatus(data.status);
      renderStatus();
      shell.announce(data.message || `${account.name || "账号"}的请求统计已清空`);
    } catch (error) {
      const failure = `清空请求统计失败：${error.message}`;
      shell.showGlobalError(failure);
      shell.announce("清空请求统计失败");
    } finally {
      restoreButton();
    }
  }

  return {
    testAccountBalance,
    refreshAccountModels,
    refreshSavedAccountModels,
    resetAccount,
    resumeAccount,
    clearRequestStatistics,
  };
}
