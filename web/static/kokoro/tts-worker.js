// Runs the Kokoro voice model off the page's main thread, so the page (and
// a live call's microphone) stays responsive while speech is generated.
import { KokoroTTS, env } from "./kokoro.web.js";

const MODEL = "onnx-community/Kokoro-82M-v1.0-ONNX";
env.wasmPaths = self.location.origin + "/tts/ort/";
let tts = null;
let device = "";

async function load(engine) {
  if (tts) return;
  // The graphics chip (WebGPU) is much faster when there is one.
  if (engine !== "cpu" && self.navigator.gpu) {
    try {
      if (await self.navigator.gpu.requestAdapter()) {
        tts = await KokoroTTS.from_pretrained(MODEL, { dtype: "fp32", device: "webgpu" });
        device = "webgpu";
        return;
      }
    } catch (e) {
      tts = null;
    }
  }
  env.numThreads = Math.max(1, Math.min(8, (self.navigator.hardwareConcurrency || 4) - 1));
  tts = await KokoroTTS.from_pretrained(MODEL, { dtype: "q8", device: "wasm" });
  device = "cpu";
}

self.onmessage = async ({ data }) => {
  const { id, type } = data;
  try {
    if (type === "load") {
      await load(data.engine);
      const t = performance.now();
      const a = await tts.generate("Hello there.", { voice: "af_heart" });
      const speed = (a.audio.length / a.sampling_rate) / ((performance.now() - t) / 1000);
      self.postMessage({ id, ok: true, device, speed });
    } else if (type === "speak") {
      const a = await tts.generate(data.text, { voice: data.voice, speed: data.speed || 1 });
      const pcm = a.audio;
      self.postMessage({ id, ok: true, audio: pcm, rate: a.sampling_rate }, [pcm.buffer]);
    }
  } catch (e) {
    self.postMessage({ id, ok: false, error: String((e && e.message) || e) });
  }
};
