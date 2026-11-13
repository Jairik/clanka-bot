// HeroUI themes key off data-theme/class, not the media query. Follow the OS;
// ?theme=light|dark overrides it.
(function () {
  var q = new URLSearchParams(location.search).get("theme");
  var mq = matchMedia("(prefers-color-scheme: dark)");
  function apply() {
    var t = q === "light" || q === "dark" ? q : mq.matches ? "dark" : "light";
    document.documentElement.dataset.theme = t;
    document.documentElement.className = t;
  }
  apply();
  mq.addEventListener("change", apply);
})();
