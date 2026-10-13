package kvm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/jetkvm/kvm/internal/fmp4"
	"github.com/jetkvm/kvm/internal/sync"
	"github.com/rs/zerolog"
)

// The WebSocket transport carries video and the control channels over the
// single TCP connection the browser already opened to load the UI. It exists
// for networks where WebRTC cannot work at all — a client reached through an
// application-level tunnel (ZTNA/SASE proxies, for instance) has no IP path
// to the device, so ICE can never pair a candidate no matter which transport
// or port it is offered on. Signalling succeeds over HTTP while media has
// nowhere to go.
//
// Everything here is therefore strictly client-initiated and same-origin:
// one GET that upgrades, no second port, no inbound connection, no UDP.
//
// Wire format: every WebSocket message is binary, with a one-byte channel
// tag followed by the payload. Text frames are not used, so the client never
// has to branch on message type.
const (
	wsChannelControl      byte = 0x00 // JSON control messages, both directions
	wsChannelVideoInit    byte = 0x01 // fMP4 init segment (ftyp+moov)
	wsChannelVideoSegment byte = 0x02 // fMP4 fragment (moof+mdat)
	wsChannelRPC          byte = 0x10 // JSON-RPC, both directions
	wsChannelHID          byte = 0x11 // HID RPC, both directions
)

const (
	// wsCtrlQueueDepth bounds queued control messages. These are small and
	// must not be dropped, so a full queue means the peer has stopped
	// reading and the session is torn down.
	wsCtrlQueueDepth = 256

	// wsVideoQueueDepth bounds queued video messages. Video is droppable;
	// see video_fanout.go for the resync policy.
	wsVideoQueueDepth = 8

	// wsReadLimit caps an inbound message. RPC payloads carry macros and
	// EDID blobs, so it needs headroom over the library's 32 KiB default.
	wsReadLimit = 1 << 20

	// wsWriteTimeout bounds a single write so one wedged connection cannot
	// pin a writer goroutine forever.
	wsWriteTimeout = 10 * time.Second

	// wsPingInterval keeps intermediaries from idling out the connection.
	// Corporate proxies are the reason this transport exists, and they are
	// often the most aggressive about idle timeouts.
	wsPingInterval = 15 * time.Second
)

// wsOutbound is one framed message waiting to be written.
type wsOutbound struct {
	channel byte
	payload []byte
}

// wsTransport serialises writes to a single WebSocket connection. The
// library's Conn is not safe for concurrent writers, so every send goes
// through one writer goroutine.
type wsTransport struct {
	conn *websocket.Conn
	l    *zerolog.Logger

	ctrl  chan wsOutbound
	video chan wsOutbound

	done      chan struct{}
	closeOnce sync.Once
}

func newWSTransport(conn *websocket.Conn, l *zerolog.Logger) *wsTransport {
	return &wsTransport{
		conn:  conn,
		l:     l,
		ctrl:  make(chan wsOutbound, wsCtrlQueueDepth),
		video: make(chan wsOutbound, wsVideoQueueDepth),
		done:  make(chan struct{}),
	}
}

func (t *wsTransport) isClosed() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

func (t *wsTransport) close() {
	t.closeOnce.Do(func() { close(t.done) })
}

var errWSTransportClosed = errors.New("websocket transport closed")

// enqueueCtrl queues a control message. A full queue means the peer is not
// reading; the session is closed rather than silently losing input.
func (t *wsTransport) enqueueCtrl(channel byte, payload []byte) error {
	if t.isClosed() {
		return errWSTransportClosed
	}

	msg := wsOutbound{channel: channel, payload: payload}
	select {
	case t.ctrl <- msg:
		return nil
	case <-t.done:
		return errWSTransportClosed
	default:
		t.l.Warn().Msg("control queue full, closing websocket session")
		t.close()
		return errWSTransportClosed
	}
}

func (t *wsTransport) sendRPC(text string) error {
	return t.enqueueCtrl(wsChannelRPC, []byte(text))
}

func (t *wsTransport) sendHID(data []byte) error {
	return t.enqueueCtrl(wsChannelHID, data)
}

func (t *wsTransport) sendControl(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return t.enqueueCtrl(wsChannelControl, payload)
}

// enqueueVideo queues a video message, discarding it if the connection is
// behind. Video loss is recoverable at the next keyframe; blocking here
// would stall the capture fan-out.
func (t *wsTransport) enqueueVideo(channel byte, payload []byte) {
	if t.isClosed() {
		return
	}
	select {
	case t.video <- wsOutbound{channel: channel, payload: payload}:
	case <-t.done:
	default:
	}
}

// writeLoop is the only writer on the connection. Control messages take
// priority over video so input never queues behind a frame backlog.
func (t *wsTransport) writeLoop(ctx context.Context) {
	defer t.close()

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()

	for {
		// Drain control first so a video backlog cannot delay a keystroke.
		select {
		case msg := <-t.ctrl:
			if !t.write(ctx, msg) {
				return
			}
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-t.done:
			return
		case msg := <-t.ctrl:
			if !t.write(ctx, msg) {
				return
			}
		case msg := <-t.video:
			if !t.write(ctx, msg) {
				return
			}
		case <-ping.C:
			pingCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := t.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				t.l.Debug().Err(err).Msg("websocket ping failed, closing session")
				return
			}
		}
	}
}

func (t *wsTransport) write(ctx context.Context, msg wsOutbound) bool {
	framed := make([]byte, 0, len(msg.payload)+1)
	framed = append(framed, msg.channel)
	framed = append(framed, msg.payload...)

	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()

	if err := t.conn.Write(writeCtx, websocket.MessageBinary, framed); err != nil {
		t.l.Debug().Err(err).Uint8("channel", msg.channel).Msg("websocket write failed, closing session")
		return false
	}
	return true
}

// handleStreamWebSocket serves the WebSocket transport. It is registered on
// the authenticated router, so the connection carries the same credentials
// as the page that opened it.
func handleStreamWebSocket(c *gin.Context) {
	source := c.ClientIP()
	scopedLogger := websocketLogger.With().
		Str("component", "wsstream").
		Str("source", source).
		Logger()

	conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // same-origin is enforced by the auth middleware
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		scopedLogger.Warn().Err(err).Msg("failed to accept websocket stream connection")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to establish WebSocket connection"})
		return
	}
	conn.SetReadLimit(wsReadLimit)

	// Compression is disabled deliberately: the payload is already-compressed
	// H.264, so deflate would burn CPU on a 256 MB ARM device for nothing.

	defer conn.Close(websocket.StatusNormalClosure, "")

	// Not derived from the request context: the connection is hijacked by the
	// upgrade, and net/http may cancel the request context once it stops
	// managing it. The signaling handler does the same.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	transport := newWSTransport(conn, &scopedLogger)
	session := newBaseSession()
	session.ws = transport

	scopedLogger.Info().Msg("websocket stream session starting")

	// Replace whatever session was active, matching the WebRTC handover.
	displacePreviousSession(session, &scopedLogger)

	activateWebSocketSession(session, &scopedLogger)
	defer deactivateWebSocketSession(session, &scopedLogger)

	go transport.writeLoop(runCtx)
	go pumpVideoToWebSocket(runCtx, transport, &scopedLogger)

	// Announce readiness so the client can distinguish "connected" from
	// "connected but the device has no video yet".
	_ = transport.sendControl(map[string]any{"type": "ready"})

	// Connect-time state mirrors what the WebRTC rpc channel sends OnOpen.
	go reportLocalVersionAndCapabilities(session)
	triggerOTAStateUpdate(otaState.ToRPCState())
	triggerVideoStateUpdate()
	triggerUSBStateUpdate()
	notifyFailsafeMode(session)

	readWebSocketMessages(runCtx, conn, session, transport, &scopedLogger)
}

// readWebSocketMessages demultiplexes inbound frames onto the session's
// ordered queues. It returns when the connection closes.
func readWebSocketMessages(
	ctx context.Context,
	conn *websocket.Conn,
	session *Session,
	transport *wsTransport,
	l *zerolog.Logger,
) {
	defer transport.close()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && websocket.CloseStatus(err) == -1 {
				l.Debug().Err(err).Msg("websocket stream read error")
			}
			return
		}
		if typ != websocket.MessageBinary || len(data) < 1 {
			continue
		}

		channel, payload := data[0], data[1:]
		switch channel {
		case wsChannelRPC:
			if !session.enqueueRPCMessage(dataMessage{Data: payload, IsString: true}) {
				return
			}
		case wsChannelHID:
			if len(payload) < 1 {
				continue
			}
			// Mirror the WebRTC handler: a cancel must pre-empt the queue,
			// otherwise a late cancel can abort the macro that replaced the
			// one it was meant to stop.
			if hidrpcIsCancelMacro(payload) {
				if !session.isClosed() {
					rpcCancelKeyboardMacro()
				}
				continue
			}
			// Clamp as the WebRTC handler does: an unrecognised message type
			// still has to be processed in order rather than dropped.
			queueIndex := hidrpcQueueIndex(payload)
			if queueIndex < 0 || queueIndex >= len(session.hidQueue) {
				l.Warn().Int("queueIndex", queueIndex).Msg("unknown HID RPC queue index")
				queueIndex = 3
			}
			if !session.enqueueHidMessage(queueIndex, hidQueueMessage{
				dataMessage: dataMessage{Data: payload},
				channel:     "websocket",
			}) {
				return
			}
		case wsChannelControl:
			// Reserved for future client-originated control messages.
			l.Trace().Msg("control message from client ignored")
		default:
			l.Debug().Uint8("channel", channel).Msg("unknown websocket channel")
		}
	}
}

// pumpVideoToWebSocket muxes the encoded stream into fMP4 and feeds it to
// the transport.
func pumpVideoToWebSocket(ctx context.Context, transport *wsTransport, l *zerolog.Logger) {
	sub := videoHub.Subscribe()
	defer videoHub.Unsubscribe(sub)

	muxer := fmp4.NewMuxer()
	initSent := false

	// Frames are legitimately unusable until the first keyframe, so those
	// errors are not logged individually. A stream that never becomes usable
	// is a real fault worth reporting once — most likely the encoder is
	// producing something other than H.264.
	var unusableFrames int
	const unusableFrameWarnAfter = 300

	for {
		select {
		case <-ctx.Done():
			return
		case <-transport.done:
			return
		case frame, ok := <-sub.Frames():
			if !ok {
				return
			}
			if !sub.ShouldDeliver(frame) {
				continue
			}

			fragment, err := muxer.WriteAccessUnit(frame.Data, frame.Duration)
			if err != nil {
				if !errors.Is(err, fmp4.ErrNoParameterSets) && !errors.Is(err, fmp4.ErrEmptySample) {
					l.Warn().Err(err).Msg("failed to mux access unit")
					continue
				}
				if unusableFrames++; unusableFrames == unusableFrameWarnAfter {
					l.Error().Err(err).Int("frames", unusableFrames).
						Msg("no usable H.264 access unit yet; is the encoder producing H.264?")
				}
				continue
			}
			unusableFrames = 0

			// A new init segment is required on the first frame and whenever
			// the source changes resolution.
			if muxer.ParametersChanged() || !initSent {
				if err := transport.sendControl(map[string]any{
					"type":   "video-init",
					"codec":  muxer.Codec(),
					"width":  muxer.Width(),
					"height": muxer.Height(),
				}); err != nil {
					return
				}
				transport.enqueueVideo(wsChannelVideoInit, muxer.InitSegment())
				initSent = true
			}

			transport.enqueueVideo(wsChannelVideoSegment, fragment)
		}
	}
}

// activateWebSocketSession brings a WebSocket session up, mirroring what the
// WebRTC path does when ICE reaches Connected.
func activateWebSocketSession(session *Session, l *zerolog.Logger) {
	currentSession = session

	onActiveSessionsChanged()
	if incrActiveSessions() == 1 {
		onFirstSessionConnected(session)
	}

	// The fMP4 muxer only speaks H.264, and Firefox cannot play H.265 in MSE
	// at all. On a handover the encoder may still be on H.265 from the
	// WebRTC session this one replaced, in which case the pipeline has to be
	// restarted — the codec can only be selected while it is stopped.
	// sessionVideoCodecType reports H.264 for a session with no negotiated
	// WebRTC codec, which is every WebSocket session.
	if codec, err := nativeInstance.VideoGetCodecType(); err == nil && codec != 0 {
		l.Info().Int("codec", codec).Msg("restarting video as H.264 for the websocket transport")
		stopNativeVideo()
		startNativeVideoForSession(session)
	}

	notifyFailsafeMode(session)
	if mqttManager != nil {
		mqttManager.publishSessionsState()
	}
	l.Info().Msg("websocket stream session active")
}

// deactivateWebSocketSession tears a session down, mirroring the ICE Closed
// handler.
func deactivateWebSocketSession(session *Session, l *zerolog.Logger) {
	if session.ws != nil {
		session.ws.close()
	}

	if currentSession == session {
		cancelKeyboardMacro()
		gadget.CancelAllAutoReleaseTimers()
		_ = rpcKeyboardReport(0, keyboardClearStateKeys)
		currentSession = nil
	}

	session.close()
	releaseVideoPause(session)

	if session.shouldUmountVirtualMedia {
		if err := rpcUnmountImage(); err != nil {
			l.Warn().Err(err).Msg("unmount image failed on websocket session close")
		}
	}

	onActiveSessionsChanged()
	if decrActiveSessions() == 0 {
		l.Info().Msg("last session disconnected, stopping video stream")
		onLastSessionDisconnected()
	}
	if mqttManager != nil {
		mqttManager.publishSessionsState()
	}
	l.Info().Msg("websocket stream session closed")
}

// displacePreviousSession evicts the session a new connection is replacing.
// Only one client drives the host at a time.
func displacePreviousSession(incoming *Session, l *zerolog.Logger) {
	previous := currentSession
	if previous == nil || previous == incoming {
		return
	}

	writeJSONRPCEvent("otherSessionConnected", nil, previous)
	gadget.CancelAllAutoReleaseTimers()
	_ = rpcKeyboardReport(0, keyboardClearStateKeys)

	if previous.ws != nil {
		ws := previous.ws
		go func() {
			time.Sleep(1 * time.Second)
			ws.close()
		}()
		return
	}

	if previous.peerConnection != nil {
		peerConn := previous.peerConnection
		go func() {
			time.Sleep(1 * time.Second)
			_ = peerConn.Close()
		}()
	}
	l.Info().Msg("displaced previous session")
}
