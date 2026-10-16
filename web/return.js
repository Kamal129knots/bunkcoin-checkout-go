// Stripe redirects here after the payment attempt: /return.html?session_id=cs_...
//
// This page only DISPLAYS status. It asks our server (GET /api/checkout-sessions/:id),
// which combines two answers:
//   payment_status  ← from Stripe        ("was it paid?")
//   fulfilment      ← from our order store ("were the coins credited?" — set by the webhook)
// Crediting itself happens in the webhook, even if this page is never opened.

const $ = (id) => document.getElementById(id);
const POLL_INTERVAL_MS = 2000; // re-check every 2 s…
const POLL_MAX = 8;            // …up to 8 times (~16 s) while waiting for the webhook

document.addEventListener("DOMContentLoaded", async () => {
  let shell;
  try {
    shell = await Shell.init();
  } catch (err) {
    showBanner(err, { onRetry: () => location.reload() });
    return;
  }
  if (!shell.me.signed_in) {
    $("app-area").hidden = true; // purchases are private to the organisation that made them
    return;
  }
  check(0);
});

/** Writes the result into the placeholders in return.html. */
function render(icon, title, body, action) {
  $("r-icon").textContent = icon;
  $("r-title").textContent = title;
  $("r-body").textContent = body;
  const a = $("r-action");
  a.hidden = !action;
  if (action) { a.textContent = action.label; a.href = action.href; }
}

/** Asks the server about this payment and shows the matching message. Polls while crediting. */
async function check(attempt) {
  // 1. Which payment? Stripe put the id in the URL.
  const id = new URLSearchParams(location.search).get("session_id");
  if (!id || !/^cs_(test|live)_[A-Za-z0-9]+$/.test(id)) {
    render("⚠️", "Missing payment reference",
      "This page needs a valid checkout reference. If you just paid, check your email for a receipt.",
      { label: "Top up Bunk Coin", href: "/topup.html" });
    return;
  }
  $("r-ref").textContent = "";

  // 2. Ask our server.
  let s;
  try {
    s = await api(`/api/checkout-sessions/${encodeURIComponent(id)}`);
  } catch (err) {
    if (err.code === "not_found" || err.code === "invalid_session_id") {
      render("⚠️", "Payment not found", "This payment doesn't belong to your organisation, or the link is wrong.",
        { label: "Top up Bunk Coin", href: "/topup.html" });
      return;
    }
    if (err.code === "not_signed_in") { showBanner(err, { title: "Your session has ended" }); return; }
    render("⚠️", "We couldn't check your payment", "If you were charged, your coins will still be credited.");
    showBanner(err, { onRetry: () => check(0) });
    return;
  }

  const amount = s.currency ? formatMoney(s.amount_total, s.currency) : "";
  const coins = `${formatInt(s.quantity)} Bunk Coin`;
  const email = s.customer_email ? ` A receipt was sent to ${s.customer_email}.` : "";
  if (s.order_id) $("r-ref").textContent = `Order #${s.order_id.slice(-6)}`;

  // 3. Decide what to show.
  if (s.status === "complete" && s.payment_status === "paid") {
    switch (s.fulfilment) {
      case "fulfilled": {
        const bal = s.balance_after != null ? ` Your balance is now ${formatInt(s.balance_after)}.` : "";
        render("✅", "Payment successful", `${coins} (${amount}) added to your organisation.${bal}${email}`,
          { label: "Back to dashboard", href: "/" });
        Shell.refreshMe();
        Shell.loadOrders();
        return;
      }
      case "needs_review":
        render("🛟", "Payment received — we're finalising your coins",
          `Your payment of ${amount} went through, but we couldn't add the coins automatically. Our team has been alerted and will sort it out — no need to pay again.`,
          { label: "Back to dashboard", href: "/" });
        Shell.loadOrders();
        return;
      default: // pending / credit_retrying / unknown → the webhook hasn't credited yet
        if (attempt < POLL_MAX) {
          const msg = s.fulfilment === "credit_retrying"
            ? `Payment received. Crediting ${coins} is taking a little longer than usual…`
            : `Crediting ${coins} to your account…`;
          render("⏳", "Payment received", msg);
          setTimeout(() => check(attempt + 1), POLL_INTERVAL_MS);
          return;
        }
        render("✅", "Payment received",
          `Your payment went through. ${coins} will be added as soon as our system confirms it — it shows as "Payment received" in your history until then.${email} No need to pay again.`,
          { label: "Back to dashboard", href: "/" });
        Shell.loadOrders();
        return;
    }
  }

  if (s.status === "complete") { // delayed method (bank debit): complete but not cleared
    render("🕒", "Payment processing",
      `Your payment of ${amount} is being processed by your bank. We'll add ${coins} as soon as it clears.${email}`,
      { label: "Back to dashboard", href: "/" });
    return;
  }
  if (s.status === "expired") {
    render("⌛", "Checkout expired", "This checkout expired before payment was completed. You haven't been charged.",
      { label: "Start again", href: "/topup.html" });
    return;
  }
  // "open": came back without finishing (failed authentication, cancelled)
  render("❌", "Payment not completed", "Your payment didn't go through and you haven't been charged.",
    { label: "Try again", href: "/topup.html" });
}
