// @ts-check
// Central Portal browser app: one clipboard registry, one file registry.

/**
 * @typedef {{ name?: string, size: number, sha256?: string, contentType?: string, updatedAt: string }} Entry
 * @typedef {{ clipboard: Entry | null, file: Entry | null }} Status
 * @typedef {"text" | "image"} ClipboardMode
 * @typedef {{ blob: Blob, type: string }} ClipboardImage
 */

const POLL_MS = 10000;
const TOAST_MS = 2500;
/** @type {string[]} */
const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp", "image/gif"];

/**
 * @param {string} id
 * @returns {HTMLElement}
 */
function byId(id) {
  const node = document.getElementById(id);
  if (!node) throw new Error(`Missing element #${id}`);
  return node;
}

const clipboardBox = /** @type {HTMLTextAreaElement} */ (byId("clipboard"));
const clipboardImage = /** @type {HTMLImageElement} */ (byId("clipboard-image"));
const clipboardMeta = byId("clipboard-meta");
const fileMeta = byId("file-meta");
const fileName = byId("file-name");
const pasteBtn = /** @type {HTMLButtonElement} */ (byId("paste"));
const copyBtn = /** @type {HTMLButtonElement} */ (byId("copy"));
const saveBtn = /** @type {HTMLButtonElement} */ (byId("save"));
const uploadBtn = /** @type {HTMLButtonElement} */ (byId("upload"));
const downloadBtn = /** @type {HTMLButtonElement} */ (byId("download"));
const fileInput = /** @type {HTMLInputElement} */ (byId("file-input"));
const progressWrap = byId("progress-wrap");
const progressBar = /** @type {HTMLProgressElement} */ (byId("upload-progress"));
const progressText = byId("upload-percent");
const connDot = byId("conn-dot");
const connLabel = byId("conn-label");
const toastEl = byId("toast");

/** @type {Status} */
let portalStatus = { clipboard: null, file: null };
/** @type {ClipboardMode} */
let clipboardMode = "text";
/** @type {ClipboardMode} */
let observedMode = "text";
/** @type {string | null} */
let syncedClipboardAt = null;
let clipboardDirty = false;
let clipboardSyncing = false;
let saving = false;
let sharing = false;
let uploading = false;
let refreshing = false;
/** @type {number | undefined} */
let pollTimer;
/** @type {number | undefined} */
let toastTimer;

/**
 * @param {number} bytes
 * @returns {string}
 */
function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes / 1024;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const shown = value >= 10 ? String(Math.round(value)) : value.toFixed(1);
  return `${shown} ${units[unit]}`;
}

/**
 * @param {string} iso
 * @returns {string}
 */
function formatWhen(iso) {
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return "unknown";
  const seconds = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (seconds < 10) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/**
 * @param {string} message
 */
function toast(message) {
  toastEl.textContent = message;
  toastEl.hidden = false;
  toastEl.classList.remove("is-visible");
  void toastEl.offsetWidth;
  toastEl.classList.add("is-visible");
  window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => {
    toastEl.classList.remove("is-visible");
    toastEl.hidden = true;
  }, TOAST_MS);
}

/**
 * @param {boolean} up
 */
function setConnected(up) {
  connDot.classList.toggle("online", up);
  connDot.classList.toggle("offline", !up);
  connLabel.textContent = up ? "connected" : "disconnected";
}

/**
 * @param {Response} response
 * @returns {Promise<string>}
 */
async function errorText(response) {
  let text = "";
  try {
    text = (await response.text()).trim();
  } catch {
    text = "";
  }
  if (!text) return `Request failed (${response.status})`;
  return text.length > 120 ? `${text.slice(0, 117)}…` : text;
}

/**
 * @param {string} text
 * @param {number} code
 * @returns {string}
 */
function uploadError(text, code) {
  const clean = text.trim();
  if (!clean) return `Upload failed (${code})`;
  return clean.length > 120 ? `${clean.slice(0, 117)}…` : clean;
}

/**
 * @param {string | undefined} type
 * @returns {boolean}
 */
function isImageType(type) {
  return !!type && type.startsWith("image/");
}

/**
 * @param {Entry | null} entry
 * @returns {ClipboardMode}
 */
function entryMode(entry) {
  return entry && isImageType(entry.contentType) ? "image" : "text";
}

/**
 * @param {HTMLElement} node
 * @param {Entry | null} entry
 */
function renderMeta(node, entry) {
  node.textContent = entry ? `${formatSize(entry.size)} · ${formatWhen(entry.updatedAt)}` : "empty";
}

function updateClipboardControls() {
  const canCopyImage = clipboardMode === "image" && !!portalStatus.clipboard;
  pasteBtn.disabled = sharing || saving;
  pasteBtn.textContent = sharing ? "Sharing…" : "Paste";
  copyBtn.disabled = sharing || (clipboardMode === "image" && !canCopyImage);
  saveBtn.disabled = sharing || saving || clipboardMode === "image" || !clipboardDirty;
}

/**
 * @param {Entry | null} entry
 */
function renderClipboard(entry) {
  const imageEntry = entry && isImageType(entry.contentType) ? entry : null;
  if (imageEntry && clipboardMode === "image") {
    clipboardMeta.textContent =
      `${imageEntry.contentType} · ${formatSize(imageEntry.size)} · ${formatWhen(imageEntry.updatedAt)}`;
  } else if (imageEntry) {
    clipboardMeta.textContent = "unsaved draft";
  } else {
    renderMeta(clipboardMeta, entry);
  }

  if (clipboardMode === "image" && imageEntry) {
    const src = `/api/clipboard?v=${encodeURIComponent(imageEntry.updatedAt)}`;
    if (clipboardImage.getAttribute("src") !== src) clipboardImage.src = src;
    clipboardImage.hidden = false;
    clipboardBox.hidden = true;
  } else {
    clipboardImage.hidden = true;
    clipboardBox.hidden = false;
  }
  updateClipboardControls();
}

/**
 * @param {Entry | null} entry
 */
function renderFile(entry) {
  renderMeta(fileMeta, entry);
  fileName.textContent = entry ? entry.name || "file" : "";
  fileName.hidden = !entry;
  downloadBtn.disabled = uploading || !entry;
}

/**
 * @param {Entry} entry
 */
async function syncClipboard(entry) {
  if (clipboardMode !== "text" || clipboardSyncing || clipboardDirty || document.activeElement === clipboardBox) {
    return;
  }
  clipboardSyncing = true;
  try {
    const response = await fetch("/api/clipboard", { cache: "no-store" });
    if (response.status === 404) return;
    if (!response.ok) {
      toast(await errorText(response));
      return;
    }
    const text = await response.text();
    if (clipboardMode !== "text" || clipboardDirty || document.activeElement === clipboardBox) return;
    clipboardBox.value = text;
    syncedClipboardAt = entry.updatedAt;
  } catch {
    setConnected(false);
  } finally {
    clipboardSyncing = false;
  }
}

/**
 * Fill the editor with the shared clipboard. Used when the device clipboard is
 * empty or blocked, so pressing Paste never blanks the box.
 */
async function loadRegistryClipboard() {
  try {
    const statusResponse = await fetch("/api/status", { cache: "no-store" });
    if (!statusResponse.ok) {
      toast(await errorText(statusResponse));
      return;
    }
    const next = /** @type {Status} */ (await statusResponse.json());
    portalStatus = next;
    renderFile(next.file);
    const entry = next.clipboard;
    if (!entry) {
      toast("Shared clipboard is empty.");
      return;
    }
    if (isImageType(entry.contentType)) {
      applyStatus(next);
      toast("Loaded shared image");
      return;
    }

    const body = await fetch("/api/clipboard", { cache: "no-store" });
    if (!body.ok) {
      toast(await errorText(body));
      return;
    }
    if (isImageType(body.headers.get("Content-Type") || "")) {
      await refresh();
      return;
    }
    const text = await body.text();
    if (text === "") {
      toast("Shared clipboard is empty.");
      return;
    }
    clipboardMode = "text";
    observedMode = "text";
    clipboardDirty = false;
    clipboardBox.value = text;
    syncedClipboardAt = entry.updatedAt;
    renderClipboard(entry);
    updateClipboardControls();
    setConnected(true);
    toast("Loaded shared clipboard");
  } catch {
    setConnected(false);
    toast("Could not load the shared clipboard.");
  }
}

/**
 * @param {Status} next
 */
function applyStatus(next) {
  const entry = next.clipboard;
  const nextMode = entryMode(entry);
  const switched = nextMode !== observedMode;

  portalStatus = next;
  renderFile(next.file);

  if (switched) {
    observedMode = nextMode;
    clipboardMode = nextMode;
    clipboardDirty = false;
    syncedClipboardAt = null;
    clipboardBox.value = "";
  }

  renderClipboard(entry);

  if (!entry) {
    if (clipboardMode === "text" && !clipboardDirty && document.activeElement !== clipboardBox) {
      clipboardBox.value = "";
    }
    return;
  }
  if (clipboardMode === "text" && !isImageType(entry.contentType) && entry.updatedAt !== syncedClipboardAt) {
    void syncClipboard(entry);
  }
}

async function refresh() {
  if (refreshing) return;
  refreshing = true;
  try {
    const response = await fetch("/api/status", { cache: "no-store" });
    setConnected(true);
    if (!response.ok) {
      toast(await errorText(response));
      return;
    }
    applyStatus(/** @type {Status} */ (await response.json()));
  } catch {
    setConnected(false);
  } finally {
    refreshing = false;
  }
}

async function saveClipboard() {
  const draft = clipboardBox.value;
  if (draft === "") {
    toast("Clipboard is empty.");
    return;
  }
  if (saving || sharing || clipboardMode === "image") return;
  saving = true;
  updateClipboardControls();
  try {
    const response = await fetch("/api/clipboard", {
      method: "PUT",
      headers: { "Content-Type": "text/plain; charset=utf-8" },
      body: draft,
      cache: "no-store",
    });
    setConnected(true);
    if (!response.ok) {
      toast(await errorText(response));
      return;
    }
    const entry = /** @type {Entry | null} */ (await response.json().catch(() => null));
    if (entry) {
      portalStatus.clipboard = entry;
      syncedClipboardAt = entry.updatedAt;
      clipboardMode = entryMode(entry);
      observedMode = clipboardMode;
      if (clipboardBox.value === draft) clipboardDirty = false;
      renderClipboard(entry);
    }
    toast("Clipboard saved");
    void refresh();
  } catch {
    setConnected(false);
    toast("Could not save. Check the connection.");
  } finally {
    saving = false;
    updateClipboardControls();
  }
}

/**
 * @param {string} text
 */
function setDraft(text) {
  if (text === "") return;
  clipboardMode = "text";
  clipboardBox.value = text;
  clipboardDirty = true;
  renderClipboard(portalStatus.clipboard);
  toast("Pasted");
}

/**
 * @param {Blob} blob
 * @param {string} type
 */
async function shareImage(blob, type) {
  if (sharing) return;
  if (!IMAGE_TYPES.includes(type)) {
    toast("Unsupported clipboard image type.");
    return;
  }
  sharing = true;
  updateClipboardControls();
  try {
    const response = await fetch("/api/clipboard", {
      method: "PUT",
      headers: { "Content-Type": type },
      body: blob,
      cache: "no-store",
    });
    setConnected(true);
    if (!response.ok) {
      toast(await errorText(response));
      return;
    }
    const entry = /** @type {Entry | null} */ (await response.json().catch(() => null));
    if (entry) {
      portalStatus.clipboard = entry;
      clipboardMode = entryMode(entry);
      observedMode = clipboardMode;
      syncedClipboardAt = clipboardMode === "text" ? null : entry.updatedAt;
      clipboardDirty = false;
      clipboardBox.value = "";
      renderClipboard(entry);
    }
    toast("Image shared");
    void refresh();
  } catch {
    setConnected(false);
    toast("Could not share the image.");
  } finally {
    sharing = false;
    updateClipboardControls();
  }
}

/**
 * @param {ClipboardItem[]} items
 * @returns {Promise<ClipboardImage | null>}
 */
async function findImage(items) {
  for (const type of IMAGE_TYPES) {
    for (const item of items) {
      if (item.types.includes(type)) {
        const blob = await item.getType(type);
        return { blob, type };
      }
    }
  }
  return null;
}

/**
 * @param {ClipboardItem[]} items
 * @returns {Promise<string | null>}
 */
async function findText(items) {
  for (const item of items) {
    if (item.types.includes("text/plain")) {
      const blob = await item.getType("text/plain");
      return blob.text();
    }
  }
  return null;
}

async function pasteText() {
  const clipboard = navigator.clipboard;
  if (!clipboard || typeof clipboard.readText !== "function") {
    await loadRegistryClipboard();
    return;
  }
  try {
    const text = await clipboard.readText();
    if (text !== "") {
      setDraft(text);
      return;
    }
  } catch {
    // Fall through to the shared clipboard.
  }
  await loadRegistryClipboard();
}

async function pasteClipboard() {
  const clipboard = navigator.clipboard;
  if (!clipboard || typeof clipboard.read !== "function") {
    await pasteText();
    return;
  }
  try {
    const items = await clipboard.read();
    const image = await findImage(items);
    if (image) {
      await shareImage(image.blob, image.type);
      return;
    }
    const text = await findText(items);
    if (text !== null && text !== "") {
      setDraft(text);
      return;
    }
  } catch {
    // Fall back to readText below.
  }
  await pasteText();
}

/**
 * @returns {boolean}
 */
function fallbackCopy() {
  const start = clipboardBox.selectionStart;
  const end = clipboardBox.selectionEnd;
  clipboardBox.focus();
  clipboardBox.select();
  let copied = false;
  try {
    copied = document.execCommand("copy");
  } catch {
    copied = false;
  }
  clipboardBox.setSelectionRange(start, end);
  return copied;
}

/**
 * @param {Blob} blob
 * @returns {Promise<Blob>}
 */
async function toPng(blob) {
  const canvas = document.createElement("canvas");
  const context = canvas.getContext("2d");
  if (!context) throw new Error("Canvas is unavailable");

  /** @type {ImageBitmap | null} */
  let bitmap = null;
  if (typeof createImageBitmap === "function") {
    try {
      bitmap = await createImageBitmap(blob);
    } catch {
      bitmap = null;
    }
  }

  if (bitmap) {
    canvas.width = bitmap.width;
    canvas.height = bitmap.height;
    context.drawImage(bitmap, 0, 0);
    bitmap.close();
  } else {
    // A data URL keeps the conversion inside the CSP's img-src 'self' data:.
    /** @type {Promise<string>} */
    const dataUrl = new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : "");
      reader.onerror = () => reject(new Error("Could not read the image"));
      reader.readAsDataURL(blob);
    });
    const image = new Image();
    image.src = await dataUrl;
    await image.decode();
    canvas.width = image.naturalWidth;
    canvas.height = image.naturalHeight;
    context.drawImage(image, 0, 0);
  }

  /** @type {Promise<Blob>} */
  const png = new Promise((resolve, reject) => {
    canvas.toBlob((result) => (result ? resolve(result) : reject(new Error("PNG conversion failed"))), "image/png");
  });
  return png;
}

async function copyImage() {
  const clipboard = navigator.clipboard;
  if (!clipboard || typeof clipboard.write !== "function" || typeof ClipboardItem === "undefined") {
    toast("Image copy needs HTTPS and browser support.");
    return;
  }
  try {
    const response = await fetch("/api/clipboard", { cache: "no-store" });
    setConnected(true);
    if (!response.ok) {
      toast(await errorText(response));
      return;
    }
    const source = await response.blob();
    const png = source.type === "image/png" ? source : await toPng(source);
    await clipboard.write([new ClipboardItem({ "image/png": png })]);
    toast("Copied");
  } catch {
    toast("Could not copy the image.");
  }
}

async function copyClipboard() {
  if (clipboardMode === "image") {
    await copyImage();
    return;
  }
  const text = clipboardBox.value;
  if (text === "") {
    toast("Nothing to copy.");
    return;
  }
  const clipboard = navigator.clipboard;
  if (clipboard && typeof clipboard.writeText === "function") {
    try {
      await clipboard.writeText(text);
      toast("Copied");
      return;
    } catch {
      // Fall through to the legacy copy path.
    }
  }
  toast(fallbackCopy() ? "Copied" : "Could not copy. Select and copy manually.");
}

/**
 * @param {boolean} active
 */
function setUploading(active) {
  uploading = active;
  uploadBtn.disabled = active;
  fileInput.disabled = active;
  downloadBtn.disabled = active || !portalStatus.file;
  progressWrap.hidden = !active;
  if (!active) {
    progressBar.value = 0;
    progressText.textContent = "0%";
  }
}

/**
 * @param {File} file
 */
function upload(file) {
  if (uploading) return;
  setUploading(true);
  progressBar.value = 0;
  progressText.textContent = "0%";

  const xhr = new XMLHttpRequest();
  xhr.open("PUT", `/api/file?name=${encodeURIComponent(file.name)}`);
  xhr.upload.onprogress = (event) => {
    if (!event.lengthComputable || event.total === 0) return;
    const percent = Math.round((event.loaded / event.total) * 100);
    progressBar.value = percent;
    progressText.textContent = `${percent}%`;
  };
  xhr.onload = () => {
    setConnected(true);
    if (xhr.status >= 200 && xhr.status < 300) {
      try {
        const entry = /** @type {Entry} */ (JSON.parse(xhr.responseText));
        portalStatus.file = entry;
        renderFile(entry);
      } catch {
        // Fall back to the status refresh below.
      }
      toast("File uploaded");
      void refresh();
    } else {
      toast(uploadError(xhr.responseText, xhr.status));
    }
    setUploading(false);
  };
  xhr.onerror = () => {
    setConnected(false);
    toast("Upload failed. Check the connection.");
    setUploading(false);
  };
  xhr.onabort = () => {
    toast("Upload cancelled.");
    setUploading(false);
  };
  xhr.send(file);
}

function downloadFile() {
  const entry = portalStatus.file;
  if (!entry) return;
  const link = document.createElement("a");
  link.href = "/api/file";
  link.download = entry.name || "";
  link.rel = "noopener";
  document.body.append(link);
  link.click();
  link.remove();
}

function startPolling() {
  if (pollTimer !== undefined) return;
  pollTimer = window.setInterval(() => {
    if (document.visibilityState === "visible") void refresh();
  }, POLL_MS);
}

function stopPolling() {
  if (pollTimer === undefined) return;
  window.clearInterval(pollTimer);
  pollTimer = undefined;
}

clipboardBox.addEventListener("input", () => {
  clipboardDirty = true;
  updateClipboardControls();
});
clipboardBox.addEventListener("blur", () => {
  if (!clipboardDirty) void refresh();
});
pasteBtn.addEventListener("click", () => void pasteClipboard());
copyBtn.addEventListener("click", () => void copyClipboard());
saveBtn.addEventListener("click", () => void saveClipboard());
uploadBtn.addEventListener("click", () => {
  if (uploading) return;
  fileInput.value = "";
  fileInput.click();
});
downloadBtn.addEventListener("click", downloadFile);
fileInput.addEventListener("change", () => {
  const file = fileInput.files && fileInput.files[0];
  fileInput.value = "";
  if (file) upload(file);
});

document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") {
    void refresh();
    startPolling();
  } else {
    stopPolling();
  }
});
window.addEventListener("online", () => void refresh());

renderClipboard(null);
renderFile(null);
void refresh();
if (document.visibilityState === "visible") startPolling();

const secure = location.protocol === "https:" ||
  ["localhost", "127.0.0.1", "[::1]"].includes(location.hostname);
if (secure && "serviceWorker" in navigator) {
  window.addEventListener("load", () => {
    navigator.serviceWorker.register("/sw.js").catch(() => {});
  });
}
