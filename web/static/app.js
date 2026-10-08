"use strict";

// ---- API helpers ----

const H = { "X-Private-AI": "1" };

async function api(method, path, body) {
  const opts = { method, headers: { ...H } };
  if (body instanceof FormData) {
    opts.body = body;
  } else if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 423) { showLock(); throw new Error("Locked"); }
  if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
  return res.json();
}

const $ = (id) => document.getElementById(id);
const el = (tag, attrs = {}, ...kids) => {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  for (const k of kids) if (k != null) e.append(k);
  return e;
};

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1 << 20) return (n / 1024).toFixed(0) + " KB";
  return (n / (1 << 20)).toFixed(1) + " MB";
}
const fmtDate = (s) => new Date(s).toLocaleString();

// Minimal, safe Markdown: input is escaped first, then a few constructs are
// re-introduced. Model output can never inject HTML.
function escapeHTML(s) {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}
// Some models (gpt-oss) write math as LaTeX; show it as plain math.
const LATEX_SYMBOLS = { times: "×", cdot: "·", div: "÷", approx: "≈", le: "≤", leq: "≤", ge: "≥", geq: "≥", neq: "≠", pm: "±",
  to: "→", rightarrow: "→", Rightarrow: "⇒", infty: "∞", "%": "%", "$": "$", ",": " ", ";": " ", ":": " ", "!": "", quad: "  ", qquad: "   " };
function latexLite(t) {
  return t
    .replace(/\\\[\s*([\s\S]*?)\s*\\\]/g, (_, m) => "\n" + m.trim() + "\n")
    .replace(/\\\(\s*([\s\S]*?)\s*\\\)/g, "$1")
    .replace(/\\(?:text|mathrm|textbf|mathbf|operatorname|boxed)\{([^{}]*)\}/g, "$1")
    .replace(/\\frac\{([^{}]*)\}\{([^{}]*)\}/g, "($1)/($2)")
    .replace(/\\sqrt\{([^{}]*)\}/g, "√($1)")
    .replace(/\\(left|right)(?![a-zA-Z])/g, "")
    .replace(/\\([a-zA-Z]+|[%$,;:!])/g, (m, c) => LATEX_SYMBOLS[c] ?? m)
    .replace(/\^\{([^{}]*)\}/g, "^$1");
}

function markdown(src) {
  const blocks = [];
  let s = escapeHTML(src).replace(/```[\w-]*\n?([\s\S]*?)(```|$)/g, (_, code) => {
    blocks.push("<pre><code>" + code.replace(/\n$/, "") + "</code></pre>");
    return "\u0000" + (blocks.length - 1) + "\u0000";
  });
  s = latexLite(s);
  const inline = (t) => t
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/(^|[^*])\*([^*\s][^*]*)\*/g, "$1<em>$2</em>");
  const out = [];
  for (const para of s.split(/\n{2,}/)) {
    const m = para.match(/^\u0000(\d+)\u0000$/);
    if (m) { out.push(blocks[+m[1]]); continue; }
    const lines = para.split("\n");
    if (lines.every((l) => /^\s*([-*•]|\d+[.)])\s+/.test(l))) {
      const ordered = /^\s*\d/.test(lines[0]);
      const items = lines.map((l) => "<li>" + inline(l.replace(/^\s*([-*•]|\d+[.)])\s+/, "")) + "</li>").join("");
      out.push(ordered ? "<ol>" + items + "</ol>" : "<ul>" + items + "</ul>");
    } else if (/^#{1,4}\s/.test(para)) {
      out.push("<p><strong>" + inline(para.replace(/^#+\s*/, "")) + "</strong></p>");
    } else {
      out.push("<p>" + inline(para).replace(/\n/g, "<br>") + "</p>");
    }
  }
  return out.join("").replace(/\u0000(\d+)\u0000/g, (_, i) => blocks[+i]);
}

// ---- State ----

const state = {
  status: null,
  chatId: null,
  focus: new Map(), // doc id -> name
  busy: false,
  view: "chat",
  agentId: "",     // agent for new chats ("" = plain Private AI)
  agents: [],
  catalog: null,   // tools + templates
  images: [],      // pending attachments as data: URLs
};

const MAX_IMAGES = 4;
const MAX_IMAGE_SIDE = 1280; // keeps image tokens (and CPU time) reasonable

// ---- Status / engine pill ----

async function refreshStatus() {
  try {
    const st = await (await fetch("/api/status", { headers: H })).json();
    state.status = st;
    const pill = $("engine");
    pill.className = "pill";
    if (st.chat_state === "ready") {
      pill.textContent = "● " + st.chat_model;
      pill.classList.add("ready");
    } else if (st.chat_state === "starting") {
      const secs = st.chat_loading_secs || 0;
      pill.textContent = `Loading ${st.chat_model || "model"}… ${Math.floor(secs / 60)}:${String(secs % 60).padStart(2, "0")}`;
      pill.classList.add("starting");
    } else {
      pill.textContent = st.chat_state === "no_model" ? "No model on drive" : "Model failed to start";
      pill.classList.add("error");
    }
    pill.title = st.chat_error || "";
    updateComposer();
    renderMic();
    renderLoadTip(st);
    const warn = st.chat_state === "ready" && st.chat_warning && st.chat_warning !== state.dismissedWarn;
    $("engine-warn").classList.toggle("hidden", !warn);
    if (warn) $("engine-warn-text").textContent = st.chat_warning;
    const failed = st.chat_state === "error" || st.chat_state === "no_model";
    $("engine-error").classList.toggle("hidden", !failed);
    if (failed) {
      $("engine-error-title").textContent = st.chat_state === "no_model"
        ? "No AI model was found on this drive." : "The AI model failed to start.";
      $("engine-error-text").textContent = st.chat_error || "";
    }
    if (st.locked && $("lock").classList.contains("hidden") && $("bye").classList.contains("hidden")) showLock();
    if (state.view === "settings") renderSettings();
    return st;
  } catch {
    $("engine").textContent = "Disconnected";
    $("engine").className = "pill error";
  }
}

// The composer waits for the model: loading a few GB from a USB drive can
// take a minute or two on first start.
const modelReady = () => state.status && state.status.chat_state === "ready";
function updateComposer() {
  renderVisionNote();
  const ready = modelReady();
  $("send").disabled = state.busy || !ready;
  $("input").placeholder = ready ? "Ask anything…"
    : state.status && state.status.chat_state === "starting"
      ? "Loading the AI model from the drive… this can take a minute or two the first time"
      : "The AI model is not running — see the message above";
}

// After a slow load from the drive, suggest the model cache.
function renderLoadTip(st) {
  const slow = (st.chat_state === "starting" && st.chat_loading_secs > 90) ||
    (st.chat_state === "ready" && st.chat_load_secs > 90);
  const show = slow && !st.cache_on && !state.tipDismissed;
  $("load-tip").classList.toggle("hidden", !show);
}

// ---- Lock screen ----

function showLock() {
  const init = state.status && state.status.initialized;
  document.querySelectorAll(".view").forEach((v) => v.classList.add("hidden"));
  $("tabs").classList.add("hidden");
  $("lock").classList.remove("hidden");
  $("lock-title").textContent = init ? "Unlock" : "Create your private vault";
  $("lock-help").textContent = init
    ? "Enter your passphrase to decrypt your chats, files and memory."
    : "Choose a passphrase (8+ characters). It encrypts everything you store on this drive. There is no recovery if you forget it.";
  $("pass2").classList.toggle("hidden", init);
  $("pass2").required = !init;
  $("pass").autocomplete = init ? "current-password" : "new-password";
  $("lock-btn").textContent = init ? "Unlock" : "Create vault";
  $("lock-error").textContent = "";
  $("pass").value = $("pass2").value = "";
  $("pass").focus();
  typeWake();
}

$("lock-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const create = !state.status.initialized;
  if (create && $("pass").value !== $("pass2").value) {
    $("lock-error").textContent = "Passphrases do not match.";
    return;
  }
  $("lock-btn").disabled = true;
  $("lock-error").textContent = create ? "Creating vault…" : "Unlocking…";
  try {
    await api("POST", "/api/unlock", { passphrase: $("pass").value, create });
    $("pass").value = $("pass2").value = "";
    $("lock").classList.add("hidden");
    $("tabs").classList.remove("hidden");
    await refreshStatus();
    await loadAgents();
    newChat();
    show("chat");
  } catch (err) {
    $("lock-error").textContent = err.message;
  } finally {
    $("lock-btn").disabled = false;
  }
});

// ---- Navigation ----

function show(view) {
  state.view = view;
  document.querySelectorAll(".view").forEach((v) => v.classList.add("hidden"));
  $("view-" + view).classList.remove("hidden");
  document.querySelectorAll("#tabs button").forEach((b) => b.classList.toggle("active", b.dataset.view === view));
  if (view === "agents") renderAgents();
  if (view === "files") renderDocs();
  if (view === "memory") renderMemory();
  if (view === "chats") renderChats();
  if (view === "settings") renderSettings();
  if (view === "chat") $("input").focus();
}
document.querySelectorAll("#tabs button").forEach((b) => b.addEventListener("click", () => show(b.dataset.view)));

// ---- Chat ----

function newChat() {
  state.chatId = null;
  state.focus.clear();
  renderFocus();
  $("messages").replaceChildren(emptyState());
  $("agent-select").disabled = false;
}
function emptyState() {
  const ag = currentAgent();
  if (ag) {
    return el("div", { class: "empty", id: "chat-empty" },
      el("div", { class: "agent-emoji", style: "font-size:40px;width:auto" }, ag.emoji),
      el("h2", {}, ag.name),
      el("p", { class: "muted" }, ag.description || "Ask me anything."),
      ag.tools && ag.tools.length ? el("p", { class: "muted" }, "Tools: " + ag.tools.map(toolLabel).join(", ")) : null);
  }
  return el("div", { class: "empty", id: "chat-empty" },
    el("h2", {}, "What can I help you with?"),
    el("p", { class: "muted" }, "Paste a screenshot (⌘V / Ctrl+V), or drop a PDF, Word or text file anywhere to ask about it. Everything stays on this drive."));
}
const currentAgent = () => state.agents.find((a) => a.id === state.agentId);
const extraLabels = { read_image: "Image reader" };
const toolLabel = (id) => extraLabels[id] || (state.catalog && (state.catalog.tools.find((t) => t.id === id) || {}).label) || id;
$("new-chat").addEventListener("click", () => { newChat(); $("input").focus(); });

function renderFocus() {
  const box = $("focus");
  box.replaceChildren();
  for (const [id, name] of state.focus) {
    box.append(el("span", { class: "chip", title: "The assistant will read this file" }, "📄 " + name,
      el("button", { type: "button", "aria-label": "Remove", onclick: () => { state.focus.delete(id); renderFocus(); } }, "×")));
  }
}

function sourcesEl(sources) {
  if (!sources || !sources.length) return null;
  const list = el("ul");
  for (const s of sources) {
    list.append(el("li", {}, el("strong", {}, s.doc_name + (s.seq >= 0 ? ` (part ${s.seq + 1})` : " (full text)")), ": " + s.excerpt));
  }
  return el("details", { class: "sources" }, el("summary", {}, `Sources (${sources.length})`), list);
}

function imagesEl(srcs) {
  if (!srcs || !srcs.length) return null;
  return el("div", { class: "msg-images" }, ...srcs.map((src) =>
    el("img", { src, alt: "Attached image", onclick: () => window.open(src, "_blank", "noopener") })));
}

function stepsEl(steps) {
  const list = el("ol");
  const box = el("details", { class: "steps" }, el("summary", {}, "🔧 Tools used"), list);
  box._list = list;
  for (const st of steps || []) addStep(box, st, true);
  return box;
}
function addStep(box, st, done) {
  let args = st.args || "";
  try { const o = JSON.parse(args); args = Object.values(o).join(", "); } catch {}
  const li = el("li", {}, el("strong", {}, toolLabel(st.tool)), args ? ": " : "", args ? el("code", {}, args) : null);
  if (done && st.result) li.append(" → " + st.result);
  box._list.append(li);
  box.querySelector("summary").textContent = `🔧 Tools used (${box._list.children.length})`;
  return li;
}

function addMessage(role, content, sources, images, steps, agentName) {
  const empty = $("chat-empty");
  if (empty) empty.classList.add("hidden");
  const body = el("div", { class: "body" });
  if (role === "assistant") body.innerHTML = markdown(content); else body.textContent = content;
  const msg = el("div", { class: "msg " + role });
  if (role === "assistant" && agentName) msg.append(el("div", { class: "agent-label" }, agentName));
  const imgs = imagesEl(images);
  if (imgs) msg.append(imgs);
  if (role === "assistant") {
    msg._steps = stepsEl(steps);
    if (!steps || !steps.length) msg._steps.classList.add("hidden");
    msg.append(msg._steps);
  }
  msg.append(body);
  if (role === "assistant") {
    const src = sourcesEl(sources);
    if (src) msg.append(src);
    msg.append(el("div", { class: "msg-tools" },
      el("button", { type: "button", onclick: () => navigator.clipboard && navigator.clipboard.writeText(msg._raw || content) }, "Copy"),
      el("button", { type: "button", title: "Read aloud", onclick: () => readAloud(msg._raw || content, agentName) }, "🔊")));
  }
  msg._raw = content;
  $("messages").append(msg);
  $("messages").scrollTop = $("messages").scrollHeight;
  return msg;
}

// opts: fromVoice (said with the mic), speaker (a live call's), signal
// (aborts the turn), onText (the reply so far).
async function send(text, opts = {}) {
  const images = state.images.slice();
  if (state.busy || (!text.trim() && !images.length)) return;
  state.busy = true;
  $("send").disabled = true;
  state.images = [];
  renderAttachments();
  const ag = currentAgent();
  addMessage("user", text, null, images);
  const msg = addMessage("assistant", "", null, null, null, ag ? `${ag.emoji} ${ag.name}` : "");
  $("agent-select").disabled = true; // a chat keeps its agent
  const body = msg.querySelector(".body");
  body.classList.add("typing");
  let raw = "";
  const speaker = opts.speaker !== undefined ? opts.speaker : voicePrefs.speak ? makeSpeaker(agentVoice(ag)) : null;
  try {
    const res = await fetch("/api/chat", {
      method: "POST",
      signal: opts.signal,
      headers: { ...H, "Content-Type": "application/json" },
      body: JSON.stringify({
        chat_id: state.chatId,
        agent_id: state.chatId ? "" : state.agentId,
        message: text,
        images,
        use_docs: $("use-docs").checked,
        doc_ids: [...state.focus.keys()],
      }),
    });
    if (res.status === 423) { showLock(); return; }
    if (!res.ok) throw new Error(await res.text());
    const reader = res.body.getReader();
    const dec = new TextDecoder();
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, i);
        buf = buf.slice(i + 2);
        const ev = (block.match(/^event: (.*)$/m) || [])[1];
        const data = JSON.parse((block.match(/^data: (.*)$/m) || [, "{}"])[1]);
        if (ev === "meta") {
          state.chatId = data.chat_id;
          const src = sourcesEl(data.sources);
          if (src) msg.insertBefore(src, msg.querySelector(".msg-tools"));
        } else if (ev === "tool") {
          msg._steps.classList.remove("hidden");
          msg._pending = addStep(msg._steps, data, false);
          if (!raw) body.textContent = `Using ${toolLabel(data.tool)}…`;
        } else if (ev === "tool_result") {
          if (msg._pending) msg._pending.append(" → " + data.result);
          if (!raw) body.textContent = "Thinking…";
        } else if (ev === "thinking") {
          if (!raw) body.textContent = "Thinking…";
        } else if (ev === "token") {
          raw += data.text;
          body.innerHTML = markdown(raw.replace(/<think>[\s\S]*?(<\/think>|$)/g, "").replace(/<tool_call>[\s\S]*?(<\/tool_call>|$)/g, ""));
          msg._raw = raw;
          if (speaker) speaker.feed(raw);
          if (opts.onText) opts.onText(raw);
          const m = $("messages");
          if (m.scrollHeight - m.scrollTop - m.clientHeight < 120) m.scrollTop = m.scrollHeight;
        } else if (ev === "error") {
          throw new Error(data.error);
        }
      }
    }
  } catch (err) {
    if (err.name === "AbortError") body.append(el("p", { class: "muted small" }, "(interrupted)"));
    else body.append(el("p", { class: "err" }, "⚠ " + err.message));
  } finally {
    body.classList.remove("typing");
    state.busy = false;
    updateComposer();
    // Hands-free: after the reply has been spoken, listen for the next turn.
    if (speaker && !opts.speaker) speaker.end(raw, () => { if (opts.fromVoice && voicePrefs.handsfree && raw) listen(true); });
    if (opts.onDone) opts.onDone(raw);
  }
}

$("chat-form").addEventListener("submit", (e) => {
  e.preventDefault();
  if (!modelReady() || state.busy) return; // keep the typed text until the model is ready
  if (state.images.length && !(state.status && state.status.images_ok)) return; // see the vision note
  const text = $("input").value;
  if (!text.trim() && !state.images.length) return;
  $("input").value = "";
  autosize();
  send(text);
});
$("input").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
    e.preventDefault();
    $("chat-form").requestSubmit();
  }
});
function autosize() {
  const t = $("input");
  t.style.height = "auto";
  t.style.height = Math.min(t.scrollHeight, 200) + "px";
}
$("input").addEventListener("input", autosize);

async function openChat(id) {
  const chat = await api("GET", "/api/chats/" + id);
  state.agentId = chat.agent_id || "";
  $("agent-select").value = state.agentId;
  newChat();
  state.chatId = chat.id;
  const ag = currentAgent();
  const label = chat.agent_id ? (ag ? `${ag.emoji} ${ag.name}` : "Deleted agent") : "";
  for (const m of chat.messages || []) {
    addMessage(m.role, m.content, m.sources, (m.images || []).map((i) => "/api/images/" + i), m.steps, label);
  }
  $("agent-select").disabled = true;
  show("chat");
}

// ---- Images ----

// Downscale in the browser: a full-resolution screenshot costs the model
// several thousand tokens; 1280px keeps text legible at a fraction.
async function toDataURL(file) {
  const bmp = await createImageBitmap(file);
  const scale = Math.min(1, MAX_IMAGE_SIDE / Math.max(bmp.width, bmp.height));
  const c = document.createElement("canvas");
  c.width = Math.round(bmp.width * scale);
  c.height = Math.round(bmp.height * scale);
  const ctx = c.getContext("2d");
  ctx.fillStyle = "#fff"; // flatten transparency for JPEG
  ctx.fillRect(0, 0, c.width, c.height);
  ctx.drawImage(bmp, 0, 0, c.width, c.height);
  bmp.close && bmp.close();
  return c.toDataURL("image/jpeg", 0.9);
}

async function attachImages(files) {
  for (const f of files) {
    if (state.images.length >= MAX_IMAGES) {
      addMessage("assistant", `⚠ You can attach up to ${MAX_IMAGES} images per message.`);
      break;
    }
    try {
      state.images.push(await toDataURL(f));
    } catch {
      addMessage("assistant", `⚠ Could not read ${f.name || "that image"}.`);
    }
  }
  renderAttachments();
  $("input").focus();
}

function renderAttachments() {
  $("attachments").replaceChildren(...state.images.map((src, i) =>
    el("div", { class: "thumb" }, el("img", { src, alt: "Attachment" }),
      el("button", { type: "button", "aria-label": "Remove image", onclick: () => { state.images.splice(i, 1); renderAttachments(); } }, "×"))));
  renderVisionNote();
}

function renderVisionNote() {
  const note = $("vision-note");
  const st = state.status;
  if (!state.images.length || !st || st.images_ok) { note.classList.add("hidden"); return; }
  note.classList.remove("hidden");
  if (st.vision_state === "starting") {
    note.replaceChildren(el("span", {}, `The image reader (${st.vision_model}) is still loading — send in a moment.`));
    return;
  }
  const vision = (st.models || []).find((m) => m.present && m.vision);
  if (vision) {
    note.replaceChildren(el("span", {}, `${st.chat_model || "This model"} can't see images. ${vision.name} can.`),
      el("button", { type: "button", class: "primary", onclick: async () => {
        await api("POST", "/api/models/select", { id: vision.id });
        refreshStatus();
      } }, "Switch model"));
  } else {
    note.replaceChildren(el("span", {}, "None of the models on this drive can see images. Add a vision model such as Qwen3 VL (see README)."));
  }
}

// Split dropped/pasted/attached files: images go to the message, others to Files.
function takeFiles(files) {
  const imgs = files.filter((f) => f.type.startsWith("image/"));
  const docs = files.filter((f) => !f.type.startsWith("image/"));
  if (imgs.length) attachImages(imgs);
  if (docs.length) {
    upload(docs, true);
    addMessage("assistant", `Adding ${docs.map((f) => f.name).join(", ")}… Ask your question when the file chip appears below.`);
  }
}

$("attach-input").addEventListener("change", (e) => {
  takeFiles([...e.target.files]);
  e.target.value = "";
});

document.addEventListener("paste", (e) => {
  if (!$("lock").classList.contains("hidden")) return;
  const files = [...(e.clipboardData?.files || [])];
  if (!files.length) return; // plain text pastes normally
  e.preventDefault();
  if (state.view !== "chat") show("chat");
  takeFiles(files);
});

// ---- Files ----

async function upload(files, focusAfter) {
  if (!files.length) return;
  const status = $("upload-status");
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  status.textContent = `Reading and indexing ${files.length} file(s) locally…`;
  let scanned = [];
  try {
    const results = await api("POST", "/api/docs", fd);
    scanned = results.filter((r) => r.needs_ocr).map((r) => files.find((f) => f.name === r.name)).filter(Boolean);
    const errors = results.filter((r) => r.error && !r.needs_ocr);
    status.textContent = errors.length ? errors.map((r) => `${r.name}: ${r.error}`).join(" · ") : "";
    if (focusAfter) {
      for (const r of results) if (r.doc) state.focus.set(r.doc.id, r.doc.name);
      renderFocus();
      if (errors.length) addMessage("assistant", "⚠ " + errors.map((r) => `${r.name}: ${r.error}`).join("\n"));
    }
  } catch (err) {
    status.textContent = err.message;
    if (focusAfter) addMessage("assistant", "⚠ " + err.message);
  }
  if (state.view === "files") renderDocs();
  for (const f of scanned) await readScanned(f, focusAfter);
}

// ---- Scanned PDFs ----
// A scanned PDF has no text, only page pictures (and some PDFs can't be
// parsed on the server). Each page is drawn here
// with the bundled pdf.js and read by the vision model (POST /api/ocr);
// then the file is stored with that text. Nothing leaves this computer.

const OCR_MAX_PAGES = 300;
const OCR_PAGE_PX = 1600; // long side of each page image
let pdfjsLib = null;
let ocrStop = null;

async function loadPdfjs() {
  if (!pdfjsLib) {
    pdfjsLib = await import("./pdfjs/pdf.min.mjs");
    pdfjsLib.GlobalWorkerOptions.workerSrc = "./pdfjs/pdf.worker.min.mjs";
  }
  return pdfjsLib;
}

async function pageImage(pdf, n) {
  const page = await pdf.getPage(n);
  const base = page.getViewport({ scale: 1 });
  const viewport = page.getViewport({ scale: Math.min(OCR_PAGE_PX / Math.max(base.width, base.height), 4) });
  const canvas = document.createElement("canvas");
  canvas.width = Math.round(viewport.width);
  canvas.height = Math.round(viewport.height);
  const ctx = canvas.getContext("2d");
  ctx.fillStyle = "#fff";
  ctx.fillRect(0, 0, canvas.width, canvas.height);
  await page.render({ canvasContext: ctx, viewport }).promise;
  page.cleanup();
  return canvas.toDataURL("image/jpeg", 0.85);
}

function ocrProgress(text, value, max) {
  $("ocr-bar").classList.toggle("hidden", text == null);
  if (text == null) return;
  $("ocr-text").textContent = text;
  $("ocr-prog").max = max || 1;
  $("ocr-prog").value = value || 0;
}
$("ocr-stop").addEventListener("click", () => { if (ocrStop) ocrStop(); });

// "Gemma 3 12B (sharp image reading, …)" -> "Gemma 3 12B"
const shortModel = (name) => (name || "").replace(/\s*\(.*\)\s*$/, "");

function fmtDuration(ms) {
  const m = Math.round(ms / 60000);
  return m < 1 ? "under a minute" : m === 1 ? "about 1 minute" : `about ${m} minutes`;
}

async function readScanned(file, focusAfter) {
  const status = $("upload-status");
  const say = (msg) => { status.textContent = msg; if (focusAfter) addMessage("assistant", msg); };
  let pdf;
  try {
    const lib = await loadPdfjs();
    pdf = await lib.getDocument({ data: new Uint8Array(await file.arrayBuffer()), isEvalSupported: false }).promise;
  } catch (err) {
    say(`⚠ ${file.name}: couldn't open the PDF (${err.message})`);
    return;
  }
  const pages = Math.min(pdf.numPages, OCR_MAX_PAGES);
  let stopped = false;
  ocrStop = () => { stopped = true; ocrProgress(`Stopping after this page…`, 0, 1); };
  const parts = [];
  let model = "";
  const started = Date.now();
  try {
    for (let n = 1; n <= pages && !stopped; n++) {
      const eta = n > 1 ? ` — ${fmtDuration(((Date.now() - started) / (n - 1)) * (pages - n + 1))} left` : "";
      ocrProgress(`📄 Reading ${file.name} from its page images: page ${n} of ${pages}${model ? " with " + shortModel(model) : ""}${eta}`, n - 1, pages);
      const r = await api("POST", "/api/ocr", { image: await pageImage(pdf, n) });
      model = r.model;
      parts.push(`--- Page ${n} ---\n${r.text}`);
    }
  } catch (err) {
    ocrProgress(null);
    say(`⚠ ${file.name}: ${err.message}`);
    return;
  } finally {
    ocrStop = null;
    pdf.destroy();
  }
  ocrProgress(null);
  if (stopped) { say(`Stopped reading ${file.name}; nothing was saved.`); return; }
  const fd = new FormData();
  fd.append("ocr_text", parts.join("\n\n"));
  fd.append("ocr_model", model);
  fd.append("file", file, file.name);
  try {
    const [r] = await api("POST", "/api/docs", fd);
    if (r.error) throw new Error(r.error);
    const more = pdf.numPages > pages ? ` (only the first ${pages} of ${pdf.numPages} pages)` : "";
    status.textContent = `Read ${pages} scanned page(s) of ${file.name} with ${shortModel(model)}${more}.`;
    if (focusAfter) {
      state.focus.set(r.doc.id, r.doc.name);
      renderFocus();
      addMessage("assistant", `✓ Read ${pages} scanned page(s) of **${file.name}** with ${shortModel(model)}${more}. Ask your question.`);
    }
  } catch (err) {
    say(`⚠ ${file.name}: ${err.message}`);
  }
  if (state.view === "files") renderDocs();
}

$("file-input").addEventListener("change", async (e) => {
  await upload([...e.target.files], false);
  e.target.value = "";
});

async function renderDocs() {
  if (state.status) $("exts").textContent = (state.status.supported_ext || []).join(" ");
  const docs = (await api("GET", "/api/docs")) || [];
  const list = $("doc-list");
  list.replaceChildren();
  if (!docs.length) list.append(el("li", { class: "muted" }, "No files yet. Add some, or drop them anywhere."));
  for (const d of docs) {
    list.append(el("li", {},
      el("div", { class: "grow" }, d.name,
        el("div", { class: "sub" }, `${fmtSize(d.size)} · ${d.ocr ? "scan read by " + shortModel(d.ocr) + " · " : ""}${d.pieces} sections · ${d.embedded ? "semantic + keyword search" : "keyword search"} · ${fmtDate(d.added)}`)),
      el("button", { onclick: () => { state.focus.set(d.id, d.name); renderFocus(); show("chat"); } }, "Ask"),
      el("button", { onclick: () => window.open("/api/docs/" + d.id + "/file", "_blank", "noopener") }, "Open"),
      el("button", { class: "danger", onclick: async () => {
        if (!confirm(`Delete ${d.name} from the drive?`)) return;
        await api("DELETE", "/api/docs/" + d.id);
        state.focus.delete(d.id); renderFocus();
        renderDocs();
      } }, "Delete")));
  }
}

// Drag and drop anywhere: add files and ask about them.
let dragDepth = 0;
window.addEventListener("dragenter", (e) => {
  if (!e.dataTransfer || ![...e.dataTransfer.types].includes("Files")) return;
  if (!$("lock").classList.contains("hidden")) return;
  dragDepth++;
  $("drop").classList.remove("hidden");
});
window.addEventListener("dragleave", () => { if (--dragDepth <= 0) { dragDepth = 0; $("drop").classList.add("hidden"); } });
window.addEventListener("dragover", (e) => e.preventDefault());
window.addEventListener("drop", (e) => {
  e.preventDefault();
  dragDepth = 0;
  $("drop").classList.add("hidden");
  if (!$("lock").classList.contains("hidden")) return;
  const files = [...(e.dataTransfer?.files || [])];
  if (!files.length) return;
  if (state.view === "chat") takeFiles(files);
  else upload(files, false);
});

// ---- Agents ----

const agentPrefix = (id) => { const a = id && state.agents.find((x) => x.id === id); return a ? a.emoji + " " : ""; };

async function loadAgents() {
  try {
    state.agents = (await api("GET", "/api/agents")) || [];
    if (!state.catalog) state.catalog = await api("GET", "/api/agents/catalog");
  } catch { return; }
  if (state.agentId && !currentAgent()) state.agentId = "";
  const sel = $("agent-select");
  sel.replaceChildren(el("option", { value: "" }, "💬 Private AI"),
    ...state.agents.map((a) => el("option", { value: a.id }, `${a.emoji} ${a.name}`)));
  sel.value = state.agentId;
}

$("agent-select").addEventListener("change", (e) => {
  state.agentId = e.target.value;
  newChat();
  $("input").focus();
});

function chatWith(id) {
  state.agentId = id;
  $("agent-select").value = id;
  newChat();
  show("chat");
}

async function renderWeb() {
  let ws;
  try { ws = await api("GET", "/api/web"); } catch { return; }
  $("web-ddg").checked = ws.duckduckgo;
  $("web-brave").checked = ws.brave;
  $("brave-key").value = "";
  $("brave-key").placeholder = ws.brave_key_saved ? "•••••••• key saved (type to replace)" : "Brave API key";
  $("brave-remove").classList.toggle("hidden", !ws.brave_key_saved);
  state.web = ws;
  const on = ws.duckduckgo || (ws.brave && ws.brave_key_saved);
  $("web-status").textContent = on ? "Internet access is on for agents with 🌐 tools." : "Internet access is off.";
  if (ws.brave && !ws.brave_key_saved) $("web-status").textContent = "Add your Brave API key to use Brave Search.";
}
async function saveWeb(extra) {
  await api("POST", "/api/web", { duckduckgo: $("web-ddg").checked, brave: $("web-brave").checked, ...extra });
  renderWeb();
}
$("web-ddg").addEventListener("change", () => saveWeb());
$("web-brave").addEventListener("change", () => saveWeb());
$("brave-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const key = $("brave-key").value.trim();
  if (!key) return;
  await saveWeb({ brave_key: key, brave: true });
});
$("brave-remove").addEventListener("click", async () => {
  if (!confirm("Remove the saved Brave API key?")) return;
  $("web-brave").checked = false;
  await saveWeb({ brave_key: "-" });
});
$("web-test").addEventListener("click", async () => {
  $("web-status").textContent = "Testing…";
  try {
    const r = await api("POST", "/api/web/test");
    $("web-status").textContent = `✓ Working — ${r.results} results via ${r.provider}.`;
  } catch (err) {
    $("web-status").textContent = "✗ " + err.message;
  }
});

const usesWeb = (a) => (a.tools || []).some((t) => t === "web_search" || t === "read_webpage");

async function renderAgents() {
  renderWeb();
  await loadAgents();
  const list = $("agent-list");
  list.replaceChildren();
  if (!state.agents.length) list.append(el("li", { class: "muted" }, "No agents yet. Create one, or start from a template below."));
  for (const a of state.agents) {
    list.append(el("li", {},
      el("div", { class: "agent-emoji" }, a.emoji),
      el("div", { class: "grow" }, a.name, usesWeb(a) ? el("span", { class: "web-badge", title: "This agent can use the internet when it's switched on" }, "🌐 internet") : null,
        el("div", { class: "sub" }, [a.description, (a.tools || []).length ? "Tools: " + a.tools.map(toolLabel).join(", ") : "No tools",
          { all: "All files", selected: `${(a.doc_ids || []).length} selected file(s)`, none: "No files" }[a.knowledge] || ""].filter(Boolean).join(" · "))),
      el("button", { class: "primary", onclick: () => chatWith(a.id) }, "Chat"),
      el("button", { onclick: () => editAgent(a) }, "Edit"),
      el("button", { class: "danger", onclick: async () => {
        if (!confirm(`Delete the agent ${a.name}? Its past chats are kept.`)) return;
        await api("DELETE", "/api/agents/" + a.id);
        if (state.agentId === a.id) { state.agentId = ""; newChat(); }
        renderAgents();
      } }, "Delete")));
  }
  $("agent-templates").replaceChildren(...(state.catalog ? state.catalog.templates : []).map((t) =>
    el("button", { type: "button", title: t.description, onclick: () => editAgent({ ...t, id: "" }) }, `${t.emoji} ${t.name}`)));
}

let editingAgent = null;
async function editAgent(a) {
  editingAgent = a || { name: "", emoji: "🤖", description: "", instructions: "", tools: [], knowledge: "all", doc_ids: [], temperature: 0.6 };
  const ag = editingAgent;
  $("agent-form-title").textContent = ag.id ? "Edit agent" : "New agent";
  $("agent-emoji").value = ag.emoji || "🤖";
  $("agent-name").value = ag.name || "";
  $("agent-desc").value = ag.description || "";
  $("agent-instr").value = ag.instructions || "";
  $("agent-temp").value = ag.temperature ?? 0.6;
  $("agent-temp-out").textContent = $("agent-temp").value;
  $("agent-error").textContent = "";
  $("agent-tools").replaceChildren(...(state.catalog ? state.catalog.tools : []).map((t) =>
    el("label", {}, el("input", { type: "checkbox", value: t.id, ...((ag.tools || []).includes(t.id) ? { checked: "" } : {}) }),
      el("span", {}, t.label, el("small", {}, t.description)))));
  $("agent-search-first").checked = !!ag.search_first;
  fillVoices($("agent-voice"), ag.voice || "", "Default voice (Settings)");
  syncSearchFirst();
  document.querySelectorAll("input[name=knowledge]").forEach((r) => { r.checked = r.value === (ag.knowledge || "all"); });
  const docs = (await api("GET", "/api/docs")) || [];
  $("agent-docs").replaceChildren(...(docs.length ? docs.map((d) =>
    el("label", {}, el("input", { type: "checkbox", value: d.id, ...((ag.doc_ids || []).includes(d.id) ? { checked: "" } : {}) }), el("span", {}, d.name)))
    : [el("span", { class: "muted" }, "No files yet — add some in Files.")]));
  syncKnowledge();
  $("agent-form-wrap").classList.remove("hidden");
  $("agent-name").focus();
  $("agent-form-wrap").scrollIntoView({ block: "start", behavior: "smooth" });
}
// "Always search the web" only makes sense with the web search tool.
function syncSearchFirst() {
  const web = document.querySelector('#agent-tools input[value="web_search"]');
  const on = !!(web && web.checked);
  $("agent-search-first").disabled = !on;
  if (!on) $("agent-search-first").checked = false;
  $("agent-search-first").parentElement.classList.toggle("muted", !on);
}
$("agent-tools").addEventListener("change", syncSearchFirst);
function syncKnowledge() {
  const k = document.querySelector("input[name=knowledge]:checked").value;
  $("agent-docs").classList.toggle("hidden", k !== "selected");
}
document.querySelectorAll("input[name=knowledge]").forEach((r) => r.addEventListener("change", syncKnowledge));
$("agent-temp").addEventListener("input", () => { $("agent-temp-out").textContent = $("agent-temp").value; });
$("agent-new").addEventListener("click", () => editAgent(null));
$("agent-cancel").addEventListener("click", () => $("agent-form-wrap").classList.add("hidden"));
$("agent-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const checked = (sel) => [...document.querySelectorAll(sel + " input:checked")].map((i) => i.value);
  const body = {
    ...(editingAgent || {}),
    emoji: $("agent-emoji").value.trim(),
    name: $("agent-name").value.trim(),
    description: $("agent-desc").value.trim(),
    instructions: $("agent-instr").value,
    tools: checked("#agent-tools"),
    knowledge: document.querySelector("input[name=knowledge]:checked").value,
    doc_ids: checked("#agent-docs"),
    temperature: parseFloat($("agent-temp").value),
    search_first: $("agent-search-first").checked,
    voice: $("agent-voice").value,
  };
  try {
    await api("POST", "/api/agents", body);
    $("agent-form-wrap").classList.add("hidden");
    renderAgents();
  } catch (err) {
    $("agent-error").textContent = err.message;
  }
});

// ---- Memory ----

async function renderMemory() {
  const items = (await api("GET", "/api/memory")) || [];
  const list = $("memory-list");
  list.replaceChildren();
  if (!items.length) list.append(el("li", { class: "muted" }, "Nothing remembered yet."));
  for (const m of items) {
    list.append(el("li", {}, el("div", { class: "grow" }, m.text),
      el("button", { class: "danger", onclick: async () => { await api("DELETE", "/api/memory/" + m.id); renderMemory(); } }, "Forget")));
  }
}
$("memory-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const text = $("memory-input").value.trim();
  if (!text) return;
  await api("POST", "/api/memory", { text });
  $("memory-input").value = "";
  renderMemory();
});

// ---- Chats ----

async function renderChats() {
  const chats = (await api("GET", "/api/chats")) || [];
  const list = $("chat-list");
  list.replaceChildren();
  if (!chats.length) list.append(el("li", { class: "muted" }, "No conversations yet."));
  for (const c of chats) {
    list.append(el("li", {},
      el("div", { class: "grow" }, agentPrefix(c.agent_id) + c.title, el("div", { class: "sub" }, fmtDate(c.updated))),
      el("button", { onclick: () => openChat(c.id) }, "Open"),
      el("button", { class: "danger", onclick: async () => {
        if (!confirm("Delete this conversation?")) return;
        await api("DELETE", "/api/chats/" + c.id);
        if (state.chatId === c.id) newChat();
        renderChats();
      } }, "Delete")));
  }
}

// ---- Settings ----

function renderSettings() {
  const st = state.status;
  if (!st) return;
  renderSpeechStatus();
  const models = $("model-list");
  models.replaceChildren();
  const ram = Math.floor(st.host.ram_bytes / 2 ** 30);
  for (const m of st.models || []) {
    const tooBig = ram && m.min_ram_gb > ram;
    const row = el("div", { class: "row" + (m.active ? " active" : "") },
      el("div", { class: "grow" }, m.name,
        el("div", { class: "sub" }, m.problem ? "⚠ " + m.problem : !m.present ? "Not on this drive" :
          `Needs ~${m.min_ram_gb} GB RAM` + (m.vision ? " · sees images" : "") + (tooBig ? " — may be slow or fail on this computer" : "") +
          (m.active ? ` · ${st.chat_state === "ready" ? "active" : st.chat_state}` : ""))));
    if (m.present && !m.active) {
      row.append(el("button", { onclick: async () => { await api("POST", "/api/models/select", { id: m.id }); refreshStatus(); } }, "Use"));
    }
    models.append(row);
  }
  if (st.chat_error) models.append(el("div", { class: "row error" }, st.chat_error));

  // Image reader.
  const sel = $("vision-select");
  if (document.activeElement !== sel) {
    const choices = (st.models || []).filter((m) => m.present && m.vision && !m.active);
    sel.replaceChildren(el("option", { value: "" }, "Automatic"), el("option", { value: "off" }, "Off"),
      ...choices.map((m) => el("option", { value: m.id }, m.name)));
    sel.value = st.vision_choice || "";
  }
  const vs = { ready: `● Running ${st.vision_model}`, starting: `Loading ${st.vision_model}…`,
    error: `Failed: ${st.vision_error}`, off: `Not running — ${st.vision_error || "off"}` }[st.vision_state] || "";
  $("vision-status").textContent = vs;

  // Model cache.
  $("cache-on").checked = !!st.cache_on;
  const gb = (st.cache_bytes || 0) / 2 ** 30;
  $("cache-status").textContent = st.cache_bytes
    ? `Cached on this computer (${gb.toFixed(1)} GB): ${(st.cache_names || []).join(", ") || "copying…"}`
    : st.cache_on ? "Nothing cached yet — models are copied after they load." : "Nothing cached on this computer.";
  $("cache-clear").classList.toggle("hidden", !st.cache_bytes);

  const facts = [
    ["Private AI version", st.version || "dev"],
    ["System", `${st.host.os} ${st.host.arch}`],
    ["CPU threads", st.host.cpus],
    ["Memory", ram ? ram + " GB" : "unknown"],
    ["Acceleration", (st.host.accel || []).join(", ") || "CPU only"],
    ["Runtime in use", st.chat_runtime || "—"],
    ["Context window", st.context_size ? st.context_size + " tokens" : "—"],
    ["Search model", st.embed_state === "ready" ? st.embed_model : st.embed_state === "starting" ? "loading…" : "keyword search only"],
  ];
  $("host-info").replaceChildren(...facts.flatMap(([k, v]) => [el("dt", {}, k), el("dd", {}, String(v))]));
}

$("lock-now").addEventListener("click", async () => {
  await api("POST", "/api/lock");
  newChat();
  await refreshStatus();
  showLock();
});
$("shutdown").addEventListener("click", async () => {
  if (!confirm("Shut down Private AI? You can then safely remove the drive.")) return;
  try { await api("POST", "/api/shutdown"); } catch {}
  document.querySelectorAll(".screen").forEach((v) => v.classList.add("hidden"));
  $("tabs").classList.add("hidden");
  $("bye").classList.remove("hidden");
  $("engine").textContent = "Stopped";
  clearInterval(poll);
});

$("cache-on").addEventListener("change", async (e) => {
  await api("POST", "/api/cache", { enabled: e.target.checked });
  refreshStatus();
});
$("cache-clear").addEventListener("click", async () => {
  if (!confirm("Remove the cached models from this computer? They stay on the drive.")) return;
  await api("DELETE", "/api/cache");
  refreshStatus();
});
$("vision-select").addEventListener("change", async (e) => {
  await api("POST", "/api/vision/select", { id: e.target.value });
  e.target.blur();
  refreshStatus();
});
$("load-tip-on").addEventListener("click", async () => {
  await api("POST", "/api/cache", { enabled: true });
  state.tipDismissed = true;
  $("load-tip").classList.add("hidden");
  refreshStatus();
});
$("load-tip-close").addEventListener("click", () => { state.tipDismissed = true; $("load-tip").classList.add("hidden"); });
$("engine-warn-close").addEventListener("click", () => {
  state.dismissedWarn = state.status && state.status.chat_warning;
  $("engine-warn").classList.add("hidden");
});
$("engine-retry").addEventListener("click", async () => {
  $("engine-error").classList.add("hidden");
  try { await api("POST", "/api/models/retry"); } catch {}
  refreshStatus();
});

// ---- Theme: Matrix (default) or Classic ----

const prefs = (() => {
  const get = (k, d) => { try { return localStorage.getItem(k) ?? d; } catch { return d; } };
  const set = (k, v) => { try { localStorage.setItem(k, v); } catch {} };
  return { get, set };
})();

const THEMES = ["matrix", "claude", "terminal", "classic"];
const theme = () => { const t = prefs.get("theme", "matrix"); return THEMES.includes(t) ? t : "matrix"; };

function applyTheme() {
  const t = theme();
  document.documentElement.dataset.theme = t;
  document.documentElement.dataset.family = t === "classic" ? "plain" : "term";
  document.querySelectorAll(".theme-btn").forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.themeChoice === t)));
  if (!$("lock").classList.contains("hidden")) typeWake();
}
document.querySelectorAll(".theme-btn").forEach((b) => b.addEventListener("click", () => {
  prefs.set("theme", b.dataset.themeChoice);
  applyTheme();
}));
applyTheme();

// A themed greeting typed on the lock screen.
let wakeTimer = null;
function typeWake() {
  const box = $("wake");
  clearTimeout(wakeTimer);
  box.textContent = "";
  const init = state.status && state.status.initialized;
  const now = new Date();
  const lines = {
    matrix: init ? "Wake up…\nThe Matrix has you.\nKnock, knock." : "Wake up…\nFollow the white rabbit.",
    claude: init ? "✻ Welcome back to Private AI!\n  /unlock to continue" : "✻ Welcome to Private AI!\n  /init to create your vault",
    terminal: `Last login: ${now.toDateString().slice(0, 10)} ${now.toTimeString().slice(0, 8)} on ttys000\n~ % ${init ? "privateai unlock" : "privateai init"}`,
  }[theme()];
  if (!lines) return;
  let i = 0;
  const step = () => {
    box.textContent = lines.slice(0, ++i);
    if (i < lines.length) wakeTimer = setTimeout(step, lines[i - 1] === "\n" ? 450 : 55);
  };
  step();
}

// ---- Boot ----

const poll = setInterval(refreshStatus, 3000);
(async () => {
  const st = await refreshStatus();
  if (!st) return;
  if (st.locked) showLock();
  else { await loadAgents(); newChat(); $("tabs").classList.remove("hidden"); show("chat"); }
})();


// ---- Voice ----
// Talk-to-text: the microphone is recorded here, converted to 16 kHz WAV and
// transcribed by the speech model on the drive (POST /api/transcribe).
// Replies are read aloud with natural voices (the Kokoro model on the drive,
// run in a background worker) or with the computer's own on-device voices.
// No audio or text goes to a cloud speech service.

const voicePrefs = {
  get voice() { return prefs.get("voice", ""); },
  get rate() { return parseFloat(prefs.get("voice-rate", "1")) || 1; },
  get speak() { return prefs.get("speak-replies", "0") === "1"; },
  get handsfree() { return prefs.get("voice-handsfree", "1") === "1"; },
  get engine() { return prefs.get("voice-engine", "auto"); },
};
const synth = window.speechSynthesis;

// Kokoro voice ids are like "am_michael": a = US, b = UK; f/m = female/male.
const KOKORO_BEST = ["af_heart", "af_bella", "af_nicole", "am_michael", "am_fenrir", "am_puck", "bf_emma", "bm_george", "bm_fable"];
const naturalVoices = () => ((state.status && state.status.voices) || []).slice()
  .sort((a, b) => ((KOKORO_BEST.indexOf(a) + 1 || 99) - (KOKORO_BEST.indexOf(b) + 1 || 99)) || a.localeCompare(b));
function naturalLabel(id) {
  const who = { af: "US woman", am: "US man", bf: "UK woman", bm: "UK man" }[id.slice(0, 2)] || "";
  const name = id.slice(3) || id;
  return `${name[0].toUpperCase()}${name.slice(1)}${who ? ` (${who})` : ""}${KOKORO_BEST.includes(id) ? " ★" : ""}`;
}

function systemVoices() {
  if (!synth) return [];
  const lang = (navigator.language || "en").slice(0, 2);
  return synth.getVoices().filter((v) => v.localService)
    .sort((a, b) => (b.lang.startsWith(lang) - a.lang.startsWith(lang)) ||
      (/premium|enhanced/i.test(b.name) - /premium|enhanced/i.test(a.name)) || a.name.localeCompare(b.name));
}

// A voice setting is "kokoro:<id>" for a natural voice, or a system voice
// name. Empty means the default: the best natural voice when available.
function resolveVoice(v) {
  v = v || voicePrefs.voice;
  const nat = naturalVoices();
  if (v.startsWith("kokoro:") && nat.includes(v.slice(7))) return v;
  if (v && !v.startsWith("kokoro:") && systemVoices().some((s) => s.name === v)) return v;
  if (nat.length) return "kokoro:" + nat[0];
  return "";
}
const agentVoice = (ag) => resolveVoice(ag && ag.voice);

function fillVoices(sel, current, firstLabel) {
  const nat = naturalVoices(), sys = systemVoices();
  const groups = [el("option", { value: "" }, firstLabel)];
  if (nat.length) groups.push(el("optgroup", { label: "Natural voices (on this drive)" },
    ...nat.map((id) => el("option", { value: "kokoro:" + id }, naturalLabel(id)))));
  if (sys.length) groups.push(el("optgroup", { label: "This computer's voices" },
    ...sys.map((v) => el("option", { value: v.name }, `${v.name} (${v.lang})`))));
  const known = !current || (current.startsWith("kokoro:") ? nat.includes(current.slice(7)) : sys.some((v) => v.name === current));
  if (!known) groups.push(el("option", { value: current }, `${current} (not available here)`));
  sel.replaceChildren(...groups);
  sel.value = current;
}

// Text as it should sound: no Markdown symbols, code, links or emoji.
function speechText(t) {
  return latexLite(t).replace(/<think>[\s\S]*?(<\/think>|$)/g, "").replace(/<tool_call>[\s\S]*?(<\/tool_call>|$)/g, "")
    .replace(/```[\s\S]*?(```|$)/g, " The code is shown on screen. ")
    .replace(/`([^`]*)`/g, "$1")
    .replace(/!\[[^\]]*\]\([^)]*\)/g, "")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/https?:\/\/\S+/g, "a link")
    .replace(/^\s*\|?\s*:?-{3,}[\s|:-]*$/gm, "")
    .replace(/\|/g, ", ")
    .replace(/^#{1,6}\s*/gm, "").replace(/^\s*>\s?/gm, "").replace(/^\s*[-*+•]\s+/gm, "")
    .replace(/\*\*|__|~~|\*|(?<=\s|^)_|_(?=\s|$)/g, "")
    .replace(/\p{Extended_Pictographic}️?/gu, "")
    .replace(/\s+/g, " ").trim();
}

// Where the first complete, speakable sentence of t ends (0 if none yet).
// Never inside an unfinished code block or <think> section.
function sentenceEnd(t) {
  const re = /[.!?…]+["'”’)\]]*(?=\s)|\n+/g;
  let m;
  while ((m = re.exec(t))) {
    const end = m.index + m[0].length;
    const head = t.slice(0, end);
    if (/\d$/.test(t.slice(0, m.index)) && m[0][0] === ".") continue; // "1." in a list
    if ((head.match(/```/g) || []).length % 2) continue;
    if (/<think>(?![\s\S]*<\/think>)/.test(head)) continue;
    if (speechText(head).length < 2) continue;
    return end;
  }
  return 0;
}

// ---- Natural voices (Kokoro, in a worker) ----

const kokoro = { worker: null, ready: null, info: null, error: "", seq: 0, waiting: new Map() };

function kokoroCall(type, data, transfer) {
  const id = ++kokoro.seq;
  return new Promise((resolve, reject) => {
    kokoro.waiting.set(id, { resolve, reject });
    kokoro.worker.postMessage({ id, type, ...data }, transfer || []);
  });
}

function kokoroLoad() {
  if (!kokoro.ready) {
    kokoro.worker = new Worker("kokoro/tts-worker.js", { type: "module" });
    kokoro.worker.onmessage = ({ data }) => {
      const w = kokoro.waiting.get(data.id);
      if (!w) return;
      kokoro.waiting.delete(data.id);
      if (data.ok) w.resolve(data); else w.reject(new Error(data.error));
    };
    kokoro.worker.onerror = (e) => {
      for (const w of kokoro.waiting.values()) w.reject(new Error(e.message || "voice engine crashed"));
      kokoro.waiting.clear();
    };
    kokoro.ready = kokoroCall("load", { engine: voicePrefs.engine }).then((info) => {
      kokoro.info = info;
      kokoro.error = "";
      renderVoiceEngine();
      return info;
    }, (err) => {
      kokoro.error = err.message;
      kokoro.ready = null;
      kokoro.worker.terminate();
      renderVoiceEngine();
      throw err;
    });
    renderVoiceEngine();
  }
  return kokoro.ready;
}

function renderVoiceEngine() {
  const box = $("voice-engine-status");
  if (!box) return;
  if (!naturalVoices().length) { box.textContent = "Natural voices are not on this drive yet (run the update)."; return; }
  if (kokoro.error) { box.textContent = "Natural voices failed to start: " + kokoro.error + ". This computer's voices are used instead."; return; }
  if (!kokoro.info) { box.textContent = kokoro.ready ? "Warming up natural voices…" : "Natural voices start the first time they speak."; return; }
  const where = kokoro.info.device === "webgpu" ? "the graphics chip" : "the processor";
  const x = kokoro.info.speed;
  box.textContent = `Natural voices run on ${where}, ${x >= 1 ? x.toFixed(1) + "× faster than real time" : "slower than real time: replies may pause between sentences"}.`;
}

let audioCtx = null;
function getAudioCtx() {
  audioCtx = audioCtx || new (window.AudioContext || window.webkitAudioContext)();
  if (audioCtx.state === "suspended") audioCtx.resume();
  return audioCtx;
}

// ---- Speakers: read a streaming reply aloud, sentence by sentence ----
// All speakers have feed(raw), end(raw, onDone), stop(), and call onStart
// when sound begins. level() is the output loudness (0..1) when known.

let activeSpeaker = null;
function makeSpeaker(voice, hooks = {}) {
  voice = resolveVoice(voice);
  if (activeSpeaker) activeSpeaker.stop();
  let sp = null;
  if (voice.startsWith("kokoro:")) sp = naturalSpeaker(voice.slice(7), hooks);
  else if (synth && systemVoices().length) sp = systemSpeaker(voice, hooks);
  activeSpeaker = sp;
  return sp;
}

function speakerBase(say, hooks) {
  let done = 0, ended = false, stopped = false, onIdle = null;
  const sp = {
    busy: () => false,
    feed(raw) {
      let cut;
      while (!stopped && (cut = sentenceEnd(raw.slice(done)))) { say(raw.slice(done, done + cut)); done += cut; }
    },
    end(raw, cb) {
      if (!stopped) say(raw.slice(done));
      done = raw.length; ended = true; onIdle = stopped ? null : cb; sp.check();
    },
    check() { if (ended && !sp.busy() && onIdle) { const f = onIdle; onIdle = null; f(); } },
    stopped: () => stopped,
    stop() { stopped = true; onIdle = null; sp.halt(); if (activeSpeaker === sp) activeSpeaker = null; },
    level: () => 0,
  };
  return sp;
}

function systemSpeaker(name, hooks) {
  const v = systemVoices().find((x) => x.name === name) || systemVoices().find((x) => x.default) || systemVoices()[0];
  let pending = 0, started = false;
  const sp = speakerBase((text) => {
    const t = speechText(text);
    if (!t || sp.stopped()) return;
    const u = new SpeechSynthesisUtterance(t);
    if (v) { u.voice = v; u.lang = v.lang; }
    u.rate = voicePrefs.rate;
    pending++;
    u.onstart = () => { if (!started) { started = true; hooks.onStart && hooks.onStart(); } };
    u.onend = u.onerror = () => { pending--; sp.check(); };
    synth.speak(u);
  }, hooks);
  sp.busy = () => pending > 0;
  sp.halt = () => synth.cancel();
  return sp;
}

function naturalSpeaker(voiceId, hooks) {
  const queue = [];
  let making = false, playing = 0, playEnd = 0, started = false, fallback = null;
  const sources = new Set();
  const ctx = getAudioCtx();
  const out = ctx.createAnalyser();
  out.fftSize = 512;
  out.connect(ctx.destination);
  const buf = new Float32Array(out.fftSize);
  const play = (audio, rate) => {
    const b = ctx.createBuffer(1, audio.length, rate);
    b.copyToChannel(audio, 0);
    const src = ctx.createBufferSource();
    src.buffer = b;
    src.connect(out);
    const at = Math.max(ctx.currentTime + 0.05, playEnd);
    playEnd = at + b.duration;
    playing++;
    sources.add(src);
    src.onended = () => { playing--; sources.delete(src); sp.check(); };
    src.start(at);
    if (!started) { started = true; setTimeout(() => !sp.stopped() && hooks.onStart && hooks.onStart(), Math.max(0, (at - ctx.currentTime) * 1000)); }
  };
  const pump = async () => {
    if (making || sp.stopped() || !queue.length) { sp.check(); return; }
    making = true;
    const text = queue.shift();
    try {
      await kokoroLoad();
      if (sp.stopped()) return;
      const { audio, rate } = await kokoroCall("speak", { text, voice: voiceId, speed: voicePrefs.rate });
      if (!sp.stopped()) play(audio, rate);
    } catch {
      // Natural voices unavailable: finish with the computer's voice.
      if (!fallback && synth && systemVoices().length) fallback = systemSpeaker("", hooks);
      if (fallback && !sp.stopped()) { fallback.feed(text + "\n"); }
    } finally {
      making = false;
      pump();
    }
  };
  const sp = speakerBase((text) => {
    const t = speechText(text);
    if (t) { queue.push(t); pump(); }
  }, hooks);
  sp.busy = () => making || playing > 0 || queue.length > 0 || (fallback ? fallback.busy() : false);
  sp.halt = () => {
    queue.length = 0;
    for (const s of sources) { try { s.stop(); } catch {} }
    sources.clear();
    if (fallback) fallback.stop();
  };
  sp.level = () => {
    out.getFloatTimeDomainData(buf);
    let s = 0;
    for (let i = 0; i < buf.length; i++) s += buf[i] * buf[i];
    return Math.min(1, Math.sqrt(s / buf.length) * 4);
  };
  return sp;
}

function readAloud(text, agentLabel) {
  const ag = state.agents.find((a) => agentLabel && agentLabel === `${a.emoji} ${a.name}`) || currentAgent();
  const sp = makeSpeaker(agentVoice(ag));
  if (!sp) { addMessage("assistant", "⚠ No voices are available to read with."); return; }
  sp.end(text, null);
}

function testVoice(v) {
  const sp = makeSpeaker(v);
  if (sp) sp.end("Hi! This is how I sound. Everything I say is made right here on this computer.", null);
}

// Safari only lets a page make sound after a click; unlock audio on one.
function primeAudio() {
  getAudioCtx();
  if (synth && !primeAudio.done) { synth.speak(new SpeechSynthesisUtterance("")); primeAudio.done = true; }
}

function renderVoiceSettings() {
  fillVoices($("voice-default"), voicePrefs.voice, "Automatic (best available)");
  $("voice-engine").value = voicePrefs.engine;
  $("voice-rate").value = voicePrefs.rate;
  $("voice-rate-out").textContent = voicePrefs.rate.toFixed(1) + "×";
  $("voice-handsfree").checked = voicePrefs.handsfree;
  renderSpeechStatus();
  renderVoiceEngine();
}

function renderSpeechStatus() {
  const st = state.status || {};
  $("speech-status").textContent = {
    ready: `${st.speech_model}, ready`, starting: `${st.speech_model || "speech model"}, loading…`,
    no_model: "not on this drive yet: run the update to download it", error: "it failed to start",
  }[st.speech_state] || "checking…";
}

if (synth) synth.addEventListener("voiceschanged", () => {
  if (state.view === "settings") renderVoiceSettings();
  if (!$("agent-form-wrap").classList.contains("hidden")) fillVoices($("agent-voice"), $("agent-voice").value, "Default voice (Settings)");
});
$("voice-default").addEventListener("change", (e) => { prefs.set("voice", e.target.value); primeAudio(); testVoice(e.target.value); });
$("voice-test").addEventListener("click", () => { primeAudio(); testVoice($("voice-default").value); });
$("agent-voice-test").addEventListener("click", () => { primeAudio(); testVoice($("agent-voice").value); });
$("voice-engine").addEventListener("change", (e) => {
  prefs.set("voice-engine", e.target.value);
  if (kokoro.worker) kokoro.worker.terminate();
  Object.assign(kokoro, { worker: null, ready: null, info: null, error: "" });
  renderVoiceEngine();
});
$("voice-rate").addEventListener("input", (e) => { prefs.set("voice-rate", e.target.value); $("voice-rate-out").textContent = parseFloat(e.target.value).toFixed(1) + "×"; });
$("voice-handsfree").addEventListener("change", (e) => prefs.set("voice-handsfree", e.target.checked ? "1" : "0"));
$("speak-replies").checked = voicePrefs.speak;
$("speak-replies").addEventListener("change", (e) => {
  prefs.set("speak-replies", e.target.checked ? "1" : "0");
  if (e.target.checked) { primeAudio(); if (resolveVoice("").startsWith("kokoro:")) kokoroLoad().catch(() => {}); }
  else if (activeSpeaker) activeSpeaker.stop();
});
const settingsTab = document.querySelector('.tabs button[data-view="settings"]');
if (settingsTab) settingsTab.addEventListener("click", renderVoiceSettings);

// ---- Microphone ----

const SPEECH_RATE = 16000;

// openMic streams microphone chunks to onChunk(samples, rms) until closed.
async function openMic(onChunk) {
  const ctx = getAudioCtx();
  await ctx.resume();
  const stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true } });
  const src = ctx.createMediaStreamSource(stream);
  const proc = ctx.createScriptProcessor(2048, 1, 1);
  const mute = ctx.createGain();
  mute.gain.value = 0;
  proc.onaudioprocess = (e) => {
    const d = new Float32Array(e.inputBuffer.getChannelData(0));
    let s = 0;
    for (let i = 0; i < d.length; i++) s += d[i] * d[i];
    onChunk(d, Math.sqrt(s / d.length));
  };
  src.connect(proc);
  proc.connect(mute);
  mute.connect(ctx.destination);
  return {
    rate: ctx.sampleRate,
    seconds: (n) => n * 2048 / ctx.sampleRate,
    close() { proc.onaudioprocess = null; proc.disconnect(); src.disconnect(); mute.disconnect(); stream.getTracks().forEach((t) => t.stop()); },
  };
}

function micError(err) {
  return err.name === "NotAllowedError"
    ? "allow microphone access for this page in your browser (Safari: Settings → Websites → Microphone)" : err.message;
}

let mic = null; // the 🎤 recording in progress

function renderMic() {
  const st = state.status || {};
  const ok = st.speech_state === "ready";
  for (const b of [$("mic"), $("call-btn")]) b.classList.toggle("off", !ok && !mic && !call);
  $("mic").title = mic ? "Stop and send" : ok ? "Talk instead of typing" :
    st.speech_state === "starting" ? "The voice model is loading…" :
    st.speech_state === "no_model" ? "No speech model on this drive yet" : "The voice model is not running";
  $("call-btn").title = ok ? "Live voice call" : $("mic").title;
}

function micState(s) {
  const b = $("mic");
  b.classList.toggle("recording", s === "recording");
  b.classList.toggle("working", s === "working");
  b.textContent = s === "working" ? "…" : "🎤";
  if (s !== "recording") b.style.removeProperty("--level");
  renderMic();
}

function voiceReady(auto) {
  const st = state.status || {};
  if (st.speech_state !== "ready") {
    if (!auto) addMessage("assistant", "⚠ Voice input isn't available: " + $("mic").title.toLowerCase() + ".");
    return false;
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    if (!auto) addMessage("assistant", "⚠ This browser can't use the microphone here.");
    return false;
  }
  return true;
}

async function listen(auto) {
  if (mic) { mic.finish(); return; }
  if (call || !voiceReady(auto)) return;
  if (activeSpeaker) activeSpeaker.stop();
  const chunks = [];
  let floor = 1, heard = false, quietSince = 0, t = 0, closed = false, m;
  const close = () => { if (closed) return false; closed = true; m.close(); mic = null; return true; };
  try {
    m = await openMic((d, rms) => {
      if (closed) return;
      chunks.push(d);
      t = m.seconds(chunks.length) * 1000;
      $("mic").style.setProperty("--level", Math.min(1, rms * 12).toFixed(2));
      // The quietest moment of the first 300 ms is the room's background
      // noise (the minimum, so talking right away doesn't raise it).
      if (t < 300) { floor = Math.min(floor, rms); return; }
      if (rms > Math.max(0.012, Math.min(floor, 0.02) * 3)) { heard = true; quietSince = 0; }
      else if (heard) { quietSince = quietSince || t; if (t - quietSince > 1500) mic.finish(); }
      if (!heard && t > (auto ? 8000 : 12000)) mic.cancel(); // nobody spoke
      if (t > 120000) mic.finish();
    });
  } catch (err) {
    if (!auto) addMessage("assistant", "⚠ Microphone not available: " + micError(err) + ".");
    return;
  }
  mic = {
    finish() { if (close()) transcribeTurn(chunks, m.rate, auto, heard); },
    cancel() { if (close()) micState("idle"); },
  };
  micState("recording");
}

async function transcribeChunks(chunks, rate) {
  const wav = encodeWAV(downsample(chunks, rate, SPEECH_RATE), SPEECH_RATE);
  const { text } = await api("POST", "/api/transcribe", { audio: toBase64(wav) });
  return text;
}

async function transcribeTurn(chunks, rate, auto, heard) {
  if (!heard) { micState("idle"); return; }
  micState("working");
  try {
    const text = await transcribeChunks(chunks, rate);
    if (!text) { if (!auto) addMessage("assistant", "🎤 I didn't catch that — try again a little closer to the mic."); return; }
    if (voicePrefs.handsfree && modelReady() && !state.busy && !$("input").value.trim()) {
      send(text, { fromVoice: true });
    } else {
      const box = $("input");
      box.value = (box.value.trim() ? box.value.trimEnd() + " " : "") + text;
      autosize();
      box.focus();
    }
  } catch (err) {
    addMessage("assistant", "⚠ Voice input: " + err.message);
  } finally {
    micState("idle");
  }
}

function downsample(chunks, from, to) {
  const len = chunks.reduce((n, c) => n + c.length, 0);
  const all = new Float32Array(len);
  let o = 0;
  for (const c of chunks) { all.set(c, o); o += c.length; }
  if (from === to) return all;
  const ratio = from / to;
  const out = new Float32Array(Math.floor(len / ratio));
  for (let i = 0; i < out.length; i++) {
    const a = Math.floor(i * ratio), b = Math.min(len, Math.floor((i + 1) * ratio));
    let s = 0;
    for (let j = a; j < b; j++) s += all[j];
    out[i] = s / Math.max(1, b - a);
  }
  return out;
}

function encodeWAV(samples, rate) {
  const buf = new ArrayBuffer(44 + samples.length * 2);
  const v = new DataView(buf);
  const str = (o, s) => { for (let i = 0; i < s.length; i++) v.setUint8(o + i, s.charCodeAt(i)); };
  str(0, "RIFF"); v.setUint32(4, 36 + samples.length * 2, true); str(8, "WAVE");
  str(12, "fmt "); v.setUint32(16, 16, true); v.setUint16(20, 1, true); v.setUint16(22, 1, true);
  v.setUint32(24, rate, true); v.setUint32(28, rate * 2, true); v.setUint16(32, 2, true); v.setUint16(34, 16, true);
  str(36, "data"); v.setUint32(40, samples.length * 2, true);
  for (let i = 0; i < samples.length; i++) {
    const x = Math.max(-1, Math.min(1, samples[i]));
    v.setInt16(44 + i * 2, x < 0 ? x * 0x8000 : x * 0x7fff, true);
  }
  return new Uint8Array(buf);
}

function toBase64(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  return btoa(s);
}

$("mic").addEventListener("click", () => { primeAudio(); listen(false); });

// ---- Live voice call ----
// The mic stays open for the whole call. Each pause ends your turn: it is
// transcribed and sent, and the reply is spoken as it is written. Speaking
// while the agent talks (or thinks) interrupts it and starts your next turn.

let call = null;

const CALL_PAUSE_MS = 1200; // silence that ends your turn
const CALL_BARGE_MS = 250; // speech that interrupts the agent

function callSay(stateName, label) {
  call.state = stateName;
  $("call").dataset.state = stateName;
  $("call-state").textContent = label;
}

async function startCall() {
  if (call) return;
  primeAudio();
  if (!voiceReady(false)) return;
  if (!modelReady()) { addMessage("assistant", "⚠ The AI model is still loading — try the call again in a moment."); return; }
  if (activeSpeaker) activeSpeaker.stop();
  const ag = currentAgent();
  $("call-emoji").textContent = ag ? ag.emoji : "💬";
  $("call-name").textContent = ag ? ag.name : (state.status && state.status.chat_model) || "Private AI";
  $("call-you").textContent = "";
  $("call-them").textContent = "";
  $("call").classList.remove("hidden");
  call = { state: "", muted: false, chunks: [], preroll: [], floor: 0.01, echo: 0, loud: 0, quiet: 0, t: 0, abort: null, speaker: null, turn: 0 };
  callSay("connecting", "Connecting…");
  renderMic();
  const voice = agentVoice(ag);
  if (voice.startsWith("kokoro:")) {
    callSay("connecting", "Warming up the voice…");
    try { await kokoroLoad(); } catch {}
  }
  if (!call) return;
  try {
    call.mic = await openMic(onCallAudio);
  } catch (err) {
    endCall();
    addMessage("assistant", "⚠ Microphone not available: " + micError(err) + ".");
    return;
  }
  callSay("listening", "Listening…");
  call.anim = requestAnimationFrame(animateOrb);
}

function onCallAudio(d, rms) {
  const c = call;
  if (!c || c.muted || !c.mic) return;
  const ms = c.mic.seconds(1) * 1000;
  c.level = rms;
  const agentTalking = c.state === "speaking" || c.state === "thinking";
  // Background noise when nobody talks; the agent's own voice leaking
  // into the mic while it speaks (echo cancellation removes most of it).
  if (c.state === "listening" && rms < c.floor * 2) c.floor = Math.min(0.03, Math.max(0.002, c.floor * 0.95 + rms * 0.05));
  const thr = Math.max(0.012, c.floor * 3, agentTalking ? c.echo * 2.5 : 0);
  // Learn the echo level in the first moments of each reply (no
  // interrupting then), afterwards only from sounds quieter than speech,
  // so your own voice never counts as echo.
  if (c.state === "speaking") {
    c.spoken = (c.spoken || 0) + ms;
    if (c.spoken < 600) { c.echo = Math.max(c.echo * 0.8 + rms * 0.2, c.floor); return; }
    if (rms < thr) c.echo = c.echo * 0.95 + rms * 0.05;
  }
  if (c.state === "hearing") {
    c.chunks.push(d);
    c.quiet = rms > thr ? 0 : c.quiet + ms;
    if (c.quiet > CALL_PAUSE_MS || c.chunks.length * ms > 60000) endTurn();
    return;
  }
  c.preroll.push(d);
  if (c.preroll.length * ms > 400) c.preroll.shift();
  c.loud = rms > thr ? c.loud + ms : 0;
  const need = agentTalking ? CALL_BARGE_MS : 150;
  if ((c.state === "listening" || agentTalking) && c.loud >= need) {
    // Talking on before the agent has said anything continues your turn
    // (it was only a pause); once it speaks, talking interrupts it.
    const resume = c.state === "thinking" && !c.replied ? c.said : [];
    if (agentTalking) interruptAgent();
    c.chunks = resume.concat(c.preroll);
    c.preroll = [];
    c.quiet = 0;
    callSay("hearing", "Listening…");
  }
}

function interruptAgent() {
  if (call.speaker) call.speaker.stop();
  if (call.abort) call.abort.abort();
  call.speaker = null;
  call.abort = null;
  call.turn++;
}

async function endTurn() {
  const c = call;
  const chunks = c.chunks;
  c.chunks = [];
  c.said = chunks;
  c.replied = false;
  callSay("thinking", "Thinking…");
  const turn = ++c.turn;
  let text = "";
  try {
    text = await transcribeChunks(chunks, c.mic.rate);
  } catch (err) {
    if (call === c && c.turn === turn) { $("call-them").textContent = "⚠ " + err.message; callSay("listening", "Listening…"); }
    return;
  }
  if (call !== c || c.turn !== turn) return; // interrupted or hung up meanwhile
  if (!text) { callSay("listening", "Listening…"); return; }
  $("call-you").textContent = text;
  $("call-them").textContent = "";
  // Wait for a turn that is still finishing (it was just interrupted).
  for (let i = 0; state.busy && i < 100; i++) await new Promise((r) => setTimeout(r, 50));
  if (call !== c || c.turn !== turn) return;
  const ag = currentAgent();
  c.abort = new AbortController();
  c.speaker = makeSpeaker(agentVoice(ag), {
    onStart: () => { if (call === c && c.turn === turn) { c.echo = c.floor; c.spoken = 0; callSay("speaking", "Speaking… (talk to interrupt)"); } },
  });
  const speaker = c.speaker;
  await send(text, {
    speaker, signal: c.abort.signal,
    onText: (raw) => { if (call === c && c.turn === turn) { c.replied = true; $("call-them").textContent = speechText(raw).slice(-280); } },
  });
  if (call !== c || c.turn !== turn) return;
  if (!speaker) { callSay("listening", "Listening…"); return; }
  speaker.end(lastReply(), () => { if (call === c && c.turn === turn) callSay("listening", "Listening…"); });
}

const lastReply = () => { const m = [...document.querySelectorAll(".msg.assistant")].pop(); return (m && m._raw) || ""; };

function animateOrb() {
  if (!call) return;
  const lvl = call.state === "speaking" && call.speaker ? call.speaker.level() || 0.25
    : call.state === "hearing" ? Math.min(1, (call.level || 0) * 10) : 0;
  $("call-orb").style.setProperty("--level", lvl.toFixed(2));
  call.anim = requestAnimationFrame(animateOrb);
}

function endCall() {
  const c = call;
  if (!c) return;
  call = null;
  if (c.mic) c.mic.close();
  if (c.speaker) c.speaker.stop();
  if (c.abort) c.abort.abort();
  cancelAnimationFrame(c.anim);
  $("call").classList.add("hidden");
  renderMic();
}

$("call-btn").addEventListener("click", startCall);
$("call-end").addEventListener("click", endCall);
$("call-mute").addEventListener("click", () => {
  if (!call) return;
  call.muted = !call.muted;
  $("call-mute").textContent = call.muted ? "🎤 Unmute" : "🎤 Mute";
  $("call").classList.toggle("muted", call.muted);
  if (call.muted && call.state === "hearing") { call.chunks = []; callSay("listening", "Muted"); }
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && call) endCall(); });
