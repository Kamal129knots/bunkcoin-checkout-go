// Shared fetch + error helpers for index.html and return.html.

class ApiError extends Error {
  constructor({ status = 0, code = "unknown", message, retryable = false, requestId = "", fields = [] }) {
    super(message || "Something went wrong.");
    Object.assign(this, { status, code, retryable, requestId, fields });
  }
}

/**
 * Calls our backend and always either resolves with parsed JSON or throws an ApiError
 * with a customer-safe message. Handles offline, timeouts, non-JSON and error envelopes.
 */
async function api(path, { method = "GET", body, headers = {}, timeoutMs = 20000 } = {}) {
  if (!navigator.onLine) {
    throw new ApiError({ code: "offline", retryable: true,
      message: "You appear to be offline. Check your connection and try again." });
  }
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  let res;
  try {
    res = await fetch(path, {
      method,
      headers: { Accept: "application/json", ...(body ? { "Content-Type": "application/json" } : {}), ...headers },
      body: body ? JSON.stringify(body) : undefined,
      signal: ctrl.signal,
      credentials: "same-origin",
    });
  } catch (err) {
    if (err.name === "AbortError") {
      throw new ApiError({ code: "timeout", retryable: true,
        message: "The server took too long to respond. Please try again." });
    }
    throw new ApiError({ code: "network_error", retryable: true,
      message: "We couldn't reach the server. Please check your connection and try again." });
  } finally {
    clearTimeout(timer);
  }

  const requestId = res.headers.get("X-Request-Id") || "";
  let data = null;
  try { data = await res.json(); } catch { /* non-JSON (proxy error page, etc.) */ }

  if (res.ok && data) return data;

  if (data && data.error) {
    throw new ApiError({ status: res.status, ...data.error, requestId: data.error.request_id || requestId });
  }
  throw new ApiError({
    status: res.status, requestId,
    retryable: res.status >= 500 || res.status === 429,
    code: "http_" + res.status,
    message: res.status >= 500
      ? "Our server had a problem. Please try again in a moment."
      : "The request couldn't be completed.",
  });
}

// Money is handled in the currency's minor unit (cents) end-to-end; only format at the edge.
function formatMoney(minor, currency) {
  const fmt = new Intl.NumberFormat(undefined, { style: "currency", currency: currency.toUpperCase() });
  const digits = fmt.resolvedOptions().maximumFractionDigits; // 2 for USD/SGD, 0 for JPY
  return fmt.format(minor / 10 ** digits);
}

function formatInt(n) {
  return new Intl.NumberFormat().format(n);
}

function showBanner(err, { onRetry, title, tone = "error" } = {}) {
  const el = document.getElementById("banner");
  el.classList.toggle("warn", tone === "warn");
  document.getElementById("banner-title").textContent =
    title || (err.retryable ? "That didn't go through" : "We couldn't complete that");
  document.getElementById("banner-msg").textContent = err.message;
  document.getElementById("banner-ref").textContent = err.requestId ? `Reference: ${err.requestId}` : "";
  const retry = document.getElementById("banner-retry");
  retry.hidden = !(onRetry && err.retryable !== false);
  retry.onclick = () => { hideBanner(); onRetry(); };
  document.getElementById("banner-close").onclick = hideBanner;
  el.hidden = false;
  el.scrollIntoView({ behavior: "smooth", block: "nearest" });
}

function hideBanner() {
  document.getElementById("banner").hidden = true;
}

// Stripe.js is loaded async from js.stripe.com; wait for it, and fail clearly if it's
// blocked (ad blockers, corporate proxies, CSP).
function waitForStripe(timeoutMs = 10000) {
  return new Promise((resolve, reject) => {
    if (window.Stripe) return resolve(window.Stripe);
    const tag = document.getElementById("stripe-js");
    const fail = () => reject(new ApiError({ code: "stripe_js_unavailable", retryable: true,
      message: "The secure payment form couldn't load. Disable ad/script blockers for this page or try another network, then reload." }));
    const t = setTimeout(fail, timeoutMs);
    tag.addEventListener("load", () => { clearTimeout(t); window.Stripe ? resolve(window.Stripe) : fail(); });
    tag.addEventListener("error", () => { clearTimeout(t); fail(); });
  });
}
