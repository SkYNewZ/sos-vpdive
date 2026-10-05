// sos-vpdive: the only script of both sites (spec §9.1). It enhances pages
// that work without it; only the anti-robot check needs JavaScript.
"use strict";

// Member form: show the fields of the chosen category only. Hidden
// fieldsets are disabled too, so that their required fields never block
// sending, and their values are not sent.
(() => {
  const select = document.querySelector("[data-category-select]");
  if (!select) return;
  const sync = () => {
    for (const fieldset of document.querySelectorAll("fieldset[data-category]")) {
      const chosen = fieldset.dataset.category === select.value;
      fieldset.hidden = !chosen;
      fieldset.disabled = !chosen;
    }
  };
  select.addEventListener("change", sync);
  sync();
})();
