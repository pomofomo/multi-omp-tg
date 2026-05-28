# Improvements

- Support longer voice messages. As per this log: May 28 07:22:42 omp-dev trd[38316]: /workspace/sherpa-onnx/csrc/offline-recognizer-whisper-impl.h:DecodeStream:97 Only waves less than 30 seconds are supported. We process only the first 30 seconds and discard the remaining data
- Better text to speech models, at least flag. There is a high quality version of the same speech model to try first, otherwise we can try using kokoro in onnx (not the python version which is super slow)

