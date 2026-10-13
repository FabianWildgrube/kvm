import { MsePlayer } from "./msePlayer";
import { DataChannelLike, WS_CHANNEL, WsControlMessage } from "./types";

/**
 * The WebSocket fallback transport.
 *
 * It exists for networks where WebRTC cannot connect at all — typically a
 * client reached through an application-level tunnel, which has no IP route
 * to the device and so can never pair an ICE candidate. Signalling succeeds
 * over HTTP while media has nowhere to go.
 *
 * Everything rides the one connection the browser opens to the device:
 * video as fMP4 fragments, plus the RPC and HID channels. Each message is
 * binary, tagged with a one-byte channel id.
 */

const logPrefix = "[ws-transport]";

/** Reconnect backoff, capped so a long outage still retries promptly. */
const reconnectDelay = (attempt: number) => Math.min(500 * 2 ** attempt, 10_000);

/**
 * A stand-in for RTCDataChannel backed by the shared WebSocket. Built on
 * EventTarget so add/removeEventListener behave exactly as the hooks expect.
 */
class WsChannel implements DataChannelLike {
  readonly label: string;
  binaryType: BinaryType = "arraybuffer";

  private target = new EventTarget();
  private state: RTCDataChannelState = "connecting";
  private sender: (data: Uint8Array) => void;
  private onCloseRequested: () => void;

  constructor(label: string, sender: (data: Uint8Array) => void, onCloseRequested: () => void) {
    this.label = label;
    this.sender = sender;
    this.onCloseRequested = onCloseRequested;
  }

  get readyState(): RTCDataChannelState {
    return this.state;
  }

  send(data: string | ArrayBufferLike | ArrayBufferView | Blob): void {
    if (this.state !== "open") return;

    if (typeof data === "string") {
      this.sender(new TextEncoder().encode(data));
      return;
    }
    if (data instanceof Blob) {
      // The hooks never send Blobs; converting would make this method async.
      console.warn(`${logPrefix} Blob payloads are not supported`);
      return;
    }
    if (ArrayBuffer.isView(data)) {
      this.sender(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
      return;
    }
    this.sender(new Uint8Array(data));
  }

  /**
   * Closing one logical channel has no meaning on a shared socket, so this
   * drops the whole transport. Callers use close() to force a fresh session,
   * and a reconnect delivers exactly that.
   */
  close(): void {
    this.onCloseRequested();
  }

  addEventListener(
    type: string,
    listener: (ev: never) => void,
    options?: boolean | AddEventListenerOptions,
  ): void {
    this.target.addEventListener(type, listener as EventListener, options);
  }

  removeEventListener(
    type: string,
    listener: (ev: never) => void,
    options?: boolean | EventListenerOptions,
  ): void {
    this.target.removeEventListener(type, listener as EventListener, options);
  }

  dispatchEvent(event: Event): boolean {
    return this.target.dispatchEvent(event);
  }

  /** @internal */
  setOpen(): void {
    if (this.state === "open") return;
    this.state = "open";
    this.target.dispatchEvent(new Event("open"));
  }

  /** @internal */
  setClosed(): void {
    if (this.state === "closed") return;
    this.state = "closed";
    this.target.dispatchEvent(new Event("close"));
  }

  /**
   * Delivers an inbound payload. Text channels (RPC) receive a string so
   * `JSON.parse(e.data)` works; binary channels receive an ArrayBuffer so
   * `new Uint8Array(e.data)` works. This mirrors how the WebRTC channels are
   * configured on the sending side.
   */
  /** @internal */
  deliver(payload: Uint8Array, asText: boolean): void {
    // slice() copies: the socket's view of the frame is reused for the next
    // message, so the ArrayBuffer handed to a listener must be its own.
    const data = asText ? new TextDecoder().decode(payload) : payload.slice().buffer;
    this.target.dispatchEvent(new MessageEvent("message", { data }));
  }
}

export interface WsStreamCallbacks {
  onStateChange?: (state: RTCPeerConnectionState) => void;
  onChannelsReady?: (channels: { rpc: DataChannelLike; hid: DataChannelLike }) => void;
  onVideoDimensions?: (width: number, height: number) => void;
  onFatal?: (reason: string) => void;
}

export class WsStreamTransport {
  private url: string;
  private video: HTMLVideoElement;
  private callbacks: WsStreamCallbacks;

  private socket: WebSocket | null = null;
  private player: MsePlayer | null = null;

  private rpcChannel: WsChannel;
  private hidChannel: WsChannel;

  private reconnectAttempts = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private destroyed = false;

  constructor(video: HTMLVideoElement, callbacks: WsStreamCallbacks = {}) {
    this.video = video;
    this.callbacks = callbacks;

    const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
    this.url = `${scheme}//${window.location.host}/stream/ws`;

    const dropSocket = () => this.dropSocket();
    this.rpcChannel = new WsChannel("rpc", data => this.send(WS_CHANNEL.RPC, data), dropSocket);
    this.hidChannel = new WsChannel("hidrpc", data => this.send(WS_CHANNEL.HID, data), dropSocket);
  }

  /**
   * Closes the socket and lets the normal reconnect path rebuild it. Used
   * when a caller asks for a channel to be closed.
   */
  private dropSocket(): void {
    const socket = this.socket;
    if (!socket) return;
    socket.close();
  }

  get channels(): { rpc: DataChannelLike; hid: DataChannelLike } {
    return { rpc: this.rpcChannel, hid: this.hidChannel };
  }

  connect(): void {
    if (this.destroyed || this.socket) return;

    this.callbacks.onStateChange?.("connecting");

    const socket = new WebSocket(this.url);
    socket.binaryType = "arraybuffer";
    this.socket = socket;

    socket.onopen = () => {
      if (this.socket !== socket) return;
      console.log(`${logPrefix} connected`);
      this.reconnectAttempts = 0;

      this.player = new MsePlayer(this.video, {
        onFatal: reason => this.callbacks.onFatal?.(reason),
      });

      this.rpcChannel.setOpen();
      this.hidChannel.setOpen();
      this.callbacks.onChannelsReady?.(this.channels);
      this.callbacks.onStateChange?.("connected");
    };

    socket.onmessage = event => {
      if (this.socket !== socket) return;
      if (!(event.data instanceof ArrayBuffer) || event.data.byteLength < 1) return;

      const frame = new Uint8Array(event.data);
      const channel = frame[0];
      const payload = frame.subarray(1);

      switch (channel) {
        case WS_CHANNEL.VIDEO_INIT:
          this.player?.appendInit(payload.slice());
          break;
        case WS_CHANNEL.VIDEO_SEGMENT:
          this.player?.appendSegment(payload.slice());
          break;
        case WS_CHANNEL.RPC:
          this.rpcChannel.deliver(payload, true);
          break;
        case WS_CHANNEL.HID:
          this.hidChannel.deliver(payload, false);
          break;
        case WS_CHANNEL.CONTROL:
          this.handleControl(payload);
          break;
        default:
          console.warn(`${logPrefix} unknown channel ${channel}`);
      }
    };

    socket.onerror = () => {
      if (this.socket !== socket) return;
      console.warn(`${logPrefix} socket error`);
    };

    socket.onclose = event => {
      if (this.socket !== socket) return;
      console.log(`${logPrefix} closed (${event.code})`);
      this.socket = null;
      this.teardownSession();
      this.callbacks.onStateChange?.("disconnected");
      this.scheduleReconnect();
    };
  }

  private handleControl(payload: Uint8Array): void {
    let message: WsControlMessage;
    try {
      message = JSON.parse(new TextDecoder().decode(payload)) as WsControlMessage;
    } catch {
      console.warn(`${logPrefix} unparseable control message`);
      return;
    }

    switch (message.type) {
      case "ready":
        console.log(`${logPrefix} device ready`);
        break;
      case "video-init":
        console.log(`${logPrefix} video ${message.width}x${message.height} (${message.codec})`);
        this.player?.prepare(message.codec);
        this.callbacks.onVideoDimensions?.(message.width, message.height);
        break;
    }
  }

  private send(channel: number, payload: Uint8Array): void {
    const socket = this.socket;
    if (!socket || socket.readyState !== WebSocket.OPEN) return;

    const framed = new Uint8Array(payload.byteLength + 1);
    framed[0] = channel;
    framed.set(payload, 1);
    socket.send(framed);
  }

  private scheduleReconnect(): void {
    if (this.destroyed || this.reconnectTimer !== null) return;

    const delay = reconnectDelay(this.reconnectAttempts++);
    console.log(`${logPrefix} reconnecting in ${delay}ms`);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  private teardownSession(): void {
    this.rpcChannel.setClosed();
    this.hidChannel.setClosed();
    this.player?.destroy();
    this.player = null;
  }

  destroy(): void {
    this.destroyed = true;

    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }

    const socket = this.socket;
    this.socket = null;
    if (socket) {
      socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null;
      socket.close();
    }

    this.teardownSession();
  }
}
