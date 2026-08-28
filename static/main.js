document.addEventListener("DOMContentLoaded", () => {
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
  const toastRoot = document.getElementById("toast-root");
  const flashData = document.getElementById("flash-data");

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
      node.textContent = new Intl.DateTimeFormat("en-US", {
        month: "short",
        day: "numeric",
        hour: "numeric",
        minute: "2-digit",
        hour12: true,
        ...(includeYear ? { year: "numeric" } : {}),
      }).format(date);
      node.title = `${new Intl.DateTimeFormat("en-US", {
        month: "short",
        day: "numeric",
        year: "numeric",
        hour: "numeric",
        minute: "2-digit",
        hour12: true,
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
    toast.innerHTML = `<span>${escapeHtml(text)}</span><button type="button" class="toast-close" aria-label="Dismiss notification">×</button>`;
    toastRoot.appendChild(toast);

    const remove = () => {
      toast.classList.add("toast-leave");
      setTimeout(() => toast.remove(), 180);
    };
    toast.querySelector(".toast-close").addEventListener("click", remove);
    setTimeout(remove, 5200);
  }

  function openConfirmModal({ title, message, confirmLabel = "Confirm", submitCallback }) {
    if (!confirmModal || !confirmModalText) {
      submitCallback();
      return;
    }
    if (confirmModalTitle) {
      confirmModalTitle.textContent = title;
    }
    if (confirmModalAction) {
      confirmModalAction.textContent = confirmLabel;
    }
    confirmModalText.textContent = message;
    confirmModal.showModal();
    confirmModal.addEventListener(
      "close",
      () => {
        if (confirmModal.returnValue === "confirm") {
          submitCallback();
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
      dropzoneStatus.textContent = `Uploading ${names}…`;
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
      queueJobBtn.textContent = "Starting…";
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
          .map((t) => `<span class="comp-seg ${t.seg}" style="width: ${t.pct}%;"></span>`)
          .join("");
        compBarEl.hidden = types.length === 0;
      }
      if (rowsEl) {
        rowsEl.innerHTML = `<strong>${data ? data.total : 0}</strong> rows`;
        rowsEl.hidden = types.length === 0;
      }
      if (fileTypesEl) {
        fileTypesEl.innerHTML = types
          .map(
            (t) =>
              `<span class="tchip"><span class="tdot ${t.seg}"></span>${t.label} <b>${t.count}</b></span>`
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
        title: "Delete input file?",
        message: `Delete ${name}? This cannot be undone.`,
        confirmLabel: "Delete",
        submitCallback: () => {
          deleteInputFilename.value = name;
          deleteInputForm.submit();
        },
      });
    });
  });

  document.querySelectorAll(".js-confirm-cancel").forEach((form) => {
    confirmFormSubmit(form, {
      title: "Cancel run?",
      message: `Stop ${form.dataset.jobId || "this run"}? Progress so far is kept.`,
      confirmLabel: "Cancel run",
    });
  });

  document.querySelectorAll(".js-confirm-job-delete").forEach((form) => {
    confirmFormSubmit(form, {
      title: "Delete run?",
      message: `Delete ${form.dataset.jobId || "this run"} and its results? This cannot be undone.`,
      confirmLabel: "Delete",
    });
  });

  // ── Live progress: table rows + monitor modal ──

  function setBadge(node, statusText) {
    if (!node) {
      return;
    }
    const safe = (statusText || "idle").toLowerCase();
    node.className = `status-badge status-${safe}`;
    node.innerHTML = `<span class="status-dot"></span>${safe}`;
  }

  function formatElapsed(seconds) {
    const safe = Math.max(0, Math.floor(seconds));
    if (safe < 60) {
      return `${safe}s`;
    }
    if (safe < 3600) {
      return `${Math.floor(safe / 60)}m ${safe % 60}s`;
    }
    return `${Math.floor(safe / 3600)}h ${Math.floor((safe % 3600) / 60)}m`;
  }

  // Fallback per-item retry cost (single-session headless Chrome). The
  // server sends the live figure as progress.retry_seconds_per_item — a few
  // seconds when the local Firecrawl retry pass is available, ~15s when only
  // headless Chrome remains; keep in sync with web_service.py.
  const DEEP_RETRY_SECONDS_PER_ITEM = 15;

  function estimateEta(progress, startedAt, status) {
    const normalizedStatus = String(status || "").toLowerCase();
    if (["completed", "failed", "cancelled", "archived"].includes(normalizedStatus)) {
      return "Done";
    }
    if (normalizedStatus === "queued") {
      return "Queued";
    }

    const total = Number(progress.total_domains || 0);
    const processed = Number(progress.processed || 0);
    const retrying = Number(progress.retrying || 0);
    const attempts = processed + retrying;

    if (!startedAt || total <= 0 || attempts <= 0) {
      return "Estimating…";
    }

    const started = new Date(startedAt);
    if (Number.isNaN(started.getTime())) {
      return "Estimating…";
    }

    const elapsed = (Date.now() - started.getTime()) / 1000;
    if (elapsed <= 5) {
      return "Estimating…";
    }

    // Two-rate model: unattempted items at the measured fast-crawl rate,
    // plus a fixed retry-pass cost for each item queued for retry.
    const retrySeconds =
      Number(progress.retry_seconds_per_item) || DEEP_RETRY_SECONDS_PER_ITEM;
    const fastRate = elapsed / attempts;
    const remainingFresh = Math.max(0, total - attempts);
    const remaining = remainingFresh * fastRate + retrying * retrySeconds;

    if (remaining < 60) {
      return "< 1m left";
    }
    if (remaining < 3600) {
      return `~${Math.round(remaining / 60)}m left`;
    }
    return `~${Math.floor(remaining / 3600)}h ${Math.round((remaining % 3600) / 60)}m left`;
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
          ? `${processed} / ${total} · ${retrying} to retry`
          : `${processed} / ${total}`;
      } else if (processed > 0) {
        text.textContent = `${processed} processed`;
      } else if (job.status === "queued") {
        text.textContent = "Waiting";
      } else {
        text.textContent = "Starting…";
      }
    }

    const eta = jobRow.querySelector(".queue-progress-eta");
    if (eta) {
      eta.textContent = estimateEta(progress, job.started_at, job.status);
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
      liveProgressSummary.textContent = "No progress data yet.";
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
        ? `<span class="progress-retry">${retrying} to retry</span>`
        : "";
      liveProgressSummary.innerHTML =
        `<span class="progress-counter">${processed} / ${total}</span>` +
        `<span class="progress-detail">` +
        `<span class="progress-success">${successful} ok</span>` +
        `<span class="progress-fail">${errors} failed</span>` +
        retryDetail +
        `</span>`;
    } else if (processed > 0) {
      liveProgressSummary.textContent = `${processed} processed`;
    } else {
      liveProgressSummary.textContent = "Starting…";
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
      monitorJobInput.textContent = "Input: loading…";
    }
    if (monitorJobTiming) {
      monitorJobTiming.textContent = "Elapsed — · ETA —";
    }
    setBadge(monitorJobStatus, "queued");
    updateLiveProgress({}, false);
    if (liveLog) {
      liveLog.textContent = "Loading log output…";
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
        monitorJobTiming.textContent = "Live view unavailable";
      }
      updateLiveProgress({}, false);
      return;
    }

    const payload = await response.json();
    if (!payload.job) {
      if (monitorJobTiming) {
        monitorJobTiming.textContent = payload.message || "No active data";
      }
      setBadge(monitorJobStatus, "idle");
      updateLiveProgress({}, false);
      if (liveLog) {
        liveLog.textContent = "No log output yet.";
        liveLog.classList.remove("is-active");
      }
      return;
    }

    if (monitorJobId) {
      monitorJobId.textContent = String(payload.job.name || "").trim() || String(payload.job.id);
    }
    if (monitorJobInput) {
      monitorJobInput.textContent = `Input: ${payload.job.input_file || "unknown"}`;
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
      monitorJobTiming.textContent = `Elapsed ${elapsed} · ETA ${eta}`;
    }

    updateQueueRowProgress(payload.job, payload.progress || {});
    setBadge(monitorJobStatus, payload.job.status);
    updateLiveProgress(payload.progress || {}, true);

    const lines = payload.log_lines || [];
    if (liveLog) {
      liveLog.textContent = lines.length ? lines.join("\n") : "No log output yet.";
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
          `<div class="completion-stat"><span class="completion-label">Total</span><span class="completion-value">${t}</span></div>` +
          `<div class="completion-stat"><span class="completion-label">Succeeded</span><span class="completion-value completion-success">${s}</span></div>` +
          `<div class="completion-stat"><span class="completion-label">Failed</span><span class="completion-value completion-fail">${e}</span></div>` +
          `<div class="completion-stat"><span class="completion-label">Duration</span><span class="completion-value">${durationText}</span></div>` +
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
    const researchModelSelect = document.getElementById("research-model-select");
    const researchModelField = document.getElementById("research-model-field");
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

    let saved = null;
    try {
      saved = JSON.parse(window.localStorage.getItem(storageKey) || "null");
    } catch (error) {
      saved = null;
    }
    if (saved && typeof saved === "object") {
      restoreSelect(modelSelect, saved.model);
      restoreSelect(researchModelSelect, saved.researchModel);
      if (typeof saved.research === "boolean") {
        researchCheck.checked = saved.research;
      }
      // Non-default choices restored: open the panel so they're visible,
      // not silently applied.
      const researchModelChanged =
        researchModelSelect &&
        researchModelSelect.value !== researchModelSelect.options[0].value;
      if (
        modelSelect.value !== modelSelect.options[0].value ||
        researchModelChanged ||
        !researchCheck.checked
      ) {
        advanced.setAttribute("open", "");
      }
    }

    // The research-model picker only matters while the fallback is on.
    const syncResearchModelField = () => {
      if (!researchModelField) return;
      researchModelField.style.opacity = researchCheck.checked ? "" : "0.5";
      if (researchModelSelect) researchModelSelect.disabled = !researchCheck.checked;
    };
    syncResearchModelField();

    const save = () => {
      syncResearchModelField();
      try {
        window.localStorage.setItem(
          storageKey,
          JSON.stringify({
            model: modelSelect.value,
            research: researchCheck.checked,
            researchModel: researchModelSelect ? researchModelSelect.value : undefined,
          })
        );
      } catch (error) {
        /* storage unavailable: settings just don't persist */
      }
    };
    modelSelect.addEventListener("change", save);
    researchCheck.addEventListener("change", save);
    if (researchModelSelect) researchModelSelect.addEventListener("change", save);
  })();
});
