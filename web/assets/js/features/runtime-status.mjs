const balanceStateLabels = {
  unknown: "未知（不按 0 计算）",
  unsupported: "上游不提供余额",
  unavailable: "余额获取失败",
  error: "余额获取失败",
  auth_error: "账号余额认证失败（请检查认证信息）",
  not_checked: "未知（尚未刷新）",
  pending: "正在刷新",
};

const balanceErrorStageLabels = {
  token_usage: "Token 额度接口",
  token_quota: "Token 额度接口",
  account_login: "用户登录",
  account_auth: "用户认证",
  account_quota: "用户钱包余额接口",
  account_subscription: "用户订阅余额接口",
  quota_metadata: "站点额度设置",
  status: "站点额度设置",
  dashboard_subscription: "兼容账单订阅接口",
  dashboard_usage: "兼容账单用量接口",
  conversion: "额度换算",
  status_conversion: "额度换算",
  dashboard: "兼容账单接口",
  account: "账户可用余额",
  network: "网络请求",
  timeout: "余额查询",
};

const apiKeyBalanceWarning = "未登录 New API 账户，无法核对账户及订阅余额，显示值可能不准确，余额策略可能无法按预期生效";

const balanceErrorCodeLabels = {
  auth_failed: "认证失败，请检查用户名、密码、Access Token 与用户 ID",
  unauthorized: "认证失败，请检查用户名、密码、Access Token 与用户 ID",
  invalid_credentials: "认证失败，请检查用户名、密码、Access Token 与用户 ID",
  api_key_unauthorized: "API Key 无效或无权读取 Token 额度",
  account_unauthorized: "用户认证失败，请检查用户名、密码、Access Token 与用户 ID",
  access_token_unauthorized: "用户 Access Token 无效或已失效",
  user_id_required: "上游要求提供 New API 用户 ID",
  user_id_mismatch: "New API 用户 ID 与 Access Token 不匹配",
  two_factor_required: "账号要求两步验证，请改用用户 Access Token",
  timeout: "查询超时",
  deadline_exceeded: "查询超时",
  network_error: "网络连接失败",
  rate_limited: "上游限流",
  upstream_unavailable: "上游暂不可用",
  upstream_rejected: "上游拒绝余额查询",
  upstream_error: "上游接口返回错误",
  invalid_response: "上游响应格式无效",
  missing_quota: "上游响应缺少余额字段",
  canceled: "查询已取消",
  unsupported: "上游不支持此余额查询",
};

export function balanceStatusValue(balance) {
  if (!balance || typeof balance !== "object") return "unknown";
  if (typeof balance.status === "string" && balance.status) return balance.status;
  return typeof balance.state === "string" && balance.state ? balance.state : "unknown";
}

export function balanceRefreshStatusValue(balance) {
  if (!balance || typeof balance !== "object") return "";
  return typeof balance.refreshStatus === "string" ? balance.refreshStatus : "";
}

export function balanceStatusFailed(value) {
  return value === "error" || value === "failed" || value === "unavailable" ||
    value === "auth_error" || value === "timeout" || value === "partial" || value === "canceled";
}

export function balanceErrorText(balance, retryScheduled = true) {
  const state = balanceStatusValue(balance);
  const refreshState = balanceRefreshStatusValue(balance);
  const stage = typeof balance?.errorStage === "string" ? balance.errorStage : "";
  const code = typeof balance?.errorCode === "string" ? balance.errorCode : "";
  const stageLabel = balanceErrorStageLabels[stage] || "";
  let detail = balanceErrorCodeLabels[code] || "";
  if (!detail && (state === "auth_error" || stage === "account_auth" || stage === "account_login")) {
    detail = balanceStateLabels.auth_error;
  }
  if (!detail && (state === "unsupported" || refreshState === "unsupported")) {
    detail = balanceStateLabels.unsupported;
  }
  if (!detail && (state === "pending" || refreshState === "pending")) {
    return balanceStateLabels.pending;
  }
  if (!detail && refreshState === "partial") detail = "部分余额来源查询失败";
  if (!detail && balanceStatusFailed(refreshState)) detail = "余额刷新失败";
  if (!detail) detail = balanceStateLabels[state] || "余额获取失败";
  let result = stageLabel ? `${stageLabel}：${detail}` : detail;
  if (retryScheduled !== false && balance?.retryable === true && balanceStatusFailed(refreshState || state)) {
    result += "，将自动重试";
  }
  return result;
}

function formatBalanceRefreshWarning(result, balance) {
  const refreshState = balanceRefreshStatusValue(balance);
  if (!balanceStatusFailed(refreshState)) return result;
  if (refreshState === "partial" && (balance.scope === "token_only" || balance.scope === "account_only")) {
    return `${result}（本次仅取得部分余额来源：${balanceErrorText(balance)}）`;
  }
  return `${result}（最近刷新失败：${balanceErrorText(balance)}；显示上次成功结果）`;
}

export function formatBalance(balance) {
  if (!balance || typeof balance !== "object") return "未知（尚无数据）";
  const state = balanceStatusValue(balance);
  const refreshState = balanceRefreshStatusValue(balance);
  if (balanceStatusFailed(state) ||
      ((state === "unknown" || state === "not_checked") && balanceStatusFailed(refreshState))) {
    return balanceErrorText(balance);
  }
  if (["unknown", "not_checked", "unsupported", "unavailable", "error", "auth_error", "pending"].includes(state)) {
    return state === "unsupported" ? balanceErrorText(balance) : (balanceStateLabels[state] || "未知（不按 0 计算）");
  }
  if (balance.unlimited === true && balance.limitedBy !== "account") {
    if (balance.scope === "token_only") {
      return formatBalanceRefreshWarning(`API Key 无限（${apiKeyBalanceWarning}）`, balance);
    }
    return formatBalanceRefreshWarning(`无限${balance.displayLabel ? ` · ${String(balance.displayLabel)}` : ""}`, balance);
  }
  let amount = balance.remaining;
  if (amount === null || typeof amount === "undefined") amount = balance.amount;
  if (amount === null || typeof amount === "undefined" || amount === "") return "未知（不按 0 计算）";
  const remaining = typeof amount === "number" && Number.isFinite(amount)
    ? new Intl.NumberFormat("zh-CN", { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(amount)
    : String(amount);
  const unit = balance.displayLabel || (balance.unit === "display_unit" ? "" : balance.unit);
  let result = unit ? `${remaining} ${String(unit)}` : remaining;
  if (balance.scope === "token_only") result += `（仅 API Key 额度；${apiKeyBalanceWarning}）`;
  else if (balance.scope === "account_only") result += "（仅账户可用余额，API Key 额度未验证）";
  else if (balance.limitedBy === "account") result += "（账户可用余额限制）";
  else if (balance.limitedBy === "token") result += "（API Key 额度限制）";
  if (balance.fresh === false && balance.updatedAt) result += "（已过期）";
  return formatBalanceRefreshWarning(result, balance);
}

function numericCount(source, names) {
  if (!source || typeof source !== "object") return null;
  for (const name of names) {
    const value = source[name];
    if (typeof value === "number" && Number.isFinite(value) && value >= 0) return Math.floor(value);
  }
  return null;
}

function balanceReportView(report) {
  const source = report && typeof report === "object" ? report : {};
  const balance = source.balance && typeof source.balance === "object" ? source.balance : {};
  return Object.assign({}, source, balance);
}

function balanceReportState(report) {
  const view = balanceReportView(report);
  const state = balanceStatusValue(view);
  if (balanceStatusFailed(state) || state === "unsupported") return state;
  return balanceRefreshStatusValue(view) || state;
}

function balanceReportFailed(report) {
  return balanceStatusFailed(balanceReportState(report));
}

function balanceReports(data) {
  const payload = data && typeof data === "object" ? data : {};
  if (Array.isArray(payload.reports)) return payload.reports;
  if (Array.isArray(payload.results)) return payload.results;
  if (!payload.status || !Array.isArray(payload.status.accounts)) return [];
  return payload.status.accounts.map((account) => Object.assign({
    accountId: account && account.id,
    accountName: account && account.name,
  }, account && account.balance && typeof account.balance === "object" ? account.balance : {}));
}

export function createRuntimeStatus({ api, shell, getStatus, setStatus, findAccount, renderStatus }) {
  const elements = {
    refreshBalancesButton: document.getElementById("refreshBalancesButton"),
    balanceRefreshResult: document.getElementById("balanceRefreshResult"),
  };
  let pollTimer = 0;
  let balancePollTimer = 0;
  let balanceRefreshPromise = null;
  let balanceRetryAttempt = 0;
  let balanceRetryAccountIds = [];
  const balanceRetryDelays = [5000, 20000];
  let statusRefreshPromise = null;
  const balancePollInterval = 60000;

  function balanceReportName(report) {
    const source = report && typeof report === "object" ? report : {};
    if (typeof source.accountName === "string" && source.accountName.trim()) return source.accountName.trim();
    if (typeof source.name === "string" && source.name.trim()) return source.name.trim();
    const id = source.accountId == null ? source.id : source.accountId;
    const account = id == null ? null : findAccount(String(id));
    return account && account.name.trim() ? account.name.trim() : "未命名账号";
  }

  function hasRetryableBalanceFailure(data) {
    return balanceReports(data).some((report) => balanceReportFailed(report) && balanceReportView(report).retryable === true);
  }

  function retryableBalanceAccountIds(data) {
    const seen = Object.create(null);
    return balanceReports(data).reduce((ids, report) => {
      const view = balanceReportView(report);
      const value = report && report.accountId != null ? report.accountId : view.accountId;
      const id = value == null ? "" : String(value).trim();
      if (id && !seen[id] && balanceReportFailed(report) && view.retryable === true) {
        seen[id] = true;
        ids.push(id);
      }
      return ids;
    }, []);
  }

  function balanceRefreshMessage(data) {
    const payload = data && typeof data === "object" ? data : {};
    const reports = balanceReports(payload);
    if (reports.length) {
      const counts = { success: 0, partial: 0, failed: 0, unsupported: 0, skipped: 0 };
      const failures = [];
      reports.forEach((report) => {
        const state = balanceReportState(report);
        if (["ok", "success", "succeeded", "known", "refreshed"].includes(state)) counts.success += 1;
        else if (state === "partial") {
          counts.partial += 1;
          failures.push(report);
        } else if (state === "unsupported") counts.unsupported += 1;
        else if (balanceReportFailed(report)) {
          counts.failed += 1;
          failures.push(report);
        } else counts.skipped += 1;
      });
      const pieces = [`成功 ${counts.success}`, `失败 ${counts.failed}`];
      if (counts.partial > 0) pieces.push(`部分成功 ${counts.partial}`);
      if (counts.unsupported > 0) pieces.push(`不支持 ${counts.unsupported}`);
      if (counts.skipped > 0) pieces.push(`未检查 ${counts.skipped}`);
      let message = `余额刷新完成：${pieces.join("，")}。`;
      if (failures.length) {
        const details = failures.slice(0, 3).map((report) =>
          `${balanceReportName(report)}（${balanceErrorText(balanceReportView(report))}）`);
        message += ` 失败账号：${details.join("；")}${failures.length > 3 ? `；另有 ${failures.length - 3} 个` : ""}。`;
      }
      return message;
    }
    if (typeof payload.summary === "string" && payload.summary.trim()) return payload.summary.trim();
    if (typeof payload.balanceSummary === "string" && payload.balanceSummary.trim()) return payload.balanceSummary.trim();

    const candidates = [payload.counts, payload.summary, payload.balanceCounts, payload.balanceSummary, payload];
    let counts = null;
    for (const source of candidates) {
      const success = numericCount(source, ["success", "succeeded", "successful", "refreshed", "ok", "known"]);
      const failed = numericCount(source, ["failed", "failure", "failures", "error", "errors"]);
      const unsupported = numericCount(source, ["unsupported", "notSupported"]);
      const skipped = numericCount(source, ["skipped", "unknown"]);
      const total = numericCount(source, ["total", "checked", "processed"]);
      if (success !== null || failed !== null || unsupported !== null || skipped !== null || total !== null) {
        counts = { success, failed, unsupported, skipped, total };
        break;
      }
    }
    if (!counts && payload.status && Array.isArray(payload.status.accounts)) {
      counts = { success: 0, failed: 0, unsupported: 0, skipped: 0, total: payload.status.accounts.length };
      payload.status.accounts.forEach((account) => {
        const balance = account && account.balance && typeof account.balance === "object" ? account.balance : {};
        const state = typeof balance.status === "string" ? balance.status : balance.state;
        if (state === "ok" || state === "known") counts.success += 1;
        else if (state === "unsupported") counts.unsupported += 1;
        else if (state === "error" || state === "unavailable" || state === "auth_error") counts.failed += 1;
        else counts.skipped += 1;
      });
    }
    if (counts) {
      const pieces = [];
      if (counts.success !== null) pieces.push(`成功 ${counts.success}`);
      if (counts.failed !== null) pieces.push(`失败 ${counts.failed}`);
      if (counts.unsupported !== null) pieces.push(`不支持 ${counts.unsupported}`);
      if (counts.skipped !== null && counts.skipped > 0) pieces.push(`未检查 ${counts.skipped}`);
      if (!pieces.length && counts.total !== null) pieces.push(`已处理 ${counts.total}`);
      if (pieces.length) return `余额刷新完成：${pieces.join("，")}。`;
    }
    if (typeof payload.message === "string" && payload.message.trim()) return payload.message.trim();
    return "余额刷新请求已完成，请查看各账号的余额状态。";
  }

  async function refreshStatus(silent) {
    if (!shell.isReady() || shell.isBusy() || balanceRefreshPromise) return;
    if (statusRefreshPromise) return statusRefreshPromise;
    statusRefreshPromise = (async () => {
      try {
        const data = await api.request("/admin/status");
        setStatus(data.status || data);
        renderStatus();
        shell.setServiceState("online", "服务在线");
        if (!silent) shell.announce("状态已刷新");
      } catch (error) {
        shell.setServiceState("offline", "连接失败");
        if (!silent) shell.showGlobalError(`无法刷新状态：${error.message}`);
      }
    })().finally(() => {
      statusRefreshPromise = null;
    });
    return statusRefreshPromise;
  }

  function stopBalancePoll() {
    if (!balancePollTimer) return;
    window.clearTimeout(balancePollTimer);
    balancePollTimer = 0;
  }

  function startStatusPoll() {
    if (pollTimer) window.clearInterval(pollTimer);
    pollTimer = window.setInterval(() => {
      if (!document.hidden) refreshStatus(true);
    }, 15000);
  }

  function stopStatusPoll() {
    if (!pollTimer) return;
    window.clearInterval(pollTimer);
    pollTimer = 0;
  }

  function scheduleBalancePoll(delay, trigger) {
    stopBalancePoll();
    if (!shell.isReady() || document.hidden) return;
    balancePollTimer = window.setTimeout(() => {
      refreshBalances({ trigger: trigger || "poll" });
    }, typeof delay === "number" ? delay : balancePollInterval);
  }

  function balanceRetryPlan(retryable) {
    if (retryable && balanceRetryAttempt < balanceRetryDelays.length) {
      const delay = balanceRetryDelays[balanceRetryAttempt];
      balanceRetryAttempt += 1;
      return { delay, trigger: "retry", notice: ` 将在 ${Math.round(delay / 1000)} 秒后自动重试。` };
    }
    balanceRetryAttempt = 0;
    return { delay: balancePollInterval, trigger: "poll", notice: "" };
  }

  function balanceRefreshTime() {
    return new Intl.DateTimeFormat("zh-CN", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    }).format(new Date());
  }

  async function refreshBalances(options) {
    const trigger = options && typeof options.trigger === "string" ? options.trigger : "manual";
    const quiet = trigger === "poll" || trigger === "retry";
    if (balanceRefreshPromise) {
      if (quiet) return balanceRefreshPromise;
      await balanceRefreshPromise;
    }
    if (!shell.isReady() || shell.isBusy()) {
      scheduleBalancePoll(trigger === "retry" ? balanceRetryDelays[0] : balancePollInterval,
        trigger === "retry" ? "retry" : "poll");
      return;
    }
    stopBalancePoll();
    if (!quiet) shell.clearGlobalError();
    const busyText = trigger === "save" ? "保存后刷新中…"
      : (trigger === "retry" ? "自动重试中…" : (trigger === "poll" ? "自动刷新中…" : "正在刷新…"));
    const restoreButton = shell.setButtonBusy(elements.refreshBalancesButton, busyText);
    if (trigger === "manual") {
      shell.setTextIfChanged(elements.balanceRefreshResult, shell.isDirty()
        ? "正在刷新服务器已保存配置的余额；未保存编辑不会参与本次刷新…"
        : "正在刷新已保存配置的余额…");
    }
    let nextPlan = { delay: balancePollInterval, trigger: "poll", notice: "" };
    const task = (async () => {
      try {
        if (statusRefreshPromise) await statusRefreshPromise;
        const refreshRequest = {};
        if (trigger === "retry" && balanceRetryAccountIds.length) {
          refreshRequest.accountIds = balanceRetryAccountIds.slice();
        }
        const data = await api.request(`/admin/balances/refresh${trigger === "poll" ? "?automatic=1" : ""}`, {
          method: "POST",
          body: JSON.stringify(refreshRequest),
        });
        if (data.status) {
          setStatus(data.status);
          renderStatus();
        } else {
          await refreshStatus(true);
        }
        balanceRetryAccountIds = retryableBalanceAccountIds(data);
        nextPlan = balanceRetryPlan(balanceRetryAccountIds.length > 0 || hasRetryableBalanceFailure(data));
        const message = balanceRefreshMessage(data) + nextPlan.notice;
        if (trigger === "poll") {
          shell.setTextIfChanged(elements.balanceRefreshResult, `最近自动刷新 ${balanceRefreshTime()}。${message}`);
        } else if (trigger === "retry") {
          shell.setTextIfChanged(elements.balanceRefreshResult, `最近自动重试 ${balanceRefreshTime()}。${message}`);
        } else {
          shell.setTextIfChanged(elements.balanceRefreshResult, message);
          shell.announce(message);
        }
        return data;
      } catch (error) {
        if (trigger !== "retry") balanceRetryAccountIds = [];
        nextPlan = balanceRetryPlan(true);
        const failure = `刷新余额请求失败：${error.message}${nextPlan.notice}`;
        shell.setTextIfChanged(elements.balanceRefreshResult, failure);
        if (!quiet) {
          shell.showGlobalError(failure);
          shell.announce("余额刷新失败");
        }
        return null;
      }
    })();
    balanceRefreshPromise = task;
    try {
      return await task;
    } finally {
      if (balanceRefreshPromise === task) balanceRefreshPromise = null;
      restoreButton();
      scheduleBalancePoll(nextPlan.delay, nextPlan.trigger);
    }
  }

  function init() {
    elements.refreshBalancesButton.addEventListener("click", () => refreshBalances({ trigger: "manual" }));
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) {
        stopBalancePoll();
        shell.setTextIfChanged(elements.balanceRefreshResult, "余额自动刷新已暂停；返回页面后继续。");
      } else {
        refreshBalances({ trigger: "poll" });
      }
    });
    window.addEventListener("pagehide", () => {
      stopBalancePoll();
      stopStatusPoll();
    });
    window.addEventListener("pageshow", (event) => {
      if (event.persisted && shell.isReady() && !document.hidden) {
        startStatusPoll();
        refreshBalances({ trigger: "poll" });
      }
    });
  }

  return {
    init,
    refreshStatus,
    refreshBalances,
    startStatusPoll,
    stopBalancePoll,
    formatBalance,
    balanceErrorText,
    balanceStatusValue,
    balanceRefreshStatusValue,
    balanceStatusFailed,
    getStatus,
  };
}
