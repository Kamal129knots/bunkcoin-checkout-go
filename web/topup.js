// Top-up page logic: choose a package or custom amount → embedded Stripe Checkout.
//
// What this file does NOT do (by design):
//   • decide the price  → the server does (src/pricing.js)
//   • say who is buying → the server reads org/user ULIDs from the session (src/auth.js)
//   • credit BunkCoin   → only the Stripe webhook does (src/app.js → fulfil)

const $ = (id) => document.getElementById(id);

// Page memory. `const` fixes the object; its properties change as the user goes.
const state = {
  config: null,   // /api/config: packages, price, limits, publishable key
  choice: null,   // { kind: "package", id } | { kind: "custom" }
  idemKey: null,  // idempotency key for the current purchase attempt
  idemFor: null,  // the choice that key belongs to
  stripe: null,   // Stripe.js client (publishable key)
  checkout: null, // the mounted embedded checkout
  busy: false,    // blocks double clicks while creating a session
};

document.addEventListener("DOMContentLoaded", boot);

async function boot() {
  let shell;
  try {
    shell = await Shell.init(); // config + session (org/user ULIDs) + balance + history
  } catch (err) {
    showBanner(err, { title: "Couldn't load the page", onRetry: () => location.reload() });
    return;
  }
  if (!shell.me.signed_in) return; // Shell already showed "Please sign in"
  state.config = shell.config;

  $("app-area").hidden = false;
  const c = state.config;
  $("rate-line").textContent = `1 coin = ${formatMoney(c.unit_amount, c.currency)}`;
  $("custom-hint").textContent = `Between ${formatInt(c.min_quantity)} and ${formatInt(c.max_quantity)} coins.`;
  renderPackages();
  wireForm();
  $("loading").hidden = true;
  $("buy-form").hidden = false;

  // Arrived via "Purchase history" on the dashboard → scroll to it.
  if (location.hash === "#purchases") $("purchases").scrollIntoView({ block: "start" });

  // "+" on this page's Bunk Coin card highlights the amount picker.
  $("coin-add").addEventListener("click", () => {
    if (!$("step-pay").hidden) backToSelect();
    const panel = $("topup");
    panel.scrollIntoView({ behavior: "smooth", block: "start" });
    panel.classList.remove("flash");
    void panel.offsetWidth; // restart the highlight animation
    panel.classList.add("flash");
  });

  // Warm up Stripe.js in the background so the pay step opens instantly.
  waitForStripe().then((Stripe) => { state.stripe = Stripe(c.publishable_key); }).catch(() => {});
}

// ── Package cards (from /api/config → src/config.js DEFAULT_PACKAGES) ────────
function renderPackages() {
  const { packages, unit_amount, currency } = state.config;
  const wrap = $("packages");
  wrap.querySelectorAll(".option").forEach((n) => n.remove());
  for (const p of packages) {
    const label = document.createElement("label");
    label.className = "option";
    label.innerHTML = `
      <input type="radio" name="choice" value="pkg:${p.id}">
      <span class="option-body">
        <span class="option-head"></span>
        <span class="option-qty"><span class="q"></span> <small>coins</small></span>
        <span class="option-price"></span>
      </span>`;
    // textContent (not innerHTML) for server-provided strings
    label.querySelector(".option-head").textContent = p.label;
    label.querySelector(".q").textContent = formatInt(p.quantity);
    label.querySelector(".option-price").textContent = formatMoney(p.quantity * unit_amount, currency);
    if (p.popular) {
      const tag = document.createElement("span");
      tag.className = "tag";
      tag.textContent = "Popular";
      label.appendChild(tag);
    }
    wrap.appendChild(label);
  }
  const popular = packages.find((p) => p.popular) || packages[0];
  if (popular) {
    wrap.querySelector(`input[value="pkg:${popular.id}"]`).checked = true;
    state.choice = { kind: "package", id: popular.id };
  }
  updateSummary();
}

function wireForm() {
  const form = $("buy-form");
  form.addEventListener("change", (e) => {
    if (e.target.name !== "choice") return;
    const v = e.target.value;
    state.choice = v === "custom" ? { kind: "custom" } : { kind: "package", id: v.slice(4) };
    if (v === "custom") $("custom-qty").focus();
    setFieldError("");
    updateSummary();
  });

  const qty = $("custom-qty");
  qty.addEventListener("focus", () => {
    $("choice-custom").checked = true;
    state.choice = { kind: "custom" };
    updateSummary();
  });
  qty.addEventListener("input", () => {
    const digits = qty.value.replace(/[^\d]/g, "").slice(0, 9); // "2,500" → "2500"
    if (digits !== qty.value) qty.value = digits;
    setFieldError("");
    updateSummary();
  });
  qty.addEventListener("blur", () => { if (state.choice?.kind === "custom") validateCustom(true); });

  form.addEventListener("submit", (e) => { e.preventDefault(); startCheckout(); });
  $("back").addEventListener("click", backToSelect);
}

/** Returns the custom quantity, or null (optionally showing why). */
function validateCustom(show) {
  const { min_quantity: min, max_quantity: max } = state.config;
  const raw = $("custom-qty").value.trim();
  const n = Number(raw);
  let msg = "";
  if (raw === "") msg = "Enter how many coins you need.";
  else if (!Number.isSafeInteger(n)) msg = "Enter a whole number.";
  else if (n < min) msg = `Minimum is ${formatInt(min)} coins.`;
  else if (n > max) msg = `Maximum is ${formatInt(max)} coins. Contact us for larger orders.`;
  if (show) setFieldError(msg);
  return msg ? null : n;
}

function selectedQuantity() {
  if (!state.choice) return null;
  if (state.choice.kind === "package") return state.config.packages.find((p) => p.id === state.choice.id)?.quantity ?? null;
  return validateCustom(false);
}

function updateSummary() {
  const q = selectedQuantity();
  const { unit_amount, currency } = state.config;
  $("summary-qty").textContent = q ? `${formatInt(q)} coins` : "—";
  $("summary-total").textContent = q ? formatMoney(q * unit_amount, currency) : "—";
  $("continue").disabled = !q || state.busy;
}

function setFieldError(msg) {
  $("custom-error").textContent = msg;
  if (msg) $("custom-qty").setAttribute("aria-invalid", "true");
  else $("custom-qty").removeAttribute("aria-invalid");
}

function setBusy(on) {
  state.busy = on;
  $("continue").classList.toggle("is-loading", on);
  $("continue").querySelector(".btn-label").textContent = on ? "Preparing secure checkout…" : "Continue to payment";
  $("buy-form").querySelectorAll("input").forEach((i) => (i.disabled = on));
  updateSummary();
}

// ── Continue → create session on our server → mount Stripe's form ─────────────
async function startCheckout() {
  if (state.busy) return;
  hideBanner();
  setFieldError("");

  // The body carries only the CHOICE. No price, no organisation — the server decides both.
  const body = {};
  if (state.choice?.kind === "custom") {
    const n = validateCustom(true);
    if (!n) { $("custom-qty").focus(); return; }
    body.quantity = n;
  } else if (state.choice?.kind === "package") {
    body.package_id = state.choice.id;
  } else {
    return;
  }

  // Same purchase → same key, so a retry after a timeout reuses the same Stripe session.
  const fingerprint = JSON.stringify(body);
  if (state.idemFor !== fingerprint) {
    state.idemKey = crypto.randomUUID();
    state.idemFor = fingerprint;
  }

  setBusy(true);
  try {
    // In parallel: our server creates the session, and Stripe.js finishes loading.
    const [session, Stripe] = await Promise.all([
      api("/api/checkout-sessions", { method: "POST", body, headers: { "Idempotency-Key": state.idemKey } }),
      state.stripe ? Promise.resolve(null) : waitForStripe(),
    ]);
    if (!state.stripe) state.stripe = Stripe(state.config.publishable_key);
    await mountCheckout(session);
    state.idemKey = null; // success → the next purchase gets a fresh key
    state.idemFor = null;
    Shell.loadOrders(); // shows the new "Awaiting payment" row
  } catch (err) {
    handleCheckoutError(err);
  } finally {
    setBusy(false);
  }
}

function handleCheckoutError(err) {
  if (err.code === "not_signed_in") {
    showBanner(err, { title: "Your session has ended" });
    return;
  }
  if (err.code === "validation_failed" && err.fields?.[0]?.field === "quantity") {
    setFieldError(err.fields[0].message);
    return;
  }
  showBanner(err, { onRetry: err.retryable ? startCheckout : null });
}

async function mountCheckout(session) {
  destroyCheckout(); // Stripe allows only one embedded checkout at a time
  try {
    // Stripe.js uses the client_secret to load THIS session (amount, line items) into an iframe.
    state.checkout = await state.stripe.createEmbeddedCheckoutPage({
      fetchClientSecret: async () => session.client_secret,
    });
  } catch (err) {
    console.error("Stripe embedded checkout failed", err);
    throw new ApiError({ code: "stripe_init_failed", retryable: true,
      message: "The secure payment form couldn't start. Please try again." });
  }
  const q = session.quote;
  $("pay-summary").textContent = `${formatInt(q.quantity)} coins · ${formatMoney(q.total, q.currency)}`;
  $("step-select").hidden = true;
  $("step-pay").hidden = false;
  state.checkout.mount("#checkout");
  // After payment Stripe redirects the tab to return.html?session_id=… (return_url set by the server).
  $("topup").scrollIntoView({ behavior: "smooth", block: "start" });
}

function destroyCheckout() {
  if (state.checkout) {
    try { state.checkout.destroy(); } catch { /* already gone */ }
    state.checkout = null;
  }
}

function backToSelect() {
  destroyCheckout();
  $("step-pay").hidden = true;
  $("step-select").hidden = false;
  hideBanner();
}
