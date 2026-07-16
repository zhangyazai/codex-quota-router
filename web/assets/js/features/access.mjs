export function createAccessFeature({ api, shell, getConfig, setConfig, showView }) {
  const elements = {
    allowPublicAccess: document.getElementById("allowPublicAccess"),
    publicBaseUrl: document.getElementById("publicBaseUrl"),
    allowPublicAdmin: document.getElementById("allowPublicAdmin"),
    adminPassword: document.getElementById("adminPassword"),
    clearAdminPassword: document.getElementById("clearAdminPassword"),
    adminPasswordState: document.getElementById("adminPasswordState"),
    newGatewayTokenName: document.getElementById("newGatewayTokenName"),
    addGatewayTokenButton: document.getElementById("addGatewayTokenButton"),
    gatewayTokenSearch: document.getElementById("gatewayTokenSearch"),
    gatewayTokenList: document.getElementById("gatewayTokenList"),
    gatewayTokenPageState: document.getElementById("gatewayTokenPageState"),
    gatewayTokenPrevious: document.getElementById("gatewayTokenPrevious"),
    gatewayTokenNext: document.getElementById("gatewayTokenNext"),
  };
  let gatewayTokens = [];
  let gatewayTokenOffset = 0;
  let gatewayTokenTotal = 0;
  const gatewayTokenPageSize = 100;
  let tokenSearchTimer = 0;

  function syncSecurityFields() {
    const config = getConfig();
    const publicPage = window.location.protocol === "https:";
    elements.allowPublicAccess.checked = config.allowPublicAccess;
    elements.publicBaseUrl.value = config.publicBaseUrl;
    elements.allowPublicAdmin.checked = config.allowPublicAdmin;
    elements.adminPassword.value = "";
    elements.clearAdminPassword.checked = false;
    elements.adminPasswordState.textContent = config.adminPasswordConfigured
      ? "已保存管理员密码；留空会保留原密码。启用公网访问时，本机管理也需要登录。"
      : "尚未设置管理员密码；启用公网访问时必须填写至少 12 个字节。";
    elements.allowPublicAccess.disabled = publicPage;
    elements.publicBaseUrl.disabled = publicPage;
    elements.allowPublicAdmin.disabled = publicPage;
    elements.clearAdminPassword.disabled = publicPage;
    if (publicPage) {
      [elements.allowPublicAccess, elements.publicBaseUrl, elements.allowPublicAdmin,
        elements.clearAdminPassword].forEach((element) => {
        if (element.hasAttribute("data-page-lock-disabled")) {
          element.dataset.pageLockDisabled = "true";
        }
      });
      elements.adminPasswordState.textContent += " 公网访问设置只能从本机管理页修改。";
    }
  }

  function validationFailure(element, message) {
    showView("security", { focus: false });
    window.setTimeout(() => element.focus(), 0);
    shell.showGlobalError(message);
    return false;
  }

  function validateSecuritySettings() {
    const publicBaseUrl = elements.publicBaseUrl.value.trim();
    if (elements.allowPublicAccess.checked) {
      let parsed;
      try {
        parsed = new URL(publicBaseUrl);
      } catch (_error) {
        return validationFailure(elements.publicBaseUrl, "公网地址必须是完整的 HTTPS 地址");
      }
      if (parsed.protocol !== "https:" || !parsed.host || parsed.username || parsed.password ||
          parsed.pathname !== "/" || parsed.search || parsed.hash) {
        return validationFailure(elements.publicBaseUrl, "公网地址只能包含 HTTPS 协议、主机和可选端口");
      }
    }
    if (elements.allowPublicAdmin.checked && !elements.allowPublicAccess.checked) {
      return validationFailure(elements.allowPublicAccess, "允许公网管理前必须先允许公网访问");
    }
    const password = elements.adminPassword.value;
    if (password && new TextEncoder().encode(password).length < 12) {
      return validationFailure(elements.adminPassword, "管理员密码至少需要 12 个字节");
    }
    const config = getConfig();
    if (elements.allowPublicAccess.checked &&
        (!config.adminPasswordConfigured || elements.clearAdminPassword.checked) && !password) {
      return validationFailure(elements.adminPassword, "允许公网访问时必须设置管理员密码");
    }
    return true;
  }

  function securityPayload() {
    return {
      allowPublicAccess: elements.allowPublicAccess.checked,
      publicBaseUrl: elements.publicBaseUrl.value.trim(),
      allowPublicAdmin: elements.allowPublicAdmin.checked,
      adminPassword: elements.adminPassword.value,
      clearAdminPassword: elements.clearAdminPassword.checked,
    };
  }

  function syncControls() {
    const locked = !shell.isReady() || shell.isBusy();
    elements.gatewayTokenPrevious.disabled = locked || gatewayTokenOffset <= 0;
    elements.gatewayTokenNext.disabled = locked || gatewayTokenOffset + gatewayTokenPageSize >= gatewayTokenTotal;
  }

  function renderGatewayTokens() {
    elements.gatewayTokenList.replaceChildren();
    gatewayTokens.forEach((token) => {
      const item = document.createElement("li");
      item.className = "token-item";
      const summary = document.createElement("div");
      const name = document.createElement("div");
      name.className = "token-item-name";
      name.textContent = token.name || "未命名 Token";
      const meta = document.createElement("div");
      meta.className = "token-item-meta";
      meta.textContent = token.createdAt ? `创建于 ${new Date(token.createdAt).toLocaleString()}` : "已从旧版本迁移";
      summary.append(name, meta);
      const actions = document.createElement("div");
      actions.className = "toolbar";
      [
        ["copy-token", "复制 Token", "secondary"],
        ["copy-local", "复制本机配置", "secondary"],
        ["copy-public", "复制公网配置", "secondary"],
        ["rotate", "轮换", "secondary"],
        ["delete", "删除", "danger-button"],
      ].forEach((definition) => {
        if (definition[0] === "copy-public" && !getConfig().allowPublicAccess) return;
        const button = document.createElement("button");
        button.type = "button";
        button.className = definition[2];
        button.dataset.tokenAction = definition[0];
        button.dataset.tokenId = token.id;
        button.textContent = definition[1];
        actions.appendChild(button);
      });
      item.append(summary, actions);
      elements.gatewayTokenList.appendChild(item);
    });
    if (!gatewayTokens.length) {
      const empty = document.createElement("li");
      empty.className = "helper";
      empty.textContent = gatewayTokenTotal ? "当前页没有 Token" : "还没有网关 Token，新增后才能调用代理。";
      elements.gatewayTokenList.appendChild(empty);
    }
    const start = gatewayTokenTotal ? gatewayTokenOffset + 1 : 0;
    const end = Math.min(gatewayTokenOffset + gatewayTokens.length, gatewayTokenTotal);
    elements.gatewayTokenPageState.textContent = `显示 ${start}–${end}，共 ${gatewayTokenTotal} 个`;
    elements.gatewayTokenPrevious.disabled = gatewayTokenOffset <= 0;
    elements.gatewayTokenNext.disabled = gatewayTokenOffset + gatewayTokenPageSize >= gatewayTokenTotal;
    syncControls();
  }

  async function loadGatewayTokens() {
    const query = elements.gatewayTokenSearch.value.trim();
    const data = await api.request(`/admin/gateway-tokens?offset=${gatewayTokenOffset}` +
      `&limit=${gatewayTokenPageSize}&query=${encodeURIComponent(query)}`);
    gatewayTokens = Array.isArray(data.tokens) ? data.tokens : [];
    gatewayTokenTotal = Number.isSafeInteger(data.total) ? data.total : 0;
    renderGatewayTokens();
  }

  async function copyGatewayTokenConfig(id, target, button) {
    const restoreButton = shell.setButtonBusy(button, "正在复制…");
    try {
      const data = await api.request("/admin/gateway-tokens/snippet", {
        method: "POST",
        body: JSON.stringify({ id, target }),
      });
      await shell.copyText(data.snippet || "");
      shell.announce(target === "token" ? "网关 Token 已复制" :
        (target === "public" ? "公网 Codex 配置已复制" : "本机 Codex 配置已复制"));
    } catch (error) {
      shell.showGlobalError(`复制失败：${error.message}`);
    } finally {
      restoreButton();
    }
  }

  async function createGatewayToken() {
    const name = elements.newGatewayTokenName.value.trim();
    if (!name) {
      showView("codex", { focus: false });
      elements.newGatewayTokenName.focus();
      shell.showGlobalError("请填写新 Token 名称");
      return;
    }
    shell.clearGlobalError();
    const restoreButton = shell.setButtonBusy(elements.addGatewayTokenButton, "正在新增…");
    try {
      const data = await api.request("/admin/config", {
        method: "PUT",
        body: JSON.stringify({ createGatewayTokenName: name }),
      });
      setConfig(data.config);
      elements.newGatewayTokenName.value = "";
      gatewayTokenOffset = Math.max(0,
        Math.floor((getConfig().gatewayTokenCount - 1) / gatewayTokenPageSize) * gatewayTokenPageSize);
      await loadGatewayTokens();
      if (data.codexSnippet) await shell.copyText(data.codexSnippet);
      shell.announce("Token 已新增，本机 Codex 配置已复制");
    } catch (error) {
      shell.showGlobalError(`新增 Token 失败：${error.message}`);
    } finally {
      restoreButton();
    }
  }

  async function rotateGatewayToken(id, button) {
    if (!window.confirm("轮换后旧 Token 会立即失效。确定继续吗？")) return;
    const restoreButton = shell.setButtonBusy(button, "正在轮换…");
    try {
      const data = await api.request("/admin/config", {
        method: "PUT",
        body: JSON.stringify({ rotateGatewayTokenId: id }),
      });
      setConfig(data.config);
      await loadGatewayTokens();
      if (data.codexSnippet) await shell.copyText(data.codexSnippet);
      shell.announce("Token 已轮换，新本机配置已复制");
    } catch (error) {
      shell.showGlobalError(`轮换 Token 失败：${error.message}`);
    } finally {
      restoreButton();
    }
  }

  async function deleteGatewayToken(id, button) {
    if (!window.confirm("删除后使用该 Token 的所有客户端会立即失去访问权限。确定删除吗？")) return;
    const restoreButton = shell.setButtonBusy(button, "正在删除…");
    try {
      const data = await api.request("/admin/config", {
        method: "PUT",
        body: JSON.stringify({ deleteGatewayTokenId: id }),
      });
      setConfig(data.config);
      if (gatewayTokenOffset >= getConfig().gatewayTokenCount && gatewayTokenOffset > 0) {
        gatewayTokenOffset = Math.max(0, gatewayTokenOffset - gatewayTokenPageSize);
      }
      await loadGatewayTokens();
      shell.announce("Token 已删除");
    } catch (error) {
      shell.showGlobalError(`删除 Token 失败：${error.message}`);
    } finally {
      restoreButton();
    }
  }

  function resetPagination() {
    gatewayTokenOffset = 0;
  }

  function init() {
    elements.addGatewayTokenButton.addEventListener("click", createGatewayToken);
    elements.gatewayTokenList.addEventListener("click", (event) => {
      const button = event.target.closest("button[data-token-action]");
      if (!button) return;
      const action = button.dataset.tokenAction;
      const id = button.dataset.tokenId;
      if (action === "copy-token") copyGatewayTokenConfig(id, "token", button);
      else if (action === "copy-local") copyGatewayTokenConfig(id, "local", button);
      else if (action === "copy-public") copyGatewayTokenConfig(id, "public", button);
      else if (action === "rotate") rotateGatewayToken(id, button);
      else if (action === "delete") deleteGatewayToken(id, button);
    });
    elements.gatewayTokenPrevious.addEventListener("click", () => {
      gatewayTokenOffset = Math.max(0, gatewayTokenOffset - gatewayTokenPageSize);
      loadGatewayTokens().catch((error) => shell.showGlobalError(error.message));
    });
    elements.gatewayTokenNext.addEventListener("click", () => {
      gatewayTokenOffset += gatewayTokenPageSize;
      loadGatewayTokens().catch((error) => shell.showGlobalError(error.message));
    });
    elements.gatewayTokenSearch.addEventListener("input", () => {
      window.clearTimeout(tokenSearchTimer);
      tokenSearchTimer = window.setTimeout(() => {
        gatewayTokenOffset = 0;
        loadGatewayTokens().catch((error) => shell.showGlobalError(error.message));
      }, 250);
    });
    [elements.allowPublicAccess, elements.publicBaseUrl, elements.allowPublicAdmin,
      elements.adminPassword, elements.clearAdminPassword].forEach((element) => {
      element.addEventListener(element.type === "checkbox" ? "change" : "input", () => {
        const config = getConfig();
        config.allowPublicAccess = elements.allowPublicAccess.checked;
        config.publicBaseUrl = elements.publicBaseUrl.value.trim();
        config.allowPublicAdmin = elements.allowPublicAdmin.checked;
        shell.markDirty();
        renderGatewayTokens();
      });
    });
  }

  return {
    init,
    syncSecurityFields,
    validateSecuritySettings,
    securityPayload,
    renderGatewayTokens,
    loadGatewayTokens,
    resetPagination,
    syncControls,
  };
}
