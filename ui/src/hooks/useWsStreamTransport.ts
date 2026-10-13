import { useEffect } from "react";

import { useRTCStore, useVideoStore } from "@hooks/stores";
import { holdRpcEvents } from "@hooks/useJsonRpc";
import { doRpcHidHandshake } from "@hooks/useHidRpc";
import { WsStreamTransport } from "@/transport/wsStream";

/**
 * Runs the WebSocket fallback transport while `enabled`.
 *
 * It publishes into exactly the same store slots the WebRTC path uses — the
 * RPC and HID channels, and the connection state — so every consumer
 * downstream (`useJsonRpc`, `useHidRpc`, the overlays, the status bar) works
 * without knowing which transport is in play.
 *
 * Video goes straight to the `<video>` element that WebRTCVideo registers in
 * the video store, via MSE. `mediaStream` stays null in this mode, which is
 * what keeps the WebRTC attach path dormant.
 */
export function useWsStreamTransport(enabled: boolean): void {
  const videoElement = useVideoStore(state => state.videoElement);

  const setRpcDataChannel = useRTCStore(state => state.setRpcDataChannel);
  const setRpcHidChannel = useRTCStore(state => state.setRpcHidChannel);
  const setRpcHidProtocolVersion = useRTCStore(state => state.setRpcHidProtocolVersion);
  const setPeerConnectionState = useRTCStore(state => state.setPeerConnectionState);
  const setWsTransportActive = useRTCStore(state => state.setWsTransportActive);

  useEffect(() => {
    if (!enabled) return;

    // Claim the session before the socket is up so the overlays stop
    // reporting the WebRTC failure that triggered the handover.
    setWsTransportActive(true);
    setPeerConnectionState("connecting");

    // The player needs a video element; WebRTCVideo registers one on mount.
    if (!videoElement) return;

    const transport = new WsStreamTransport(videoElement, {
      onStateChange: state => setPeerConnectionState(state),
      onChannelsReady: ({ rpc, hid }) => {
        setRpcDataChannel(rpc);
        setRpcHidChannel(hid);
      },
      onVideoDimensions: (width, height) =>
        console.log(`[ws-transport] device reports ${width}x${height}`),
      onFatal: reason => console.error(`[ws-transport] fatal: ${reason}`),
    });

    // Both must be attached before the socket opens. The device sends its
    // connect-time RPC events immediately, and the HID handshake has to be
    // listening for the reply; the channel objects are stable across
    // reconnects, so each is wired up exactly once.
    holdRpcEvents(transport.channels.rpc);
    doRpcHidHandshake(transport.channels.hid, setRpcHidProtocolVersion);

    transport.connect();

    return () => {
      transport.destroy();
      setRpcDataChannel(null);
      setRpcHidChannel(null);
      setRpcHidProtocolVersion(null);
      setWsTransportActive(false);
    };
  }, [
    enabled,
    videoElement,
    setRpcDataChannel,
    setRpcHidChannel,
    setRpcHidProtocolVersion,
    setPeerConnectionState,
    setWsTransportActive,
  ]);
}

/**
 * Whether the fallback should be used instead of WebRTC from the start.
 *
 * `?transport=ws` forces it for one visit; `jetkvm.transport` in
 * localStorage makes it sticky, which is what a user on a network that
 * blocks WebRTC outright wants so they do not wait through a doomed ICE
 * attempt on every load.
 */
export function isWsTransportForced(): boolean {
  try {
    const param = new URLSearchParams(window.location.search).get("transport");
    if (param === "ws" || param === "websocket") return true;
    if (param === "webrtc") return false;
    return window.localStorage.getItem("jetkvm.transport") === "websocket";
  } catch {
    return false;
  }
}
