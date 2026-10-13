package kvm

// A Session is the device's view of one connected client. It was originally
// synonymous with a WebRTC peer connection, but the control channels are now
// carried either by WebRTC data channels or by the WebSocket transport (see
// websocket_stream.go), so everything below the channel plumbing is shared.
//
// Which transport a session uses is decided once, at construction, and never
// changes: a session is torn down and replaced rather than migrated.

// dataMessage is one message received on a control channel. It mirrors the
// shape of webrtc.DataChannelMessage so the handlers do not care which
// transport delivered it.
type dataMessage struct {
	Data     []byte
	IsString bool
}

// hidQueueMessage pairs a control message with the channel it arrived on,
// which the HID handlers log for diagnostics.
type hidQueueMessage struct {
	dataMessage
	channel string
}

// rpcQueueDepth bounds how many RPC requests may be in flight. The queue
// exists to preserve ordering, not to absorb load.
const rpcQueueDepth = 256

// newBaseSession builds the transport-independent half of a session: the
// ordered RPC queue, the four HID queues and the outgoing key-state queue,
// along with the goroutines that drain them. Callers attach a transport
// afterwards.
func newBaseSession() *Session {
	session := &Session{
		done:     make(chan struct{}),
		rpcQueue: make(chan dataMessage, rpcQueueDepth),
	}
	session.initQueues()
	session.initKeysDownStateQueue()
	session.startQueueWorkers()
	return session
}

// startQueueWorkers drains the RPC and HID queues for the life of the
// session.
func (s *Session) startQueueWorkers() {
	rpcQueue := s.rpcQueue
	go func() {
		for {
			select {
			case <-s.done:
				return
			default:
			}

			select {
			case <-s.done:
				return
			case msg := <-rpcQueue:
				// TODO: only use goroutine if the task is asynchronous
				go onRPCMessage(msg, s)
			}
		}
	}()

	for _, queue := range s.hidQueue {
		go s.handleHidQueue(queue)
	}
}

// rpcReady reports whether JSON-RPC messages can be sent to this session.
func (s *Session) rpcReady() bool {
	if s == nil {
		return false
	}
	if s.ws != nil {
		return !s.ws.isClosed()
	}
	return s.RPCChannel != nil
}

// hidReady reports whether HID RPC messages can be sent to this session.
func (s *Session) hidReady() bool {
	if s == nil {
		return false
	}
	if s.ws != nil {
		return !s.ws.isClosed()
	}
	return s.HidChannel != nil
}

// sendRPCText delivers a JSON-RPC response or event over whichever transport
// this session uses.
func (s *Session) sendRPCText(text string) error {
	if s.ws != nil {
		return s.ws.sendRPC(text)
	}
	return s.RPCChannel.SendText(text)
}

// sendHID delivers a binary HID RPC message over whichever transport this
// session uses.
func (s *Session) sendHID(data []byte) error {
	if s.ws != nil {
		return s.ws.sendHID(data)
	}
	return s.HidChannel.Send(data)
}
