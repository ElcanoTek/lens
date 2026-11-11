// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
document.addEventListener("DOMContentLoaded", () => {
  const { text: T, html: H, number: N, cost: C, costTitle: CT, locale } = window.LensI18n;
  const nativeInput = document.getElementById("input-file-native");
  const fileDropzone = document.getElementById("fileDropzone");
  const dropzoneStatus = document.getElementById("dropzone-status");
  const fileError = document.getElementById("fileError");
  const inputUploadForm = document.getElementById("input-upload-form");
  const queueJobForm = document.getElementById("queue-job-form");
  const queueJobBtn = document.getElementById("queue-job-btn");
  const deleteInputForm = document.getElementById("delete-input-form");
  const deleteInputFilename = document.getElementById("delete-input-filename");
  const monitorModal = document.getElementById("monitor-modal");
  const monitorModalClose = document.getElementById("monitor-modal-close");
  const monitorJobId = document.getElementById("monitor-job-id");
  const monitorJobInput = document.getElementById("monitor-job-input");
  const monitorJobTiming = document.getElementById("monitor-job-timing");
  const monitorJobStatus = document.getElementById("monitor-job-status");
  const liveProgressSummary = document.getElementById("live-progress-summary");
  const liveLog = document.getElementById("live-log");
  const confirmModal = document.getElementById("confirm-delete-modal");
  const confirmModalTitle = document.getElementById("confirm-modal-title");
  const confirmModalText = document.getElementById("confirm-delete-text");
  const confirmModalAction = document.getElementById("confirm-modal-action");
  const confirmModalCancel = document.getElementById("confirm-modal-cancel");
  const toastRoot = document.getElementById("toast-root");
  const flashData = document.getElementById("flash-data");

// Progress-bar segments carry their width as data-pct and get it applied
// here through the CSSOM. Inline style attributes would need CSP
// style-src 'unsafe-inline'; element.style assignments do not.
function applySegmentWidths(root) {
  const scope = root || document;
  scope.querySelectorAll("[data-pct]").forEach((el) => {
    const pct = Number.parseFloat(el.dataset.pct);
    if (Number.isFinite(pct)) el.style.width = `${Math.max(0, Math.min(100, pct))}%`;
  });
}
// This handler is already running because DOMContentLoaded fired. A second
// listener registered from inside it never runs, so the file-mix bars and the
// run status bars stayed at width 0.
applySegmentWidths(document);

  let activeMonitorJobId = null;
  let queuePollInFlight = false;

  function escapeHtml(value) {
    return String(value)
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;")
      .replaceAll("'", "&#39;");
  }

  // ── Timestamps: render server UTC stamps in the viewer's local time ──

  function parseUtcTimestamp(rawValue) {
    const raw = String(rawValue || "").trim();
    if (!raw) {
      return null;
    }

    const parsed = new Date(raw);
    if (!Number.isNaN(parsed.getTime())) {
      return parsed;
    }

    const compact = raw.match(/(\d{14})/);
    if (!compact) {
      return null;
    }

    const value = compact[1];
    return new Date(Date.UTC(
      Number(value.slice(0, 4)),
      Number(value.slice(4, 6)) - 1,
      Number(value.slice(6, 8)),
      Number(value.slice(8, 10)),
      Number(value.slice(10, 12)),
      Number(value.slice(12, 14)),
    ));
  }

  function localizeDisplayedTimestamps() {
    const nodes = Array.from(document.querySelectorAll(".js-local-timestamp[data-utc-timestamp]"));
    const parsed = nodes
      .map((node) => ({ node, date: parseUtcTimestamp(node.getAttribute("data-utc-timestamp")) }))
      .filter((entry) => entry.date);

    if (parsed.length === 0) {
      return;
    }

    const includeYear = new Set(parsed.map((entry) => entry.date.getFullYear())).size > 1;
    parsed.forEach(({ node, date }) => {
      node.textContent = new Intl.DateTimeFormat(locale, {
        month: "short",
        day: "numeric",
        hour: "numeric",
        minute: "2-digit",
        hour12: locale === "en-US",
        ...(includeYear ? { year: "numeric" } : {}),
      }).format(date);
      node.title = `${new Intl.DateTimeFormat(locale, {
        month: "short",
        day: "numeric",
        year: "numeric",
        hour: "numeric",
        minute: "2-digit",
        hour12: locale === "en-US",
        timeZone: "UTC",
      }).format(date)} UTC`;
    });
  }

  // ── Toasts + confirm modal ──

  function showToast(text, tone = "info") {
    if (!toastRoot || !text) {
      return;
    }
    const toast = document.createElement("div");
    toast.className = `toast toast-${tone}`;
    toast.innerHTML = `<span>${escapeHtml(text)}</span><button type="button" class="toast-close" aria-label="${H("Dismiss notification")}">×</button>`;
    toastRoot.appendChild(toast);

    const remove = () => {
      toast.classList.add("toast-leave");
      setTimeout(() => toast.remove(), 180);
    };
    toast.querySelector(".toast-close").addEventListener("click", remove);
    setTimeout(remove, 5200);
  }

  // One modal serves every confirmation, so each open sets every label and
  // the confirm button's tone (destructive actions keep the danger red and
  // "Keep"; sign-out is primary with "Cancel"), and clears the previous
  // result: Escape closes without setting one, and a stale "confirm" left
  // from an earlier open must not count as a yes. A cancelled confirmation
  // puts focus back on the control that opened it (Safari does not focus a
  // clicked button, so the dialog alone would return focus to the page).
  function openConfirmModal({ title, message, confirmLabel = T("Confirm"), cancelLabel = T("Keep"), tone = "danger", opener, submitCallback }) {
    if (!confirmModal || !confirmModalText) {
      submitCallback();
      return;
    }
    if (confirmModalTitle) {
      confirmModalTitle.textContent = title;
    }
    if (confirmModalAction) {
      confirmModalAction.textContent = confirmLabel;
      confirmModalAction.classList.toggle("btn-danger", tone === "danger");
      confirmModalAction.classList.toggle("btn-primary", tone === "primary");
    }
    if (confirmModalCancel) {
      confirmModalCancel.textContent = cancelLabel;
    }
    confirmModalText.textContent = message;
    confirmModal.returnValue = "";
    confirmModal.showModal();
    confirmModal.addEventListener(
      "close",
      () => {
        if (confirmModal.returnValue === "confirm") {
          submitCallback();
        } else if (opener && opener.isConnected) {
          opener.focus();
        }
      },
      { once: true },
    );
  }

  function confirmFormSubmit(form, options) {
    form.addEventListener("submit", (event) => {
      if (form.dataset.confirmed === "yes") {
        delete form.dataset.confirmed;
        return;
      }
      event.preventDefault();
      openConfirmModal({
        ...options,
        opener: event.submitter || form.querySelector('[type="submit"]'),
        submitCallback: () => {
          form.dataset.confirmed = "yes";
          form.requestSubmit();
        },
      });
    });
  }

  // ── Upload: drop or pick a file and it uploads immediately ──

  function startUpload() {
    if (!inputUploadForm || !nativeInput || nativeInput.files.length === 0) {
      return;
    }
    if (fileError) {
      fileError.textContent = "";
    }
    if (dropzoneStatus) {
      const names = Array.from(nativeInput.files).map((file) => file.name).join(", ");
      dropzoneStatus.textContent = T("Uploading {0}…",names);
    }
    if (fileDropzone) {
      fileDropzone.classList.add("is-uploading");
    }
    inputUploadForm.submit();
  }

  if (nativeInput) {
    nativeInput.addEventListener("change", startUpload);
  }

  if (fileDropzone && nativeInput) {
    fileDropzone.addEventListener("click", () => nativeInput.click());
    fileDropzone.addEventListener("keydown", (event) => {
      if (event.key === "Enter" || event.key === " ") {
        event.preventDefault();
        nativeInput.click();
      }
    });

    ["dragenter", "dragover"].forEach((eventName) => {
      fileDropzone.addEventListener(eventName, (event) => {
        event.preventDefault();
        fileDropzone.classList.add("is-dragover");
      });
    });

    ["dragleave", "drop"].forEach((eventName) => {
      fileDropzone.addEventListener(eventName, (event) => {
        event.preventDefault();
        fileDropzone.classList.remove("is-dragover");
      });
    });

    fileDropzone.addEventListener("drop", (event) => {
      const dropped = event.dataTransfer && event.dataTransfer.files;
      if (dropped && dropped.length > 0) {
        nativeInput.files = dropped;
        startUpload();
      }
    });
  }

  // ── Run + input-file actions ──

  if (queueJobForm && queueJobBtn) {
    queueJobForm.addEventListener("submit", () => {
      queueJobBtn.disabled = true;
      queueJobBtn.textContent = T("Starting…");
    });
  }

  // ── File card selection + ready section ─────────────────────────────

  function parseBreakdown(card) {
    if (!card || !card.dataset.breakdown) return null;
    try {
      return JSON.parse(card.dataset.breakdown);
    } catch (e) {
      return null;
    }
  }

  function updateReadySection() {
    const checkedRadio = document.querySelector('input[name="input_file"]:checked');
    const emptyEl = document.getElementById("ready-empty");
    const contentEl = document.getElementById("ready-content");
    const fileNameEl = document.getElementById("ready-file-name");
    const compBarEl = document.getElementById("ready-comp-bar");
    const rowsEl = document.getElementById("ready-rows");
    const fileTypesEl = document.getElementById("ready-file-types");

    document.querySelectorAll(".filecard").forEach((card) => {
      const r = card.querySelector('input[name="input_file"]');
      card.classList.toggle("is-selected", !!(r && r.checked));
    });

    if (checkedRadio) {
      const card = checkedRadio.closest(".filecard");
      if (emptyEl) emptyEl.hidden = true;
      if (contentEl) contentEl.hidden = false;
      if (fileNameEl) fileNameEl.textContent = checkedRadio.value;

      const data = parseBreakdown(card);
      const types = (data && data.types) || [];
      if (compBarEl) {
        compBarEl.innerHTML = types
          .map((t) => `<span class="comp-seg ${t.seg}" data-pct="${t.pct}"></span>`)
          .join("");
        applySegmentWidths(compBarEl);
        compBarEl.hidden = types.length === 0;
      }
      if (rowsEl) {
        rowsEl.innerHTML = `<strong>${H(data?.total === 1 ? "1 row" : "{0} rows", data?.total || 0)}</strong>`;
        rowsEl.hidden = types.length === 0;
      }
      if (fileTypesEl) {
        fileTypesEl.innerHTML = types
          .map(
            (t) =>
              `<span class="tchip"><span class="tdot ${t.seg}"></span>${H(t.label)} <b>${N(t.count)}</b></span>`
          )
          .join("");
      }
      if (queueJobBtn) queueJobBtn.disabled = false;
    } else {
      if (emptyEl) emptyEl.hidden = false;
      if (contentEl) contentEl.hidden = true;
      if (queueJobBtn) queueJobBtn.disabled = true;
    }
  }

  document.querySelectorAll(".filecard").forEach((card) => {
    card.addEventListener("click", (e) => {
      if (e.target.closest(".fc-actions")) return;
      const radio = card.querySelector('input[name="input_file"]');
      if (radio && !radio.checked) {
        radio.checked = true;
        updateReadySection();
      }
    });
  });

  document.querySelectorAll('input[name="input_file"]').forEach((radio) => {
    radio.addEventListener("change", updateReadySection);
  });

  // ── File search ──────────────────────────────────────────────────────

  const fileSearch = document.getElementById("file-search");
  const filesNoMatch = document.getElementById("files-no-match");
  if (fileSearch) {
    fileSearch.addEventListener("input", () => {
      const q = fileSearch.value.trim().toLowerCase();
      let visible = 0;
      document.querySelectorAll(".filecard").forEach((card) => {
        const name = (card.dataset.filename || "").toLowerCase();
        const show = !q || name.includes(q);
        card.style.display = show ? "" : "none";
        if (show) visible++;
      });
      if (filesNoMatch) filesNoMatch.hidden = visible > 0 || !q;
    });
  }

  updateReadySection();

  const renameModal = document.getElementById("rename-modal");
  const renameOldName = document.getElementById("rename-old-name");
  const renameNewName = document.getElementById("rename-new-name");
  const renameCancel = document.getElementById("rename-cancel");

  document.querySelectorAll(".js-rename-input").forEach((button) => {
    button.addEventListener("click", () => {
      const name = button.dataset.filename || "";
      if (!name || !renameModal || !renameOldName || !renameNewName) {
        return;
      }
      renameOldName.value = name;
      renameNewName.value = name;
      renameModal.showModal();
      const dot = name.lastIndexOf(".");
      renameNewName.setSelectionRange(0, dot > 0 ? dot : name.length);
      renameNewName.focus();
    });
  });

  if (renameCancel && renameModal) {
    renameCancel.addEventListener("click", () => renameModal.close());
  }

  document.querySelectorAll(".js-delete-input").forEach((button) => {
    button.addEventListener("click", () => {
      const name = button.dataset.filename || "";
      if (!name || !deleteInputForm || !deleteInputFilename) {
        return;
      }
      openConfirmModal({
        title: T("Delete input file?"),
        message: T("Delete {0}? This cannot be undone.",name),
        confirmLabel: T("Delete"),
        submitCallback: () => {
          deleteInputFilename.value = name;
          deleteInputForm.submit();
        },
      });
    });
  });

  document.querySelectorAll(".js-confirm-cancel").forEach((form) => {
    confirmFormSubmit(form, {
      title: T("Cancel run?"),
      message: T("Stop {0}? Progress so far is kept.",form.dataset.jobId || T("this run")),
      confirmLabel: T("Cancel run"),
    });
  });

  document.querySelectorAll(".js-confirm-job-delete").forEach((form) => {
    confirmFormSubmit(form, {
      title: T("Delete run?"),
      message: T("Delete {0} and its results? This cannot be undone.",form.dataset.jobId || T("this run")),
      confirmLabel: T("Delete"),
    });
  });

  // Signing out asks first. It ends the session at Auth (central mode's
  // RP-initiated logout, or the auth host's logout in elcano mode), so every
  // app is signed out, not only Lens. Without script the header button
  // still posts the same form straight away.
  document.querySelectorAll(".js-confirm-sign-out").forEach((form) => {
    confirmFormSubmit(form, {
      title: T("Sign out?"),
      message: T("You'll be signed out of all your apps, not just Lens."),
      confirmLabel: T("Sign out"),
      cancelLabel: T("Cancel"),
      tone: "primary",
    });
  });

  // ── Live progress: table rows + monitor modal ──

  function setBadge(node, statusText) {
    if (!node) {
      return;
    }
    const safe = (statusText || "idle").toLowerCase();
    node.className = `status-badge status-${safe}`;
    node.innerHTML = `<span class="status-dot"></span>${H(safe)}`;
  }

  function formatElapsed(seconds) {
    const safe = Math.max(0, Math.floor(seconds));
    if (safe < 60) {
      return T("{0} s",safe);
    }
    if (safe < 3600) {
      return T("{0} min {1} s",Math.floor(safe / 60), safe % 60);
    }
    return T("{0} h {1} min",Math.floor(safe / 3600), Math.floor((safe % 3600) / 60));
  }

  // Fallback per-item retry cost (single-session headless Chrome, ~15s per
  // site). The server sends it as progress.retry_seconds_per_item; keep in
  // sync with web_service.py.
  const DEEP_RETRY_SECONDS_PER_ITEM = 15;

  function estimateEta(progress, startedAt, status) {
    const normalizedStatus = String(status || "").toLowerCase();
    if (["completed", "failed", "cancelled", "archived"].includes(normalizedStatus)) {
      return T("Done");
    }
    if (normalizedStatus === "queued") {
      return T("Queued");
    }

    const total = Number(progress.total_domains || 0);
    const processed = Number(progress.processed || 0);
    const retrying = Number(progress.retrying || 0);
    const attempts = processed + retrying;

    if (!startedAt || total <= 0 || attempts <= 0) {
      return T("Estimating…");
    }

    const started = new Date(startedAt);
    if (Number.isNaN(started.getTime())) {
      return T("Estimating…");
    }

    const elapsed = (Date.now() - started.getTime()) / 1000;
    if (elapsed <= 5) {
      return T("Estimating…");
    }

    // Two-rate model: unattempted items at the measured fast-crawl rate,
    // plus a fixed retry-pass cost for each item queued for retry.
    const retrySeconds =
      Number(progress.retry_seconds_per_item) || DEEP_RETRY_SECONDS_PER_ITEM;
    const fastRate = elapsed / attempts;
    const remainingFresh = Math.max(0, total - attempts);
    const remaining = remainingFresh * fastRate + retrying * retrySeconds;

    if (remaining < 60) {
      return T("< 1m left");
    }
    if (remaining < 3600) {
      return T("~{0} min left",Math.round(remaining / 60));
    }
    return T("~{0} h {1} min left",Math.floor(remaining / 3600), Math.round((remaining % 3600) / 60));
  }

  function setBarSegments(container, progress) {
    if (!container) {
      return;
    }
    const total = Number(progress.total_domains || 0);
    const seg = (count) => (total > 0 ? Math.min(100, (count / total) * 100) : 0);
    const ok = container.querySelector(".bar-seg--ok");
    const retry = container.querySelector(".bar-seg--retry");
    const fail = container.querySelector(".bar-seg--fail");
    if (ok) {
      ok.style.width = `${seg(Number(progress.successful || 0))}%`;
    }
    if (retry) {
      retry.style.width = `${seg(Number(progress.retrying || 0))}%`;
    }
    if (fail) {
      fail.style.width = `${seg(Number(progress.errors || 0))}%`;
    }
  }

  function updateQueueRowProgress(job, progress) {
    if (!job || !job.id) {
      return;
    }

    const jobRow = document.querySelector(`.panel-runs tbody tr[data-job-id="${CSS.escape(String(job.id))}"]`);
    if (!jobRow) {
      return;
    }

    const total = Number(progress.total_domains || 0);
    const processed = Number(progress.processed || 0);

    setBarSegments(jobRow.querySelector(".queue-progress-bar"), progress);

    const text = jobRow.querySelector(".queue-progress-text");
    if (text) {
      const retrying = Number(progress.retrying || 0);
      if (total > 0) {
        text.textContent = retrying > 0
          ? T("{0} / {1} · {2} to retry",processed,total,retrying)
          : T("{0} / {1}",processed,total);
      } else if (processed > 0) {
        text.textContent = T("{0} processed",processed);
      } else if (job.status === "queued") {
        text.textContent = T("Waiting");
      } else {
        text.textContent = T("Starting…");
      }
    }

    const eta = jobRow.querySelector(".queue-progress-eta");
    if (eta) {
      eta.textContent = estimateEta(progress, job.started_at, job.status);
    }

    const cost = jobRow.querySelector(".queue-progress-cost");
    if (cost && progress.cost_text) {
      cost.textContent = C(progress.cost_text);
      cost.title = CT(progress.cost_title) || "";
    }

    setBadge(jobRow.querySelector(".status-badge"), job.status);
    jobRow.dataset.status = String(job.status || "").toLowerCase();
  }

  function updateLiveProgress(progress, hasJob) {
    const liveBar = document.querySelector("#live-progress .live-progress-bar");
    if (!liveProgressSummary) {
      return;
    }
    if (!hasJob) {
      setBarSegments(liveBar, {});
      liveProgressSummary.textContent = T("No progress data yet.");
      return;
    }

    const total = Number(progress.total_domains || 0);
    const processed = Number(progress.processed || 0);
    const successful = Number(progress.successful || 0);
    const errors = Number(progress.errors || 0);
    const retrying = Number(progress.retrying || 0);
    setBarSegments(liveBar, progress);

    if (total > 0) {
      const retryDetail = retrying > 0
        ? `<span class="progress-retry">${H("{0} to retry",retrying)}</span>`
        : "";
      liveProgressSummary.innerHTML =
        `<span class="progress-counter">${H("{0} / {1}",processed,total)}</span>` +
        `<span class="progress-detail">` +
        `<span class="progress-success">${H("{0} classified",successful)}</span>` +
        `<span class="progress-fail">${H("{0} failed",errors)}</span>` +
        retryDetail +
        `</span>`;
    } else if (processed > 0) {
      liveProgressSummary.textContent = T("{0} processed",processed);
    } else {
      liveProgressSummary.textContent = T("Starting…");
    }
  }

  function openMonitorModal(jobId, jobName) {
    if (!monitorModal || !jobId) {
      return;
    }
    activeMonitorJobId = jobId;
    if (monitorJobId) {
      monitorJobId.textContent = String(jobName || "").trim() || String(jobId);
    }
    if (monitorJobInput) {
      monitorJobInput.textContent = T("Input: loading…");
    }
    if (monitorJobTiming) {
      monitorJobTiming.textContent = T("Elapsed — · ETA —");
    }
    setBadge(monitorJobStatus, "queued");
    updateLiveProgress({}, false);
    if (liveLog) {
      liveLog.textContent = T("Loading log output…");
      liveLog.classList.remove("is-active");
    }
    const completionBanner = document.getElementById("monitor-completion-banner");
    if (completionBanner) {
      completionBanner.innerHTML = "";
      completionBanner.hidden = true;
    }
    if (!monitorModal.open) {
      monitorModal.showModal();
    }
    pollActiveJob();
  }

  async function pollActiveJob() {
    if (!monitorModal || !monitorModal.open || !activeMonitorJobId) {
      return;
    }

    const query = `?job_id=${encodeURIComponent(activeMonitorJobId)}`;
    const response = await fetch(`/api/jobs/live${query}`, { credentials: "same-origin" });
    if (!response.ok) {
      if (monitorJobTiming) {
        monitorJobTiming.textContent = T("Live view unavailable");
      }
      updateLiveProgress({}, false);
      return;
    }

    const payload = await response.json();
    if (!payload.job) {
      if (monitorJobTiming) {
        monitorJobTiming.textContent = T(payload.message || "No active data");
      }
      setBadge(monitorJobStatus, "idle");
      updateLiveProgress({}, false);
      if (liveLog) {
        liveLog.textContent = T("No log output yet.");
        liveLog.classList.remove("is-active");
      }
      return;
    }

    if (monitorJobId) {
      monitorJobId.textContent = String(payload.job.name || "").trim() || String(payload.job.id);
    }
    if (monitorJobInput) {
      monitorJobInput.textContent = T("Input: {0}",payload.job.input_file || T("unknown"));
    }
    if (monitorJobTiming) {
      const normalizedStatus = String(payload.job.status || "").toLowerCase();
      const isTerminal = ["completed", "failed", "cancelled"].includes(normalizedStatus);
      const started = payload.job.started_at ? new Date(payload.job.started_at) : null;
      const finished = payload.job.finished_at ? new Date(payload.job.finished_at) : null;
      let elapsed = "—";
      if (isTerminal && started && finished && !Number.isNaN(started.getTime()) && !Number.isNaN(finished.getTime())) {
        elapsed = formatElapsed((finished.getTime() - started.getTime()) / 1000);
      } else if (started && !Number.isNaN(started.getTime())) {
        elapsed = formatElapsed((Date.now() - started.getTime()) / 1000);
      }
      const eta = estimateEta(payload.progress || {}, payload.job.started_at, payload.job.status);
      monitorJobTiming.textContent = T("Elapsed {0} · ETA {1}",elapsed,eta);
    }

    updateQueueRowProgress(payload.job, payload.progress || {});
    setBadge(monitorJobStatus, payload.job.status);
    updateLiveProgress(payload.progress || {}, true);

    const lines = payload.log_lines || [];
    if (liveLog) {
      liveLog.textContent = lines.length ? lines.join("\n") : T("No log output yet.");
      liveLog.scrollTop = liveLog.scrollHeight;
      liveLog.classList.toggle("is-active", payload.job.status === "running");
    }

    const completionBanner = document.getElementById("monitor-completion-banner");
    if (completionBanner) {
      const normalizedStatus = String(payload.job.status || "").toLowerCase();
      if (["completed", "failed", "cancelled"].includes(normalizedStatus)) {
        const prog = payload.progress || {};
        const s = Number(prog.successful || 0);
        const e = Number(prog.errors || 0);
        const t = Number(prog.total_domains || 0);
        const started = payload.job.started_at ? new Date(payload.job.started_at) : null;
        const finished = payload.job.finished_at ? new Date(payload.job.finished_at) : null;
        let durationText = "—";
        if (started && finished && !Number.isNaN(started.getTime()) && !Number.isNaN(finished.getTime())) {
          durationText = formatElapsed((finished.getTime() - started.getTime()) / 1000);
        }

        completionBanner.innerHTML =
          `<div class="completion-grid">` +
          `<div class="completion-stat"><span class="completion-label">${H("Total")}</span><span class="completion-value">${N(t)}</span></div>` +
          `<div class="completion-stat"><span class="completion-label">${H("Succeeded")}</span><span class="completion-value completion-success">${N(s)}</span></div>` +
          `<div class="completion-stat"><span class="completion-label">${H("Failed")}</span><span class="completion-value completion-fail">${N(e)}</span></div>` +
          `<div class="completion-stat"><span class="completion-label">${H("Duration")}</span><span class="completion-value">${durationText}</span></div>` +
          (prog.cost_text
            ? `<div class="completion-stat" title="${escapeHtml(CT(prog.cost_title))}"><span class="completion-label">${H("Cost")}</span><span class="completion-value">${escapeHtml(C(prog.cost_text))}</span></div>`
            : "") +
          `</div>`;
        completionBanner.hidden = false;
      } else {
        completionBanner.innerHTML = "";
        completionBanner.hidden = true;
      }
    }
  }

  async function pollQueueJobs() {
    const hasActiveRows = document.querySelector(
      '.panel-runs tbody tr[data-status="queued"], .panel-runs tbody tr[data-status="running"], .panel-runs tbody tr[data-status="cancelling"]',
    );
    if (!hasActiveRows || queuePollInFlight) {
      return;
    }

    queuePollInFlight = true;
    try {
      const response = await fetch("/api/jobs/queue", { credentials: "same-origin" });
      if (!response.ok) {
        return;
      }

      const payload = await response.json();
      const jobs = Array.isArray(payload.jobs) ? payload.jobs : [];
      jobs.forEach((job) => {
        updateQueueRowProgress(job, job.progress || {});
      });
    } finally {
      queuePollInFlight = false;
    }
  }

  if (monitorModalClose && monitorModal) {
    monitorModalClose.addEventListener("click", () => {
      monitorModal.close();
    });
    monitorModal.addEventListener("close", () => {
      activeMonitorJobId = null;
    });
  }

  document.querySelectorAll(".js-view-job").forEach((button) => {
    button.addEventListener("click", () => {
      openMonitorModal(button.dataset.jobId, button.dataset.jobName);
    });
  });

  pollQueueJobs();
  setInterval(() => {
    pollActiveJob().catch(() => {});
    pollQueueJobs().catch(() => {});
  }, 2000);

  // ── Flash messages from redirects ──

  if (flashData) {
    const message = flashData.dataset.message || "";
    const error = flashData.dataset.error || "";
    const filesMessage = flashData.dataset.filesMessage || "";
    const filesError = flashData.dataset.filesError || "";
    if (message) {
      showToast(message, "success");
    }
    if (error) {
      showToast(error, "error");
    }
    if (filesMessage) {
      showToast(filesMessage, "success");
    }
    if (filesError) {
      showToast(filesError, "error");
      if (fileError) {
        fileError.textContent = filesError;
      }
    }
  }

  const url = new URL(window.location.href);
  const flashParams = ["message", "error", "files_message", "files_error", "files_scope"];
  if (flashParams.some((param) => url.searchParams.has(param))) {
    flashParams.forEach((param) => url.searchParams.delete(param));
    window.history.replaceState({}, "", `${url.pathname}${url.search ? url.search : ""}`);
  }

  localizeDisplayedTimestamps();

  // ── Advanced settings persistence ──────────────────────────────────
  // Remember the model / research-fallback choice per browser so a tweak
  // sticks for the next run instead of silently resetting to defaults.
  (function persistAdvancedSettings() {
    const storageKey = "lens-advanced-settings";
    const modelSelect = document.getElementById("llm-model-select");
    const researchCheck = document.getElementById("research-fallback-check");
    const deepCheck = document.getElementById("deep-retry-check");
    const researchModelSelect = document.getElementById("research-model-select");
    const advanced = document.querySelector(".run-advanced");
    if (!modelSelect || !researchCheck || !advanced) return;

    const restoreSelect = (select, value) => {
      if (
        select &&
        typeof value === "string" &&
        [...select.options].some((opt) => opt.value === value)
      ) {
        select.value = value;
      }
    };

    // One-time: a browser that saved the previous shipped default follows the
    // new one. The epoch is stamped on the next save, so a later explicit
    // pick of that same slug is kept.
    const modelEpoch = "gpt-luna-latest";
    const previousDefaults = (modelSelect.dataset.previousDefaults || "")
      .split(",")
      .map((item) => item.trim())
      .filter(Boolean);

    let saved = null;
    try {
      saved = JSON.parse(window.localStorage.getItem(storageKey) || "null");
    } catch (error) {
      saved = null;
    }
    let migratedModel = false;
    if (saved && typeof saved === "object" && saved.modelEpoch !== modelEpoch) {
      if (previousDefaults.includes(saved.model)) {
        delete saved.model;
        migratedModel = true;
      }
    }
    // Same one-time migration for the research model (Sonar Pro -> Sonar).
    const researchEpoch = "sonar";
    const previousResearchDefaults = ((researchModelSelect && researchModelSelect.dataset.previousDefaults) || "")
      .split(",")
      .map((item) => item.trim())
      .filter(Boolean);
    if (saved && typeof saved === "object" && saved.researchEpoch !== researchEpoch) {
      if (previousResearchDefaults.includes(saved.researchModel)) {
        delete saved.researchModel;
        migratedModel = true;
      }
    }
    const deepEpoch = "deep-off";
    if (saved && typeof saved === "object" && saved.deepEpoch !== deepEpoch) {
      if (saved.deep === true) {
        delete saved.deep;
        migratedModel = true;
      }
    }
    if (saved && typeof saved === "object") {
      restoreSelect(modelSelect, saved.model);
      restoreSelect(researchModelSelect, saved.researchModel);
      if (typeof saved.research === "boolean") {
        researchCheck.checked = saved.research;
      }
      if (deepCheck && typeof saved.deep === "boolean") {
        deepCheck.checked = saved.deep;
      }
      // Non-default choices restored: open the panel so they're visible,
      // not silently applied.
      const researchModelChanged =
        researchModelSelect &&
        researchModelSelect.value !== researchModelSelect.options[0].value;
      if (
        modelSelect.value !== modelSelect.options[0].value ||
        researchModelChanged ||
        !researchCheck.checked ||
        (deepCheck && deepCheck.checked)
      ) {
        advanced.setAttribute("open", "");
      }
    }

    // CTV always researches, even when website fallback is disabled. Keep the
    // model enabled so the browser submits the selected value for either path.
    const save = () => {
      try {
        window.localStorage.setItem(
          storageKey,
          JSON.stringify({
            model: modelSelect.value,
            research: researchCheck.checked,
            deep: deepCheck ? deepCheck.checked : undefined,
            researchModel: researchModelSelect ? researchModelSelect.value : undefined,
            modelEpoch,
            researchEpoch,
            deepEpoch,
          })
        );
      } catch (error) {
        /* storage unavailable: settings just don't persist */
      }
    };
    if (migratedModel) save();
    modelSelect.addEventListener("change", save);
    researchCheck.addEventListener("change", save);
    if (deepCheck) deepCheck.addEventListener("change", save);
    if (researchModelSelect) researchModelSelect.addEventListener("change", save);
  })();
});
