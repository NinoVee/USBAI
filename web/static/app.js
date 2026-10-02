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
function markdown(src) {
  const blocks = [];
  let s = escapeHTML(src).replace(/```[\w-]*\n?([\s\S]*?)(```|$)/g, (_, code) => {
    blocks.push("<pre><code>" + code.replace(/\n$/, "") + "</code></pre>");
    return "\u0000" + (blocks.length - 1) + "\u0000";
  });
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
};

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
      pill.textContent = "Loading " + (st.chat_model || "model") + "…";
      pill.classList.add("starting");
    } else {
      pill.textContent = st.chat_state === "no_model" ? "No model on drive" : "Model failed to start";
      pill.classList.add("error");
    }
    pill.title = st.chat_error || "";
    updateComposer();
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
  const ready = modelReady();
  $("send").disabled = state.busy || !ready;
  $("input").placeholder = ready ? "Ask anything…"
    : state.status && state.status.chat_state === "starting"
      ? "Loading the AI model from the drive… this can take a minute or two the first time"
      : "The AI model is not running — see the message above";
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
  $("messages").replaceChildren($("chat-empty") || emptyState());
  $("chat-empty").classList.remove("hidden");
}
function emptyState() {
  return el("div", { class: "empty", id: "chat-empty" },
    el("h2", {}, "What can I help you with?"),
    el("p", { class: "muted" }, "Drop a PDF, Word or text file anywhere to ask about it. Everything stays on this drive."));
}
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

function addMessage(role, content, sources) {
  const empty = $("chat-empty");
  if (empty) empty.classList.add("hidden");
  const body = el("div", { class: "body" });
  if (role === "assistant") body.innerHTML = markdown(content); else body.textContent = content;
  const msg = el("div", { class: "msg " + role }, body);
  if (role === "assistant") {
    const src = sourcesEl(sources);
    if (src) msg.append(src);
    msg.append(el("div", { class: "msg-tools" },
      el("button", { type: "button", onclick: () => navigator.clipboard && navigator.clipboard.writeText(msg._raw || content) }, "Copy")));
  }
  msg._raw = content;
  $("messages").append(msg);
  $("messages").scrollTop = $("messages").scrollHeight;
  return msg;
}

async function send(text) {
  if (state.busy || !text.trim()) return;
  state.busy = true;
  $("send").disabled = true;
  addMessage("user", text);
  const msg = addMessage("assistant", "");
  const body = msg.querySelector(".body");
  body.classList.add("typing");
  let raw = "";
  try {
    const res = await fetch("/api/chat", {
      method: "POST",
      headers: { ...H, "Content-Type": "application/json" },
      body: JSON.stringify({
        chat_id: state.chatId,
        message: text,
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
        } else if (ev === "thinking") {
          if (!raw) body.textContent = "Thinking…";
        } else if (ev === "token") {
          raw += data.text;
          body.innerHTML = markdown(raw.replace(/<think>[\s\S]*?(<\/think>|$)/g, ""));
          msg._raw = raw;
          const m = $("messages");
          if (m.scrollHeight - m.scrollTop - m.clientHeight < 120) m.scrollTop = m.scrollHeight;
        } else if (ev === "error") {
          throw new Error(data.error);
        }
      }
    }
  } catch (err) {
    body.append(el("p", { class: "err" }, "⚠ " + err.message));
  } finally {
    body.classList.remove("typing");
    state.busy = false;
    updateComposer();
  }
}

$("chat-form").addEventListener("submit", (e) => {
  e.preventDefault();
  if (!modelReady() || state.busy) return; // keep the typed text until the model is ready
  const text = $("input").value;
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
  newChat();
  state.chatId = chat.id;
  for (const m of chat.messages || []) addMessage(m.role, m.content, m.sources);
  show("chat");
}

// ---- Files ----

async function upload(files, focusAfter) {
  if (!files.length) return;
  const status = $("upload-status");
  const fd = new FormData();
  for (const f of files) fd.append("file", f, f.name);
  status.textContent = `Reading and indexing ${files.length} file(s) locally…`;
  try {
    const results = await api("POST", "/api/docs", fd);
    const errors = results.filter((r) => r.error);
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
        el("div", { class: "sub" }, `${fmtSize(d.size)} · ${d.pieces} sections · ${d.embedded ? "semantic + keyword search" : "keyword search"} · ${fmtDate(d.added)}`)),
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
  const toChat = state.view === "chat";
  upload(files, toChat);
  if (toChat) addMessage("assistant", `Adding ${files.map((f) => f.name).join(", ")}… Ask your question when the file chip appears below.`);
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
      el("div", { class: "grow" }, c.title, el("div", { class: "sub" }, fmtDate(c.updated))),
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
  const models = $("model-list");
  models.replaceChildren();
  const ram = Math.floor(st.host.ram_bytes / 2 ** 30);
  for (const m of st.models || []) {
    const tooBig = ram && m.min_ram_gb > ram;
    const row = el("div", { class: "row" + (m.active ? " active" : "") },
      el("div", { class: "grow" }, m.name,
        el("div", { class: "sub" }, !m.present ? "Not on this drive" :
          `Needs ~${m.min_ram_gb} GB RAM` + (tooBig ? " — may be slow or fail on this computer" : "") +
          (m.active ? ` · ${st.chat_state === "ready" ? "active" : st.chat_state}` : ""))));
    if (m.present && !m.active) {
      row.append(el("button", { onclick: async () => { await api("POST", "/api/models/select", { id: m.id }); refreshStatus(); } }, "Use"));
    }
    models.append(row);
  }
  if (st.chat_error) models.append(el("div", { class: "row error" }, st.chat_error));

  const facts = [
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

$("engine-retry").addEventListener("click", async () => {
  $("engine-error").classList.add("hidden");
  try { await api("POST", "/api/models/retry"); } catch {}
  refreshStatus();
});

// ---- Boot ----

const poll = setInterval(refreshStatus, 3000);
(async () => {
  const st = await refreshStatus();
  if (!st) return;
  if (st.locked) showLock();
  else { $("tabs").classList.remove("hidden"); show("chat"); }
})();
