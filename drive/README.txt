PRIVATE AI
Offline • Local • Secure
========================

Portable AI for Windows, macOS and Linux. No cloud. No subscription.
Your conversations and files remain on this drive.

START
-----
  Windows:  double-click  Start-Windows.bat
  macOS:    double-click  Start-macOS.command
            (first time: right-click → Open, then Open, if macOS warns
            about an unidentified developer)
  Linux:    run  sh start-linux.sh  in a terminal

Your browser opens at http://127.0.0.1:8740. On first use, choose a
passphrase: it encrypts everything you store on the drive. There is NO way
to recover your data if you forget it.

Keep the small console window open while you use Private AI. Before removing
the drive, click Settings → Shut down (or press Ctrl+C in the console),
then eject the drive normally.

PRIVACY
-------
  Internet ............ never required
  Account ............. never required
  Prompts / documents . never uploaded
  Chats, files, memory  stored only on this drive, encrypted (AES-256)

The assistant only listens on 127.0.0.1 (this computer); it is not reachable
from the network. Nothing is written to the computer's own disk, though your
browser may remember that you visited 127.0.0.1:8740. The exception is
Settings -> Faster loading (off by default). It copies only the public AI model
files to this computer to make loading faster, never your chats or files.

REQUIREMENTS
------------
  RAM:  8 GB recommended (4 GB works with the small model, 16 GB+ for the
        large model). The best model that fits is chosen automatically;
        change it in Settings.
  GPU:  optional. NVIDIA (CUDA), Vulkan-capable GPUs and Apple Silicon are
        used automatically when available; otherwise the CPU is used.
  Some locked-down work or school computers block programs on removable
  drives; Private AI cannot run on those.

WHAT'S ON THE DRIVE
-------------------
  bin/        Private AI app for each operating system
  runtime/    llama.cpp inference engines (CPU, CUDA, Vulkan, Metal)
  models/     AI models (GGUF): chat, embedding (search), specialist
  data/       your encrypted vault — back this folder up
  config.json settings (models, port, GPU layers)

Adding a model: copy a .gguf file into models/chat/ and add an entry for it
to config.json ("role": "chat"), then pick it in Settings.
