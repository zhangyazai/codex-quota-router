export function createApiClient() {
  let csrfToken = "";

  return {
    setCsrfToken(value) {
      csrfToken = typeof value === "string" ? value : "";
    },

    clearCsrfToken() {
      csrfToken = "";
    },

    async request(path, options) {
      const requestOptions = Object.assign({ cache: "no-store" }, options || {});
      requestOptions.headers = Object.assign({ Accept: "application/json" }, requestOptions.headers || {});
      if (requestOptions.body !== undefined) {
        requestOptions.headers["Content-Type"] = "application/json";
      }
      if (requestOptions.method && requestOptions.method !== "GET") {
        requestOptions.headers["X-CSRF-Token"] = csrfToken;
      }

      const response = await fetch(path, requestOptions);
      const text = await response.text();
      let data = {};
      if (text) {
        try {
          data = JSON.parse(text);
        } catch (_error) {
          data = { message: text };
        }
      }
      if (!response.ok) {
        const message = data.error || data.message || `请求失败，HTTP ${response.status}`;
        const error = new Error(message);
        error.status = response.status;
        error.data = data;
        throw error;
      }
      return data;
    },
  };
}
