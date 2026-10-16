// Dashboard: shows the Bunk Coin balance. "+" and "Top up Bunk Coin" are plain
// links to /topup.html (the purchase page) — no JavaScript needed for them.

document.addEventListener("DOMContentLoaded", async () => {
  try {
    await Shell.init();
  } catch (err) {
    showBanner(err, { title: "Couldn't load your balance", onRetry: () => location.reload() });
  }
});
