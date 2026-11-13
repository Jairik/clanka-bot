// Copies mic frames off the audio thread. The main thread resamples to 16 kHz
// when the recording stops; doing it here would need the whole utterance.
class Recorder extends AudioWorkletProcessor {
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (ch && ch.length) this.port.postMessage(ch.slice(0));
    return true;
  }
}
registerProcessor("clanka-recorder", Recorder);
