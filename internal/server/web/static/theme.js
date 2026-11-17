// Applies the saved theme before first paint (loaded synchronously in <head>).
(function () {
  try {
    var t = JSON.parse(localStorage.getItem("cothon:theme"));
    if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
  } catch (e) {}
})();
