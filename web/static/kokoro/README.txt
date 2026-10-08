kokoro-js 1.2.1 (npm, dist/kokoro.web.js; Apache License 2.0), which bundles
Transformers.js 3.5.1 (Apache 2.0) and phonemizer (espeak-ng, GPL-3.0).
npm integrity sha512-oq0HZJWis3t8lERkMJh84WLU86dpYD0EuBPtqYnLlQzyFP1OkyBRDcweAqCfhNOpltyN9j/azp1H6uuC47gShw==

Patched so that nothing is fetched from the internet:
- remoteHost:"https://huggingface.co/"  ->  location.origin+"/tts/"
- the voices URL https://huggingface.co/onnx-community/Kokoro-82M-v1.0-ONNX/resolve/main/voices/${e}.bin
  -> ${location.origin}/tts/onnx-community/Kokoro-82M-v1.0-ONNX/resolve/main/voices/${e}.bin
- the exported env also sets ONNX Runtime's numThreads.
The model, voices and ONNX Runtime files are on the drive (models/voice/kokoro)
and served by the app under /tts/.
