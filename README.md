# Private AI: a portable, offline AI drive

Plug in a USB drive, double-click **Start**, and a private assistant opens in
your browser. It runs on the host computer's CPU or GPU with
[llama.cpp](https://github.com/ggml-org/llama.cpp). It can read your PDFs and
documents, and it remembers what you ask it to. All of it stays on the drive,
encrypted. No internet, account, subscription or API key is needed.

> Portable AI for Windows, macOS, and Linux. No cloud. No subscription.
> Your conversations and files remain on your device.

![Chat with a document](docs/screenshot-chat.png)

## Drive layout

```
PRIVATE-AI/                         (exFAT, 64 GB+)
├── Start-Windows.bat               launchers: pick the right binary for the OS/CPU
├── Start-macOS.command
├── start-linux.sh
├── config.json                     models, port, GPU layers, system prompt
├── bin/<os>-<arch>/privateai       the app (one ~7 MB static Go binary per platform)
├── runtime/                        llama.cpp llama-server builds
│   ├── windows-x64-cuda | -vulkan | -cpu,  windows-arm64-cpu
│   ├── macos-arm64 (Metal) | macos-x64
│   └── linux-x64-vulkan | -cpu,  linux-arm64-cpu
├── models/
│   ├── chat/                       general assistant GGUF models (~1–5 GB each)
│   ├── embed/                      embedding model for document search (<200 MB)
│   └── specialist/                 optional extra models
└── data/
    ├── settings.json               non-secret preferences (chosen model)
    └── vault/                      encrypted chats, memory, documents, search index
```

Typical 64 GB budget: models 5–15 GB, runtimes about 1.5 GB for all platforms,
app under 50 MB, and the rest for your documents, index and free space.

## How it works

1. **Launcher**: `Start-*` runs `bin/<os>-<arch>/privateai`. The app finds the
   drive root by looking for `config.json`.
2. **Hardware detection**: OS, CPU architecture, RAM, and NVIDIA (CUDA),
   Vulkan or Apple Silicon (Metal) support.
3. **Runtime fallback**: it tries `runtime/<platform>-cuda`, then `-vulkan`,
   then `-cpu`. A runtime that fails to load the model is skipped, so CPU is
   always the universal fallback.
4. **Model choice**: it uses the first chat model in `config.json` that is on
   the drive and fits in RAM. You can switch models in Settings.
5. **Two local `llama-server` processes** run on `127.0.0.1` with random
   ports: one for chat and one for embeddings.
6. **Web UI** at `http://127.0.0.1:8740`, embedded in the binary.

### Privacy and security

| | |
|---|---|
| Network | Every server binds to `127.0.0.1` only, and API requests from non-loopback addresses are refused. Nothing calls out unless you switch on agent internet access, which is off by default and sends only search words and page addresses. |
| Other websites | Host-header check (blocks DNS rebinding), and every write request needs a custom header (blocks CSRF). Strict CSP. |
| Data at rest | `data/vault`: AES-256-GCM. The key comes from your passphrase (PBKDF2-SHA256, 600k iterations, random salt). Object names are HMAC'd, so file names reveal nothing. Each object is bound to its name, so swapping files is detected. |
| Locking | The key lives only in RAM. **Lock** or **Shut down** discards it. |
| Host computer | Nothing is written outside the drive and logs go only to the console window, unless you turn on **Faster loading** (off by default). That copies only public model files, named by checksum, to the computer's cache folder (`~/Library/Caches/PrivateAI` on macOS, at most 40 GB). It never copies chats, documents or memory, and **Remove cached models** deletes them. |
| Uploaded HTML/SVG | Served back as plain text, so it cannot run script in the app's origin. |

Forgetting the passphrase makes the vault unrecoverable. That is deliberate.

### Documents (local RAG)

- Text is extracted in-process from PDF, DOCX, TXT/MD, CSV/TSV, JSON, HTML, XML and RTF.
- **Scanned PDFs** (page pictures with no text layer, like archive or FOIA
  documents) are read with OCR by your vision model. The browser draws each
  page with the bundled [pdf.js](https://mozilla.github.io/pdf.js/) and the
  vision model (the chat model if it can see, otherwise the image reader)
  transcribes it. A progress bar shows the page and time left, and *Stop*
  cancels. The original PDF is kept and the text is indexed like any other
  document. Everything stays on the computer. Up to 300 pages per file.
  Speed depends on the model and computer: on a 4-core test machine with no
  GPU, Gemma 3 12B took about 1.5–2 minutes per page; a Mac with Apple
  silicon is several times faster.
- Text is split into chunks of about 1200 characters with overlap, then embedded locally.
- Search is hybrid: BM25 keyword ranking fused with embedding cosine
  similarity (reciprocal rank fusion). Without an embedding model it falls
  back to keyword search, and documents added meanwhile are embedded later.
- **Ask about a file**: drop it on the chat, or press *Ask* in Files. If the
  whole document fits in the context window, the model reads all of it. That
  matters for "summarize this contract". Otherwise it gets the most relevant
  excerpts. Sources are shown under each answer.

### Screenshots and images

Paste a screenshot into the chat (⌘V / Ctrl+V), drop an image on it, or use 📎.
The browser shrinks it to 1280 px before sending, which keeps text readable
and cuts processing time about 3–4×. Images are stored encrypted in the vault
with the chat.

Reading images needs a **vision model**: a chat model with a matching image
projector (`mmproj`) file. The default is Qwen3-VL 4B, with Qwen3-VL 2B for
low-memory computers. If the active model can't see images, the chat offers
to switch.

### Image reader (two models at once)

If the chat model can't see images, for example gpt-oss 20B or DeepSeek R1, a
second, smaller vision model can run alongside it as the **image reader**. It
describes each pasted image, transcribing all the text, and the chat model
answers from that description, so agents and tools keep using the stronger
model. Descriptions are saved with the chat, so follow-up questions about the
image keep working.

It turns on automatically on computers with 16 GB or more of memory, using the
smallest vision model on the drive. Change or turn it off under **Settings →
Image reader**.

### Voice: talk-to-text and spoken replies

- **🎤 Talk instead of typing.** The browser records the microphone, stops
  by itself when you pause, and sends a 16 kHz WAV to the app. The
  speech-recognition model on the drive,
  [Qwen3-ASR 0.6B](https://huggingface.co/Qwen/Qwen3-ASR-0.6B) (about 1 GB,
  30 languages plus 22 Chinese dialects), turns it into text. It runs as a third small llama.cpp
  engine. Recordings are never saved and never leave the computer.
  Browser dictation is not used, because it sends audio to Apple or Google.
- **🔊 Speak replies.** Replies are read aloud sentence by sentence as they
  are written. The default is **natural voices**:
  [Kokoro 82M](https://huggingface.co/hexgrad/Kokoro-82M) (Apache-2.0), with
  28 US and UK English voices, run in the browser by
  [kokoro-js](https://github.com/hexgrad/kokoro) and ONNX Runtime Web in a
  background worker. It runs on the graphics chip (WebGPU) when the browser
  has one, otherwise on the processor's cores. The model, voices and runtime
  are on the drive (`models/voice/kokoro`, about 455 MB) and served by the
  app, so nothing is downloaded while you use it. The computer's own
  on-device voices are offered as well and are used as a fallback. Each
  agent can have its own voice (Agents → Edit → Voice), and every reply has
  a 🔊 button. Code, links and Markdown symbols are skipped.
- **📞 Live voice call.** A call screen where you just talk. The mic stays
  open, a pause ends your turn, and the reply is spoken as it is written.
  Speak while the agent is talking (or still thinking) to interrupt it; this
  also stops the reply on the server. Pausing mid-sentence before the agent
  answers continues your turn instead of cutting it. Mute and End call
  buttons are on screen, and Esc hangs up. Echo cancellation keeps the agent
  from hearing itself; headphones work best.
- **Hands-free** (Settings → Voice, on by default): what you say with 🎤 is
  sent right away, and when 🔊 is on the mic opens again after the reply.
- Safari asks once for microphone permission. In testing on a 4-core
  processor without a graphics chip, Kokoro ran at about 0.4× real time, so
  sentences arrived with pauses. Apple-silicon Macs are much faster,
  especially with WebGPU. Settings → Voice shows how fast it runs on yours.
- Licenses: kokoro-js bundles a phonemizer built from espeak-ng (GPL-3.0).
  If you distribute drives, include its source or an offer for it.

### Appearance

Pick a theme with the toggle buttons under **Settings → Appearance**. The
choice is remembered in this browser:

- **Matrix** (default): green on black with terminal type, and a "Wake up…"
  greeting on the unlock screen.
- **Claude Code**: warm charcoal and cream with a terracotta accent, plus the ✻
  mark, `>` prompts and a "✻ Welcome" greeting.
- **Apple Terminal**: a macOS Terminal window with the red, yellow and green
  title-bar dots, SF Mono, macOS-blue buttons and a "Last login… on ttys000"
  greeting.
- **Classic**: the original light/dark look, which follows the system setting.

### Faster loading (model cache)

Load time is mostly the drive reading the model file, which can be 7–9 GB.
With **Settings → Faster loading** on, each model is copied to the computer's
internal disk after its first load, with the copy verified against the model's
SHA-256. Later loads read from that copy, which is typically several times
faster than a USB stick. The copy only starts once the model is loaded and
running, never while it is still loading. It keeps 10 GB of the computer's
disk free and removes the least recently used models when it reaches 40 GB.

### Agents

The **Agents** tab creates assistants with their own:

- **instructions**: their role, focus and answer format
- **tools**: search documents, read a document, list documents, calculator,
  date & time, remember a fact, create a note (saved to Files)
- **documents**: all files, selected files, or none
- **creativity**: the model's temperature setting

Pick an agent from the menu under the chat box, or start from a template:
Research Assistant, Contract Reviewer, Note Taker, Math Tutor, Writing Coach.
Agents use llama.cpp's tool calling. The model may call tools for up to 6
rounds before it must answer, and each answer shows the tools it used.

Tools run in-process. Apart from the optional web tools below, they can't
reach the internet. None of them can run programs or touch the host
computer's files. The most an agent can change is adding a note or a memory to
your own vault. Agents and their chats are encrypted like everything else.

### Internet access for agents (optional)

**Agents → Internet access** has two switches, both off by default:

- **DuckDuckGo:** free and needs no key. It reads DuckDuckGo's HTML results
  page, so it can break if their page changes, and it may ask for a human
  check after many searches.
- **Brave Search:** the official [Brave Search API](https://brave.com/search/api/),
  which needs a free API key. The key is stored encrypted in the vault and is
  never sent back to the browser.

Searches try DuckDuckGo first, then Brave **automatically** if DuckDuckGo fails.
**Test connection** shows which one answered. An agent only goes online if
internet access is on *and* the agent has the 🌐 *Search the web* or *Read web
pages* tools. Those agents show a 🌐 badge, and the **Web Researcher** template
sets one up.

Small models often answer "I can't access current information" instead of
searching. An agent's **🌐 Always search the web before answering** option
fixes that: the first step of every answer is then a forced web search (via
llama.cpp's `tool_choice: "required"`). The Web Researcher template has it on.
The plain *Private AI* chat never goes online; it suggests a web agent instead.

Safety:
- **What is sent:** only the search words or page address. Chats, files and
  memory are never sent.
- **How pages are fetched:** read-only, with no cookies, logins, forms or
  downloads, at most 3 MB per page.
- **Blocked addresses:** this computer, the local network, cloud metadata
  addresses and `.local` names are refused on every connection, including
  redirects.
- **No proxy:** system proxy settings are ignored, so that address check always
  sees the real site.
- **Untrusted content:** web text is labelled as untrusted, and agents are told
  never to follow instructions found in it. A small local model can still be
  misled by a hostile page, so keep internet access off when you don't need
  it.

## Building a drive

You need Go 1.24 or newer.

```sh
make test              # unit tests + end-to-end test against a fake llama-server
make drive             # cross-compile all 6 targets into dist/PRIVATE-AI
make drive-full        # also download llama.cpp runtimes + models from config.json (several GB)
```

Or step by step:

```sh
scripts/build-drive.sh
go run ./cmd/drivetool fetch-runtime -drive dist/PRIVATE-AI            # optionally -tag b6500 -only linux-x64-cpu
go run ./cmd/drivetool fetch-models  -drive dist/PRIVATE-AI -only qwen3-4b,nomic-embed
go run ./cmd/drivetool check         -drive dist/PRIVATE-AI
go run ./cmd/drivetool verify        -drive /Volumes/USBAI   # after copying: checks every model's size + SHA-256
```

Then format the USB drive as **exFAT**, which Windows, macOS and Linux can all
read and write. Copy the contents of `dist/PRIVATE-AI` to its root.

`drivetool fetch-runtime` matches llama.cpp GitHub release assets by name and
keeps only `llama-server` and its shared libraries. It also dereferences
symlinks, because exFAT cannot store them. If the GitHub API is blocked or
rate-limited, it lists `bNNNN` tags with `git ls-remote` and probes
the known asset names directly. All runtimes come to about 1.7 GB, about 1.2 GB
of which is the Windows CUDA build. If upstream renames its assets, update
`runtimeAssets` and `probeAssets` in `cmd/drivetool/main.go`.

By default it installs the newest numbered `bNNNN` build whose release has
every runtime. GitHub's "latest" llama.cpp release can be a `vX.Y.Z`
release with no prebuilt binaries, and the newest build may still be
uploading its files. Pass `-tag bNNNN` to pin a specific build.

Last verified against llama.cpp b11332: all 9 runtimes downloaded. A real
end-to-end run on Linux x64 (Qwen3 1.7B + nomic-embed) answered questions
about an uploaded contract with correct citations.

### Default models

Licenses: Qwen, gpt-oss and DeepSeek are Apache-2.0 / MIT. Gemma uses Google's Gemma
terms. Full DeepSeek R1/V3 (671B parameters, about 400 GB) is too large for a
laptop; the R1 distill below is the version that runs locally.

| id | model | size | RAM |
|---|---|---|---|
| `gpt-oss-20b` | OpenAI gpt-oss 20B MXFP4 (Apache-2.0): mixture of experts, 21B parameters with about 3.6B active per word, so it reasons well and runs fast; thinks before answering; the default at 16 GB+ | ~12.1 GB | 16 GB+ |
| `qwen3-8b` | Qwen3 8B Q4_K_M | ~5 GB | 16 GB+ |
| `qwen3-vl-4b` | Qwen3-VL 4B Instruct Q4_K_M + mmproj Q8_0, **sees images** | ~2.9 GB | 8 GB+ |
| `gang` | **GANG**: Qwen3-VL 4B with a hip-hop personality (no extra download) | — | 8 GB+ |
| `qwen3-4b` | Qwen3 4B Instruct 2507 Q4_K_M | ~2.5 GB | 8 GB+ |
| `qwen3-vl-2b` | Qwen3-VL 2B Instruct Q4_K_M + mmproj Q8_0, **sees images** | ~1.5 GB | 4 GB+ |
| `gemma3-12b` | Gemma 3 12B Q4_K_M + mmproj F16, **sees images**, sharpest image reading | ~8.2 GB | 16 GB+ |
| `deepseek-r1-8b` | DeepSeek R1 0528 (Qwen3 8B distill), reasons step by step; slow, and in testing it reasoned its way to a wrong answer on simple arithmetic, so treat it as optional | ~5.0 GB | 12 GB+ |
| `qwen3-1.7b` | Qwen3 1.7B Q4_K_M | ~1.1 GB | 4 GB+ |
| `nomic-embed` | nomic-embed-text v1.5 Q8_0 | ~140 MB | — |
| `qwen3-asr-0.6b` | Qwen3-ASR 0.6B Q8_0 + audio encoder, speech to text (Apache-2.0) | ~1.0 GB | 4 GB+ |
| `kokoro` | Kokoro 82M (q8 for the processor, fp32 for WebGPU), 28 English voices, ONNX Runtime Web: natural voices (Apache-2.0) | ~455 MB | — |

To use any other GGUF model, add an entry to `config.json`. For a vision
model, also set `mmproj` and `mmproj_url`. When `build-drive.sh` runs on an
existing drive, it calls `drivetool sync-config`, which adds models introduced
by updates and keeps your other settings.

To **update a drive you already use**, shut Private AI down and run
`scripts/update-drive.sh "/Volumes/GANG AI"` (with your drive's path). It
rebuilds the programs, copies them and the launchers to the drive, and adds
new model entries to the drive's `config.json`. Models and your data are
untouched. Models an update retires (listed under `retired_models` in
`drive/config.json`, such as Qwen3 14B, replaced by gpt-oss 20B) are removed
from the drive's `config.json` and their files deleted, unless another model
still uses them. If an update adds a new model, such as the speech model, download
it with `go run ./cmd/drivetool fetch-models -drive "/Volumes/GANG AI"`
(already-present models are skipped).

### Personalities (GANG)

A model entry can be a *personality* of another model: it sets `base` to that
model's id and adds a `persona`, which is appended to the system prompt. It
uses the base model's files, so it costs no disk space. **GANG** is built
this way on Qwen3-VL 4B: an urban, hip-hop voice that stays articulate,
detailed and accurate, answers adult questions frankly without lecturing,
and only declines requests that would seriously help someone hurt people.
Choose it in Settings → Model. To change GANG's voice, edit its `persona` in
`config.json` on the drive (and restart), or copy the entry with a new `id`
to make another personality.

### Damaged or incomplete model files

`config.json` records each model's exact size and SHA-256, taken from
Hugging Face. At startup, Private AI skips any model file whose size is wrong,
which is usually a copy to the drive that was cut short. If a model fails to
load, it falls back to the next model that works and shows why in a yellow
notice. To check every model on a drive, run `drivetool verify`, which can
take a few minutes over USB.

## Known limits

- **Not every computer works.** Locked-down corporate or school machines often
  block programs on removable media. Computers with very little RAM cannot run
  the models.
- **macOS Gatekeeper**: the binaries are unsigned. The launcher clears the
  quarantine flag, but the first launch may still need right-click → Open.
  Signing and notarizing is a to-do for a product release.
- **Linux `noexec` mounts**: some desktops mount USB drives without exec
  permission. The launcher detects this and tells you how to remount.
- **OCR is done by a language model**, not a classic OCR engine. It reads
  clean typed pages very well, but it can misread poor scans or handwriting,
  and occasionally add or drop a line. Check important facts against the
  original (*Open* in Files).
- USB 2.0 drives make model loading slow. USB 3 is strongly recommended.
- If the console window is closed instead of using *Shut down*, the
  `llama-server` child processes may keep running on Windows until logout.

## Code map

| path | what |
|---|---|
| `cmd/privateai` | launcher/server entry point |
| `cmd/drivetool` | builds drives: fetches runtimes and models, checks the layout |
| `internal/platform` | OS/arch/RAM/GPU detection, runtime candidates |
| `internal/llama` | starts `llama-server`, streams chat, gets embeddings |
| `internal/vault` | encrypted object store |
| `internal/rag` | text extraction, chunking, hybrid index |
| `internal/app` | app state, chat orchestration, agents and tools, images, HTTP API |
| `web/static` | the UI (vanilla HTML/CSS/JS, embedded) |
| `launchers/`, `drive/` | files copied to the drive root |
