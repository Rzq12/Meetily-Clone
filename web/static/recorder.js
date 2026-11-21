'use strict';

// Browser recording for FR-004: MediaRecorder with a 5 s timeslice, chunks sent strictly
// in seq order. Failed chunks stay queued in memory and the oldest is retried every 5 s.
class Recorder {
  constructor({ bars, clock, onWarning, onFatal }) {
    this.bars = bars;
    this.clock = clock;
    this.onWarning = onWarning;
    this.onFatal = onFatal;
    this.onLimit = null;
    this.stream = null;
    this.queue = [];
    this.sending = false;
    this.seq = 0;
    this.fatal = null;
    this.maxMs = 4 * 60 * 60 * 1000;
  }

  // preview asks for the microphone and drives the level meter; returns an error message or null.
  async preview() {
    if (this.stream) return null;
    if (!navigator.mediaDevices || !window.MediaRecorder) return 'Browser tidak mendukung perekaman audio.';
    try {
      this.stream = await navigator.mediaDevices.getUserMedia({ audio: true });
    } catch (err) {
      if (err.name === 'NotAllowedError' || err.name === 'SecurityError') return 'Izin mikrofon ditolak. Aktifkan izin mikrofon di browser.';
      if (err.name === 'NotFoundError' || err.name === 'OverconstrainedError') return 'Mikrofon tidak ditemukan.';
      return 'Mikrofon tidak bisa dibuka.';
    }
    const ctx = new AudioContext();
    this.audioCtx = ctx;
    const analyser = ctx.createAnalyser();
    analyser.fftSize = 512;
    ctx.createMediaStreamSource(this.stream).connect(analyser);
    const data = new Uint8Array(analyser.fftSize);
    const scale = [0.8, 1, 0.9];
    const tick = () => {
      if (!this.stream) return;
      analyser.getByteTimeDomainData(data);
      let sum = 0;
      for (const v of data) sum += ((v - 128) / 128) ** 2;
      const rms = Math.sqrt(sum / data.length);
      this.bars.forEach((bar, i) => { bar.style.height = Math.max(4, Math.min(30, 4 + rms * 160 * scale[i])) + 'px'; });
      requestAnimationFrame(tick);
    };
    tick();
    return null;
  }

  pickMime() {
    if (MediaRecorder.isTypeSupported('audio/webm;codecs=opus')) return { recorder: 'audio/webm;codecs=opus', server: 'audio/webm' };
    if (MediaRecorder.isTypeSupported('audio/mp4')) return { recorder: 'audio/mp4', server: 'audio/mp4' };
    return null;
  }

  async start(fields) {
    const err = await this.preview();
    if (err) return err;
    const mime = this.pickMime();
    if (!mime) return 'Browser tidak mendukung perekaman audio.';

    const res = await App.api('POST', '/api/recordings', { ...fields, consent_confirmed: true, mime_type: mime.server });
    if (!res.ok) return res.data.error || 'Rekaman tidak bisa dimulai.';
    this.id = res.data.id;

    this.media = new MediaRecorder(this.stream, { mimeType: mime.recorder, audioBitsPerSecond: 64000 });
    this.media.addEventListener('dataavailable', (e) => {
      if (e.data && e.data.size > 0) {
        this.queue.push({ seq: ++this.seq, blob: e.data });
        this.pump();
      }
    });
    this.media.start(5000);
    this.startedAt = Date.now();
    this.timer = setInterval(() => {
      const elapsed = Date.now() - this.startedAt;
      this.clock.textContent = App.clock(elapsed / 1000);
      if (elapsed >= this.maxMs && this.onLimit) {
        clearInterval(this.timer);
        this.onLimit();
      }
    }, 500);
    await this.lockScreen();
    document.addEventListener('visibilitychange', () => {
      if (document.visibilityState === 'visible' && this.media && this.media.state === 'recording') this.lockScreen();
    });
    return null;
  }

  async lockScreen() {
    try {
      if ('wakeLock' in navigator) this.wakeLock = await navigator.wakeLock.request('screen');
    } catch (e) {
      // The wake lock is best effort; recording continues without it.
    }
  }

  async pump() {
    if (this.sending || this.queue.length === 0 || this.fatal) return;
    this.sending = true;
    const item = this.queue[0];
    const res = await App.api('POST', `/api/recordings/${this.id}/chunks?seq=${item.seq}`, item.blob);
    if (res.ok) {
      this.queue.shift();
      if (this.retrying) { this.retrying = false; this.onWarning(null); }
      this.sending = false;
      this.pump();
      return;
    }
    if (res.status === 409 && res.data.expected_seq) {
      // The server already has everything below expected_seq, e.g. after a lost response.
      this.queue = this.queue.filter((c) => c.seq >= res.data.expected_seq);
      if (this.queue.length && this.queue[0].seq !== res.data.expected_seq) {
        this.fail('Potongan rekaman hilang. Hentikan dan proses rekaman yang tersimpan.');
        return;
      }
      this.sending = false;
      this.pump();
      return;
    }
    if (res.status === 404 || res.status === 409) {
      this.fail(res.data.error || 'Rapat sudah tidak tersedia.');
      return;
    }
    if (!this.retrying) { this.retrying = true; this.onWarning('Koneksi terputus, mencoba lagi.'); }
    setTimeout(() => { this.sending = false; this.pump(); }, 5000);
  }

  fail(msg) {
    this.fatal = msg;
    this.sending = false;
    this.onFatal(msg);
  }

  // stop flushes the last chunk, waits for the queue to drain, then calls /finish.
  async stop() {
    clearInterval(this.timer);
    if (this.media && this.media.state !== 'inactive') {
      await new Promise((resolve) => {
        this.media.addEventListener('stop', resolve, { once: true });
        this.media.stop();
      });
    }
    while ((this.queue.length || this.sending) && !this.fatal) {
      await new Promise((r) => setTimeout(r, 200));
    }
    this.release();
    const res = await App.api('POST', `/api/recordings/${this.id}/finish`);
    if (!res.ok) return { error: res.data.error || 'Rekaman tidak bisa diselesaikan.' };
    return { id: this.id };
  }

  release() {
    if (this.stream) this.stream.getTracks().forEach((t) => t.stop());
    this.stream = null;
    if (this.audioCtx) this.audioCtx.close();
    if (this.wakeLock) this.wakeLock.release();
    this.bars.forEach((bar) => { bar.style.height = '4px'; });
  }
}
