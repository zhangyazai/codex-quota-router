const THEME_KEY = "cqr-theme";
const SIDEBAR_KEY = "cqr-sidebar-collapsed";

function readPreference(key, fallback) {
  try {
    return localStorage.getItem(key) || fallback;
  } catch (_error) {
    return fallback;
  }
}

function writePreference(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch (_error) {
  }
}

export function createAppShell() {
  const elements = {
    main: document.getElementById("main"),
    serviceState: document.getElementById("serviceState"),
    globalNotice: document.getElementById("globalNotice"),
    liveRegion: document.getElementById("liveRegion"),
    dirtyState: document.getElementById("dirtyState"),
    savedState: document.getElementById("savedState"),
    saveBar: document.getElementById("saveBar"),
    reloadButton: document.getElementById("reloadButton"),
    saveButton: document.getElementById("saveButton"),
    logoutButton: document.getElementById("logoutButton"),
    loginDialog: document.getElementById("loginDialog"),
    loginForm: document.getElementById("loginForm"),
    loginPassword: document.getElementById("loginPassword"),
    loginError: document.getElementById("loginError"),
    loginButton: document.getElementById("loginButton"),
    sidebar: document.getElementById("sidebar"),
    sidebarBackdrop: document.getElementById("sidebarBackdrop"),
    mobileMenuButton: document.getElementById("mobileMenuButton"),
    sidebarCollapseButton: document.getElementById("sidebarCollapseButton"),
    themeButton: document.getElementById("themeButton"),
    themeMenu: document.getElementById("themeMenu"),
  };

  let dirty = false;
  let pageReady = false;
  let pageBusy = true;
  let currentView = "overview";
  let themePreference = readPreference(THEME_KEY, "auto");
  let viewFocusTimer = 0;
  const colorScheme = typeof window.matchMedia === "function"
    ? window.matchMedia("(prefers-color-scheme: dark)")
    : null;

  function announce(message) {
    if (!elements.liveRegion) return;
    elements.liveRegion.textContent = "";
    window.setTimeout(() => {
      elements.liveRegion.textContent = message;
    }, 20);
  }

  function setTextIfChanged(element, text) {
    if (!element) return;
    const next = text == null ? "" : String(text);
    if (element.textContent !== next) {
      element.textContent = next;
    }
  }

  function setHiddenIfChanged(element, hidden) {
    if (element && element.hidden !== hidden) {
      element.hidden = hidden;
    }
  }

  function setDisabledPreservingBusy(control, disabled) {
    if (!control) return;
    if (control.getAttribute("aria-busy") === "true" && !disabled) return;
    control.disabled = disabled;
  }

  function showGlobalError(message) {
    if (!elements.globalNotice) return;
    elements.globalNotice.textContent = message;
    elements.globalNotice.hidden = false;
    elements.globalNotice.focus();
  }

  function clearGlobalError() {
    if (!elements.globalNotice) return;
    elements.globalNotice.hidden = true;
    elements.globalNotice.textContent = "";
  }

  function setServiceState(kind, text) {
    if (!elements.serviceState) return;
    if (elements.serviceState.dataset.state !== kind) {
      elements.serviceState.dataset.state = kind;
    }
    setTextIfChanged(elements.serviceState, text);
  }

  function restorePageControl(control) {
    if (!control || !control.hasAttribute("data-page-lock-disabled")) return;
    control.disabled = control.dataset.pageLockDisabled === "true";
    control.removeAttribute("data-page-lock-disabled");
  }

  function updatePageInteractivity() {
    if (!elements.main) return;
    const locked = pageBusy || !pageReady;
    elements.main.setAttribute("aria-busy", String(pageBusy));
    if (pageBusy) {
      elements.main.setAttribute("inert", "");
    } else {
      elements.main.removeAttribute("inert");
    }
    elements.main.querySelectorAll("button, input, select").forEach((control) => {
      if (locked) {
        if (!control.hasAttribute("data-page-lock-disabled")) {
          control.dataset.pageLockDisabled = String(control.disabled);
        }
        control.disabled = true;
      } else {
        restorePageControl(control);
      }
    });
    if (!pageBusy && !pageReady && elements.reloadButton) {
      restorePageControl(elements.reloadButton);
      elements.reloadButton.disabled = false;
    }
  }

  function setReady(ready) {
    pageReady = ready === true;
    updatePageInteractivity();
  }

  function setBusy(busy) {
    pageBusy = busy === true;
    updatePageInteractivity();
  }

  function markDirty() {
    if (dirty) return;
    dirty = true;
    if (elements.dirtyState) elements.dirtyState.hidden = false;
    if (elements.savedState) elements.savedState.hidden = true;
    document.title = "• Codex Quota Router";
  }

  function markClean() {
    dirty = false;
    if (elements.dirtyState) elements.dirtyState.hidden = true;
    if (elements.savedState) elements.savedState.hidden = false;
    document.title = "Codex Quota Router";
  }

  async function copyText(value) {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      await navigator.clipboard.writeText(value);
      return;
    }
    const textarea = document.createElement("textarea");
    textarea.value = value;
    textarea.setAttribute("readonly", "");
    textarea.className = "visually-hidden";
    document.body.appendChild(textarea);
    textarea.select();
    if (!document.execCommand("copy")) {
      textarea.remove();
      throw new Error("浏览器拒绝访问剪贴板");
    }
    textarea.remove();
  }

  function showLoginDialog(message) {
    if (!elements.loginDialog) return;
    elements.loginError.textContent = message || "";
    elements.loginError.hidden = !message;
    elements.loginPassword.value = "";
    if (!elements.loginDialog.open) {
      elements.loginDialog.showModal();
    }
    window.setTimeout(() => elements.loginPassword.focus(), 0);
  }

  function setButtonBusy(button, busyText) {
    const original = button.textContent;
    const originallyDisabled = button.disabled;
    button.disabled = true;
    button.textContent = busyText;
    button.setAttribute("aria-busy", "true");
    return () => {
      button.textContent = original;
      button.removeAttribute("aria-busy");
      if (pageBusy || !pageReady) {
        if (button.hasAttribute("data-page-lock-disabled")) {
          button.dataset.pageLockDisabled = String(originallyDisabled);
        }
        button.disabled = true;
      } else {
        button.disabled = originallyDisabled;
      }
    };
  }

  function closeMobileSidebar(restoreFocus = false) {
    document.body.classList.remove("sidebar-open");
    if (elements.sidebarBackdrop) elements.sidebarBackdrop.hidden = true;
    if (elements.mobileMenuButton) {
      elements.mobileMenuButton.setAttribute("aria-expanded", "false");
      elements.mobileMenuButton.setAttribute("aria-label", "打开导航");
      if (restoreFocus) window.setTimeout(() => elements.mobileMenuButton.focus(), 0);
    }
  }

  function openMobileSidebar() {
    document.body.classList.add("sidebar-open");
    if (elements.sidebarBackdrop) elements.sidebarBackdrop.hidden = false;
    if (elements.mobileMenuButton) {
      elements.mobileMenuButton.setAttribute("aria-expanded", "true");
      elements.mobileMenuButton.setAttribute("aria-label", "关闭导航");
    }
  }

  function setSidebarCollapsed(collapsed, persist = true) {
    const value = collapsed === true;
    document.documentElement.dataset.sidebarCollapsed = String(value);
    if (elements.sidebar) elements.sidebar.dataset.collapsed = String(value);
    if (elements.sidebarCollapseButton) {
      elements.sidebarCollapseButton.setAttribute("aria-expanded", String(!value));
      const label = value ? "展开侧栏" : "收起侧栏";
      elements.sidebarCollapseButton.setAttribute("aria-label", label);
      elements.sidebarCollapseButton.title = label;
    }
    if (persist) writePreference(SIDEBAR_KEY, String(value));
  }

  function resolvedTheme() {
    if (themePreference === "dark" || themePreference === "light") return themePreference;
    return colorScheme && colorScheme.matches ? "dark" : "light";
  }

  function applyTheme(preference, persist = true) {
    themePreference = preference === "dark" || preference === "light" ? preference : "auto";
    document.documentElement.dataset.themePreference = themePreference;
    document.documentElement.dataset.theme = resolvedTheme();
    document.querySelectorAll("[data-theme-choice]").forEach((choice) => {
      const selected = choice.dataset.themeChoice === themePreference;
      choice.setAttribute("aria-pressed", String(selected));
      choice.classList.toggle("active", selected);
    });
    if (persist) writePreference(THEME_KEY, themePreference);
  }

  function closeThemeMenu(restoreFocus = false) {
    if (!elements.themeMenu) return;
    elements.themeMenu.hidden = true;
    if (elements.themeButton) elements.themeButton.setAttribute("aria-expanded", "false");
    if (restoreFocus && elements.themeButton) {
      window.setTimeout(() => elements.themeButton.focus(), 0);
    }
  }

  function availableViews() {
    return Array.from(document.querySelectorAll("[data-view]"));
  }

  function focusCurrentView(view) {
    window.clearTimeout(viewFocusTimer);
    viewFocusTimer = window.setTimeout(() => {
      viewFocusTimer = 0;
      const title = view && view.querySelector("[data-view-title], h1, h2");
      const target = title || elements.main;
      if (!target) return;
      if (!target.matches("a, button, input, select, textarea, [tabindex]")) {
        target.setAttribute("tabindex", "-1");
      }
      target.focus();
    }, 0);
  }

  function showView(name, options = {}) {
    const views = availableViews();
    if (!views.length) {
      currentView = name || "overview";
      return currentView;
    }
    const requested = String(name || "").replace(/^#/, "");
    const fallback = views.find((view) => view.dataset.view === "overview") || views[0];
    const active = views.find((view) => view.dataset.view === requested) || fallback;
    currentView = active.dataset.view;
    if (elements.saveBar) elements.saveBar.hidden = currentView === "overview" || currentView === "codex";
    views.forEach((view) => {
      view.hidden = view !== active;
    });
    document.querySelectorAll("[data-view-target]").forEach((target) => {
      if (target.dataset.viewTarget === currentView) {
        target.setAttribute("aria-current", "page");
      } else {
        target.removeAttribute("aria-current");
      }
    });
    const nextHash = `#${encodeURIComponent(currentView)}`;
    if (options.updateHash !== false && window.location.hash !== nextHash) {
      if (options.replaceHash === true) {
        window.history.replaceState(null, "", nextHash);
      } else {
        window.location.hash = nextHash;
      }
    }
    closeMobileSidebar();
    if (options.focus !== false) focusCurrentView(active);
    return currentView;
  }

  function initUI() {
    setSidebarCollapsed(readPreference(SIDEBAR_KEY, "false") === "true", false);
    applyTheme(themePreference, false);

    if (elements.mobileMenuButton) {
      elements.mobileMenuButton.addEventListener("click", () => {
        if (document.body.classList.contains("sidebar-open")) closeMobileSidebar();
        else openMobileSidebar();
      });
    }
    if (elements.sidebarBackdrop) {
      elements.sidebarBackdrop.addEventListener("click", () => closeMobileSidebar(true));
    }
    if (elements.sidebarCollapseButton) {
      elements.sidebarCollapseButton.addEventListener("click", () => {
        setSidebarCollapsed(document.documentElement.dataset.sidebarCollapsed !== "true");
      });
    }
    if (elements.themeButton && elements.themeMenu) {
      elements.themeButton.addEventListener("click", () => {
        const opening = elements.themeMenu.hidden;
        if (!opening) {
          closeThemeMenu();
          return;
        }
        elements.themeMenu.hidden = false;
        elements.themeButton.setAttribute("aria-expanded", "true");
        const selected = elements.themeMenu.querySelector('[data-theme-choice][aria-pressed="true"]');
        if (selected) window.setTimeout(() => selected.focus(), 0);
      });
      document.addEventListener("click", (event) => {
        if (!elements.themeMenu.hidden && !elements.themeMenu.contains(event.target) &&
            !elements.themeButton.contains(event.target)) {
          closeThemeMenu();
        }
      });
    }
    document.querySelectorAll("[data-theme-choice]").forEach((choice) => {
      choice.addEventListener("click", () => {
        applyTheme(choice.dataset.themeChoice);
        closeThemeMenu(true);
      });
    });
    document.querySelectorAll("[data-view-target]").forEach((target) => {
      target.addEventListener("click", (event) => {
        event.preventDefault();
        showView(target.dataset.viewTarget);
      });
    });
    window.addEventListener("hashchange", () => {
      const requested = decodeURIComponent(window.location.hash.slice(1));
      if (requested === currentView) return;
      showView(requested, { updateHash: false });
    });
    document.addEventListener("keydown", (event) => {
      if (event.key === "Escape") {
        const themeWasOpen = elements.themeMenu && !elements.themeMenu.hidden;
        closeThemeMenu(themeWasOpen);
        closeMobileSidebar(!themeWasOpen && document.body.classList.contains("sidebar-open"));
      }
    });
    if (colorScheme) {
      colorScheme.addEventListener("change", () => {
        if (themePreference === "auto") applyTheme("auto", false);
      });
    }
    const initial = decodeURIComponent(window.location.hash.slice(1)) || "overview";
    showView(initial, { replaceHash: !window.location.hash, focus: false });
  }

  return {
    elements,
    announce,
    setTextIfChanged,
    setHiddenIfChanged,
    setDisabledPreservingBusy,
    showGlobalError,
    clearGlobalError,
    setServiceState,
    updatePageInteractivity,
    setReady,
    isReady: () => pageReady,
    setBusy,
    isBusy: () => pageBusy,
    markDirty,
    markClean,
    isDirty: () => dirty,
    copyText,
    showLoginDialog,
    setButtonBusy,
    showView,
    getCurrentView: () => currentView,
    initUI,
  };
}
