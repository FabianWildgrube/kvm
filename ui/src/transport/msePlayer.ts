/**
 * Plays the device's fMP4 stream through Media Source Extensions.
 *
 * MSE is used rather than WebCodecs because the local UI is served over
 * plain HTTP by default, and WebCodecs is gated on a secure context — on
 * `http://<device>/` the VideoDecoder constructor simply does not exist.
 * MediaSource has no such restriction.
 *
 * MSE was designed for seekable on-demand playback, so keeping it live takes
 * three deliberate measures:
 *
 *   - append one fragment per access unit as it arrives, never batching;
 *   - evict played data ourselves, because the browser's own eviction can
 *     drop the anchor keyframe and empty the buffer entirely;
 *   - steer the playback position, because a decoder that falls behind will
 *     happily stay behind forever.
 */

const logPrefix = "[mse]";

/** Drop buffered data older than this many seconds behind the playhead. */
const BUFFER_RETENTION_SECONDS = 2;

/** Run the eviction/drift check on this cadence. */
const MAINTENANCE_INTERVAL_MS = 1000;

/**
 * Drift above this many seconds is corrected by speeding playback up
 * slightly. Below it the stream is considered live and runs at 1.0.
 */
const DRIFT_SOFT_SECONDS = 0.25;

/**
 * Drift above this is corrected by seeking straight to the live edge.
 * Catching up at 1.05x would take far too long.
 */
const DRIFT_HARD_SECONDS = 1.5;

/** Playback rate used while catching up a soft drift. */
const CATCHUP_RATE = 1.05;

type PlayerState = "idle" | "awaiting-init" | "running" | "failed";

export interface MsePlayerCallbacks {
  /** Called when the player gives up; the caller should rebuild it. */
  onFatal?: (reason: string) => void;
}

export class MsePlayer {
  private video: HTMLVideoElement;
  private callbacks: MsePlayerCallbacks;

  private mediaSource: MediaSource | null = null;
  private sourceBuffer: SourceBuffer | null = null;
  private objectUrl: string | null = null;

  private codec: string | null = null;
  private state: PlayerState = "idle";

  /**
   * Appends must be serialised: calling appendBuffer while the SourceBuffer
   * is updating throws InvalidStateError.
   */
  private queue: BufferSource[] = [];
  private maintenanceTimer: ReturnType<typeof setInterval> | null = null;

  constructor(video: HTMLVideoElement, callbacks: MsePlayerCallbacks = {}) {
    this.video = video;
    this.callbacks = callbacks;
  }

  static isSupported(): boolean {
    return typeof MediaSource !== "undefined";
  }

  /** True once a fragment has been appended and playback can begin. */
  get isRunning(): boolean {
    return this.state === "running";
  }

  /**
   * Prepares for an init segment with the given codec. Called when the
   * device announces a new video format, including the first one and any
   * mid-stream resolution change.
   */
  prepare(codec: string): void {
    if (this.mediaSource && this.codec === codec && this.state !== "failed") {
      // Same format as before: a redundant announcement, e.g. after a
      // transient device-side restart. Keep the existing buffer.
      return;
    }

    this.teardown();

    const mimeType = `video/mp4; codecs="${codec}"`;
    if (!MediaSource.isTypeSupported(mimeType)) {
      this.fail(`browser cannot play ${mimeType}`);
      return;
    }

    this.codec = codec;
    this.state = "awaiting-init";

    const mediaSource = new MediaSource();
    this.mediaSource = mediaSource;
    this.objectUrl = URL.createObjectURL(mediaSource);
    this.video.src = this.objectUrl;

    mediaSource.addEventListener(
      "sourceopen",
      () => {
        // The object URL is only needed to open the MediaSource.
        if (this.objectUrl) {
          URL.revokeObjectURL(this.objectUrl);
          this.objectUrl = null;
        }
        if (this.mediaSource !== mediaSource) return;

        try {
          const sourceBuffer = mediaSource.addSourceBuffer(mimeType);
          // "segments" tells MSE to place media by its own timestamps, which
          // is what the tfdt in each fragment provides.
          sourceBuffer.mode = "segments";
          sourceBuffer.addEventListener("updateend", () => this.flushQueue());
          sourceBuffer.addEventListener("error", () =>
            this.fail("source buffer reported an error"),
          );
          this.sourceBuffer = sourceBuffer;
          this.flushQueue();
        } catch (e) {
          this.fail(`failed to add source buffer: ${String(e)}`);
        }
      },
      { once: true },
    );
  }

  /** Appends the ftyp+moov init segment. */
  appendInit(data: BufferSource): void {
    if (this.state !== "awaiting-init" && this.state !== "running") return;
    this.enqueue(data);
  }

  /** Appends one moof+mdat fragment. */
  appendSegment(data: BufferSource): void {
    if (this.state === "idle" || this.state === "failed") return;

    this.enqueue(data);

    if (this.state === "awaiting-init") {
      this.state = "running";
      this.startMaintenance();
      // Autoplay may be refused; the caller surfaces that to the user via
      // the existing "click to play" overlay, so a rejection is not fatal.
      void this.video.play().catch(() => undefined);
    }
  }

  private enqueue(data: BufferSource): void {
    this.queue.push(data);
    this.flushQueue();
  }

  private flushQueue(): void {
    const sourceBuffer = this.sourceBuffer;
    if (!sourceBuffer || sourceBuffer.updating || this.queue.length === 0) return;
    if (this.mediaSource?.readyState !== "open") return;

    const next = this.queue.shift();
    if (!next) return;

    try {
      sourceBuffer.appendBuffer(next);
    } catch (e) {
      if (e instanceof DOMException && e.name === "QuotaExceededError") {
        // The buffer is full. Drop what we were holding and evict hard;
        // the next keyframe restarts cleanly.
        this.queue.length = 0;
        this.evict(true);
        return;
      }
      this.fail(`appendBuffer failed: ${String(e)}`);
    }
  }

  private startMaintenance(): void {
    if (this.maintenanceTimer !== null) return;
    this.maintenanceTimer = setInterval(() => {
      this.evict(false);
      this.correctDrift();
    }, MAINTENANCE_INTERVAL_MS);
  }

  /**
   * Removes played data. Left to itself the browser evicts from the front
   * when it hits its buffer cap, which takes the keyframe every later frame
   * depends on and empties the buffer; doing it ourselves keeps a bounded,
   * self-consistent window.
   */
  private evict(aggressive: boolean): void {
    const sourceBuffer = this.sourceBuffer;
    if (!sourceBuffer || sourceBuffer.updating) return;
    if (this.mediaSource?.readyState !== "open") return;
    if (sourceBuffer.buffered.length === 0) return;

    const start = sourceBuffer.buffered.start(0);
    const retention = aggressive ? 0 : BUFFER_RETENTION_SECONDS;
    const cutoff = this.video.currentTime - retention;
    if (cutoff <= start) return;

    try {
      sourceBuffer.remove(start, cutoff);
    } catch {
      // A remove that races an append is harmless; the next tick retries.
    }
  }

  /**
   * Keeps the playhead near the live edge. Without this, any stall leaves
   * playback permanently behind by however long the stall lasted.
   */
  private correctDrift(): void {
    const sourceBuffer = this.sourceBuffer;
    if (!sourceBuffer || sourceBuffer.buffered.length === 0) return;
    if (this.video.readyState < HTMLMediaElement.HAVE_CURRENT_DATA) return;

    const liveEdge = sourceBuffer.buffered.end(sourceBuffer.buffered.length - 1);
    const drift = liveEdge - this.video.currentTime;

    if (drift > DRIFT_HARD_SECONDS) {
      console.debug(`${logPrefix} drift ${drift.toFixed(2)}s, seeking to live edge`);
      this.video.currentTime = liveEdge;
      this.video.playbackRate = 1;
      return;
    }

    if (drift > DRIFT_SOFT_SECONDS) {
      if (this.video.playbackRate !== CATCHUP_RATE) this.video.playbackRate = CATCHUP_RATE;
      return;
    }

    if (this.video.playbackRate !== 1) this.video.playbackRate = 1;
  }

  private fail(reason: string): void {
    if (this.state === "failed") return;
    console.error(`${logPrefix} ${reason}`);
    this.state = "failed";
    this.callbacks.onFatal?.(reason);
  }

  /** Releases the MediaSource without disturbing the caller's video element. */
  private teardown(): void {
    if (this.maintenanceTimer !== null) {
      clearInterval(this.maintenanceTimer);
      this.maintenanceTimer = null;
    }
    this.queue.length = 0;

    if (this.sourceBuffer && this.mediaSource?.readyState === "open") {
      try {
        this.mediaSource.removeSourceBuffer(this.sourceBuffer);
      } catch {
        // Already detached.
      }
    }
    this.sourceBuffer = null;

    if (this.mediaSource?.readyState === "open") {
      try {
        this.mediaSource.endOfStream();
      } catch {
        // Already ended.
      }
    }
    this.mediaSource = null;

    if (this.objectUrl) {
      URL.revokeObjectURL(this.objectUrl);
      this.objectUrl = null;
    }

    this.video.playbackRate = 1;
    this.codec = null;
    this.state = "idle";
  }

  /** Tears everything down and detaches from the video element. */
  destroy(): void {
    this.teardown();
    this.state = "idle";
    try {
      this.video.removeAttribute("src");
      this.video.load();
    } catch {
      // The element may already be gone from the document.
    }
  }
}
