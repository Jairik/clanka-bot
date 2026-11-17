// Copies mic frames off the audio thread. The main thread resamples to 16 kHz
// when the recording stops; doing it here would need the whole utterance.
export class Recorder extends AudioWorkletProcessor {
  /** Pre-allocates the first 1024-sample batch buffer. */
  constructor() {
    super();
    this.buf = new Float32Array(1024);
    this.pos = 0;
  }

  /** Accumulates audio quanta into 1024-sample blocks and posts them to the main thread. */
  process(inputs) {
    const ch = inputs[0] && inputs[0][0];
    if (ch && ch.length) {
      let offset = 0;
      while (offset < ch.length) {
        const space = 1024 - this.pos;
        const take = Math.min(space, ch.length - offset);
        this.buf.set(ch.subarray(offset, offset + take), this.pos);
        this.pos += take;
        offset += take;
        // Post the filled 1024-sample block and start a fresh one for the next batch.
        if (this.pos >= 1024) {
          this.port.postMessage(this.buf);
          this.buf = new Float32Array(1024);
          this.pos = 0;
        }
      }
      return true;
    }
    // Flush the partial block when the node is disconnected so the tail is kept.
    if (this.pos > 0) {
      this.port.postMessage(this.buf.slice(0, this.pos));
      this.buf = new Float32Array(1024);
      this.pos = 0;
      return false;
    }
    return true;
  }
}
if (typeof registerProcessor !== "undefined") {
  registerProcessor("clanka-recorder", Recorder);
}
