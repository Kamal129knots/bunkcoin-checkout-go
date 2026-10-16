// Shared by every page:
//   • session identity + balance (GET /api/me) → Bunk Coin card
//   • purchase history table (GET /api/orders), when the page has one
//
// The organisation and user are ULIDs from the session; nothing here shows names.

window.Shell = (() => {
  const $ = (id) => document.getElementById(id);
  const state = { config: null, me: null };

  const STATUS_LABEL = {
    pending: "Awaiting payment", payment_received: "Payment received", awaiting_payment: "Bank processing", credit_retrying: "Crediting…",
    fulfilled: "Credited", needs_review: "Under review", payment_failed: "Failed", expired: "Expired",
  };

  /** Loads config + session. Returns { config, me }. Shows a sign-in message if there's no session. */
  async function init() {
    const [config, me] = await Promise.all([api("/api/config"), api("/api/me")]);
    state.config = config;
    state.me = me;
    if ($("mode-badge")) $("mode-badge").hidden = !config.publishable_key.startsWith("pk_test_");
    if ($("today")) {
      $("today").textContent = new Date().toLocaleDateString(undefined, { weekday: "long", year: "numeric", month: "long", day: "numeric" });
    }
    if (!me.signed_in) {
      showBanner({ message: "Please sign in to 129Knots to see your Bunk Coin.", retryable: false }, { title: "Not signed in", tone: "warn" });
      return { config, me };
    }
    setBalance(me.balance);
    if ($("coin-utilised")) $("coin-utilised").textContent = "0"; // PRODUCTION: from usage records
    loadOrders();
    return { config, me };
  }

  function setBalance(n) {
    if ($("coin-available")) $("coin-available").textContent = formatInt(n);
  }

  async function refreshMe() {
    try {
      state.me = await api("/api/me");
      if (state.me.signed_in) setBalance(state.me.balance);
    } catch { /* non-critical */ }
  }

  async function loadOrders() {
    const body = $("orders");
    if (!body) return;
    try {
      const { orders } = await api("/api/orders");
      body.innerHTML = "";
      $("orders-empty").hidden = orders.length > 0;
      for (const o of orders) {
        const tr = document.createElement("tr");
        tr.innerHTML = "<td><span class=q></span><small class=d></small></td><td class=a></td><td><span class=pill></span></td>";
        tr.querySelector(".q").textContent = `${formatInt(o.quantity)} coins`;
        // Order ULID (last 6 chars) + date — the reference support would ask for.
        tr.querySelector(".d").textContent = [`#${o.order_id.slice(-6)}`,
          o.created_at && new Date(o.created_at).toLocaleString([], { dateStyle: "medium", timeStyle: "short" })]
          .filter(Boolean).join(" · ");
        tr.querySelector(".d").title = o.order_id;
        tr.querySelector(".a").textContent = o.currency ? formatMoney(o.total, o.currency) : "";
        const pill = tr.querySelector(".pill");
        pill.textContent = STATUS_LABEL[o.status] ?? o.status;
        pill.classList.add(o.status);
        body.appendChild(tr);
      }
    } catch { /* non-critical */ }
  }

  return { init, state, refreshMe, loadOrders, setBalance };
})();
