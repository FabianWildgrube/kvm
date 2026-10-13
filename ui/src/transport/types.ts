/**
 * The RPC and HID hooks only ever use a handful of members of
 * RTCDataChannel. Describing that subset lets the WebSocket transport supply
 * a stand-in, so `useJsonRpc` and `useHidRpc` work unchanged whether the
 * session is carried by WebRTC or by the fallback transport.
 *
 * RTCDataChannel satisfies this structurally; no cast is needed at the call
 * sites that still create real data channels.
 */
export interface DataChannelLike {
  readonly label: string;
  readonly readyState: RTCDataChannelState;
  binaryType: BinaryType;

  send(data: string | ArrayBufferLike | ArrayBufferView | Blob): void;

  /**
   * Drops the channel. Callers use this to force a fresh session when the
   * device stops acknowledging — see the keyboard macro timeout.
   */
  close(): void;

  addEventListener(
    type: "message",
    listener: (ev: MessageEvent) => void,
    options?: boolean | AddEventListenerOptions,
  ): void;
  addEventListener(
    type: "open" | "close" | "error",
    listener: (ev: Event) => void,
    options?: boolean | AddEventListenerOptions,
  ): void;

  removeEventListener(
    type: "message",
    listener: (ev: MessageEvent) => void,
    options?: boolean | EventListenerOptions,
  ): void;
  removeEventListener(
    type: "open" | "close" | "error",
    listener: (ev: Event) => void,
    options?: boolean | EventListenerOptions,
  ): void;

  /** Used by the e2e tests to inject synthetic device messages. */
  dispatchEvent(event: Event): boolean;
}

/** Binary channel tags; must match the constants in websocket_stream.go. */
export const WS_CHANNEL = {
  CONTROL: 0x00,
  VIDEO_INIT: 0x01,
  VIDEO_SEGMENT: 0x02,
  RPC: 0x10,
  HID: 0x11,
} as const;

/** Control messages the device sends on WS_CHANNEL.CONTROL. */
export type WsControlMessage =
  | { type: "ready" }
  | { type: "video-init"; codec: string; width: number; height: number };
