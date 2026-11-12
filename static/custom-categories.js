// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
document.addEventListener("DOMContentLoaded", () => {
  const { text: T, html: H } = window.LensI18n;
  const form = document.getElementById("queue-job-form");
  const enabled = document.getElementById("custom-categories-enabled");
  if (!form || !enabled) return;
  const editor = document.getElementById("custom-category-editor");
  const list = document.getElementById("custom-category-list");
  const add = document.getElementById("add-custom-category");
  const output = document.getElementById("custom-categories-json");
  const storageKey = "lens.customCategories.v1";
  const modelSelect = document.getElementById("decision-model-select");
  const modelKey = "lens.decisionModel.v1";

  function definitions() {
    return { categories: Array.from(list.children).map((row) => {
      const category = {
        name: row.querySelector("[data-field=name]").value.trim(),
        type: row.querySelector("[data-field=type]").value,
        question: row.querySelector("[data-field=question]").value.trim(),
      };
      if (category.type === "choice") {
        category.options = row.querySelector("[data-field=options]").value.split("\n").map(s => s.trim()).filter(Boolean);
      }
      return category;
    }) };
  }

  function sync() {
    editor.hidden = !enabled.checked;
    for (const row of list.children) {
      const choice = row.querySelector("[data-field=type]").value === "choice";
      row.querySelector("[data-options]").hidden = !choice;
      row.querySelectorAll("input, textarea, select").forEach(field => {
        field.disabled = !enabled.checked || (field.dataset.field === "options" && !choice);
        field.setCustomValidity("");
      });
    }
    add.disabled = list.children.length >= 12;
    const spec = definitions();
    output.value = enabled.checked ? JSON.stringify(spec) : "";
    try { localStorage.setItem(storageKey, JSON.stringify(spec)); } catch (_) { /* Storage is optional. */ }
  }

  const spriteHref =
    document.querySelector('use[href*="core-icons.svg"]')?.getAttribute("href")?.split("#")[0] || "";

  function addCategory(value = {}) {
    if (list.children.length >= 12) return;
    const row = document.createElement("fieldset");
    row.className = "card custom-category-card";
    const selectIcon = spriteHref
      ? `<svg class="select-icon" aria-hidden="true"><use href="${spriteHref.replaceAll('"', "&quot;")}#chevron-down"></use></svg>`
      : "";
    // Static markup only; user values are assigned through DOM properties.
    row.innerHTML = `
      <legend>${H("Category")}</legend>
      <label class="field run-advanced-field"><span class="run-advanced-label">${H("Category name · CSV column label")}</span>
        <input data-field="name" required maxlength="48" pattern="[A-Za-z][A-Za-z0-9 _\\-]{0,47}" placeholder="${H("e.g. Brand safe")}" title="${H("Start with a letter; use letters, digits, spaces, underscores or hyphens.")}"></label>
      <label class="field run-advanced-field"><span class="run-advanced-label">${H("Answer format")}</span>
        <div class="select-wrap"><select data-field="type"><option value="boolean">${H("Yes / no · independent label")}</option><option value="choice">${H("Multiple choice · select one option")}</option></select>${selectIcon}</div></label>
      <label class="field run-advanced-field"><span class="run-advanced-label">${H("What should the model decide?")}</span>
        <textarea data-field="question" required maxlength="1000" rows="3" placeholder="${H("Would a mainstream brand be comfortable appearing beside this content? Judge trust and respect for the audience, not topic alone.")}"></textarea></label>
      <label class="field run-advanced-field" data-options hidden><span class="run-advanced-label">${H("Options · 2–12 choices, one per line")}</span>
        <textarea data-field="options" required maxlength="972" rows="4" placeholder="${H("News\nEntertainment\nShopping\nOther\nUnknown")}"></textarea></label>
      <button type="button" class="btn btn-ghost btn-sm">${H("Remove category")}</button>`;
    for (const field of ["name", "question"]) row.querySelector(`[data-field=${field}]`).value = typeof value[field] === "string" ? value[field] : "";
    row.querySelector("[data-field=type]").value = value.type === "choice" ? "choice" : "boolean";
    row.querySelector("[data-field=options]").value = Array.isArray(value.options) ? value.options.join("\n") : "";
    row.querySelector("button").addEventListener("click", () => {
      row.remove();
      if (!list.children.length) enabled.checked = false;
      sync();
    });
    row.addEventListener("input", sync);
    row.addEventListener("change", sync);
    list.append(row);
  }

  try {
    const saved = JSON.parse(localStorage.getItem(storageKey) || "null");
    if (Array.isArray(saved?.categories)) saved.categories.slice(0, 12).forEach(c => addCategory(c || {}));
  } catch (_) { /* Ignore invalid local preferences. */ }
  try {
    const savedModel = localStorage.getItem(modelKey);
    if (modelSelect && Array.from(modelSelect.options).some(o => o.value === savedModel)) modelSelect.value = savedModel;
  } catch (_) { /* Storage is optional. */ }
  modelSelect?.addEventListener("change", () => {
    try { localStorage.setItem(modelKey, modelSelect.value); } catch (_) { /* Storage is optional. */ }
  });
  // Definitions persist, but sending data to the decision model is opt-in for every run.
  sync();
  enabled.addEventListener("change", () => {
    if (enabled.checked && !list.children.length) addCategory();
    sync();
  });
  add.addEventListener("click", () => {
    addCategory();
    sync();
    list.lastElementChild.querySelector("input").focus();
  });
  form.addEventListener("submit", (event) => {
    sync();
    if (!enabled.checked) return;
    const spec = definitions();
    const names = new Set();
    for (let i = 0; i < spec.categories.length; i++) {
      const category = spec.categories[i];
      const row = list.children[i];
      const name = row.querySelector("[data-field=name]");
      if (!category.name || names.has(category.name.toLowerCase())) name.setCustomValidity(T("Give each category a unique name."));
      names.add(category.name.toLowerCase());
      if (!category.question) row.querySelector("[data-field=question]").setCustomValidity(T("Enter a question."));
      if (category.type === "choice") {
        const options = category.options;
        if (options.length < 2 || options.length > 12 || new Set(options.map(o => o.toLowerCase())).size !== options.length || options.some(o => o.length > 80 || /^[=+@-]/.test(o))) {
          row.querySelector("[data-field=options]").setCustomValidity(T("Enter 2–12 unique options, at most 80 characters each, not starting with =, +, - or @."));
        }
      }
    }
    if (!form.reportValidity()) {
      event.preventDefault();
      event.stopImmediatePropagation();
    }
  }, true);
});
