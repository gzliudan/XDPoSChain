// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package p2p

import (
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common/mclock"
	"github.com/XinFinOrg/XDPoSChain/event"
	"github.com/XinFinOrg/XDPoSChain/log"
	"github.com/XinFinOrg/XDPoSChain/metrics"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
	"github.com/XinFinOrg/XDPoSChain/p2p/enr"
	"github.com/XinFinOrg/XDPoSChain/rlp"
)

var (
	ErrShuttingDown = errors.New("shutting down")
)

const (
	baseProtocolVersion    = 5
	baseProtocolLength     = uint64(16)
	baseProtocolMaxMsgSize = 2 * 1024

	snappyProtocolVersion = 5

	pingInterval = 15 * time.Second
)

const (
	// devp2p message codes
	handshakeMsg = 0x00
	discMsg      = 0x01
	pingMsg      = 0x02
	pongMsg      = 0x03
)

// protoHandshake is the RLP structure of the protocol handshake.
type protoHandshake struct {
	Version    uint64
	Name       string
	Caps       []Cap
	ListenPort uint64
	ID         []byte // secp256k1 public key

	// Ignore additional fields (for forward compatibility).
	Rest []rlp.RawValue `rlp:"tail"`
}

// PeerEventType is the type of peer events emitted by a p2p.Server
type PeerEventType string

const (
	// PeerEventTypeAdd is the type of event emitted when a peer is added
	// to a p2p.Server
	PeerEventTypeAdd PeerEventType = "add"

	// PeerEventTypeDrop is the type of event emitted when a peer is
	// dropped from a p2p.Server
	PeerEventTypeDrop PeerEventType = "drop"

	// PeerEventTypeMsgSend is the type of event emitted when a
	// message is successfully sent to a peer
	PeerEventTypeMsgSend PeerEventType = "msgsend"

	// PeerEventTypeMsgRecv is the type of event emitted when a
	// message is received from a peer
	PeerEventTypeMsgRecv PeerEventType = "msgrecv"
)

// PeerEvent is an event emitted when peers are either added or dropped from
// a p2p.Server or when a message is sent or received on a peer connection
type PeerEvent struct {
	Type          PeerEventType `json:"type"`
	Peer          enode.ID      `json:"peer"`
	Error         string        `json:"error,omitempty"`
	Protocol      string        `json:"protocol,omitempty"`
	MsgCode       *uint64       `json:"msg_code,omitempty"`
	MsgSize       *uint32       `json:"msg_size,omitempty"`
	LocalAddress  string        `json:"local,omitempty"`
	RemoteAddress string        `json:"remote,omitempty"`
}

// PriorityMsgWriter is an optional interface implemented by MsgWriters that
// support two-level write priority. Messages marked as high priority preempt
// normal-priority messages at the next write boundary on the underlying
// transport (in-flight writes are never interrupted).
//
// Any MsgReadWriter that wraps another writer (for example msgEventer or
// eth.meteredMsgReadWriter) MUST also implement PriorityMsgWriter and forward
// the priority flag to the inner writer. Otherwise SendPriority will silently
// fall back to the normal-priority lane on connections that go through the
// wrapper, defeating the BFT latency guarantee for consensus messages.
type PriorityMsgWriter interface {
	MsgWriter
	// WriteMsgPriority writes msg through the underlying transport. If high
	// is true, the message is queued on the high-priority lane and will be
	// served before any pending low-priority writes.
	WriteMsgPriority(msg Msg, high bool) error
}

// writePriorityStarveLimit bounds the number of consecutive high-priority
// writes that may be served before a pending low-priority write is forced
// through. This prevents low-priority traffic (block bodies, transactions)
// from being starved by a constant stream of high-priority messages.
const writePriorityStarveLimit = 16

// writeReqQueueSize is the capacity of the per-priority write request queues.
// A buffered queue lets concurrent writers enqueue while the arbiter is busy
// with an in-flight transport write, which is required for the priority bias
// to take effect. When the queue is full, additional writers block (yielding
// back-pressure equivalent to the single-token scheduler).
//
// The 128 default is sized to absorb a burst from all eth-protocol writers
// on a single peer without blocking: a small constant set of consensus
// senders (VoteMsg / TimeoutMsg / SyncInfoMsg) on the hi lane, plus the
// tx-broadcast loop, block-propagation loop, header/body fetcher responses,
// and downloader requests on the lo lane. In practice steady-state depth
// should stay in single digits; sustained depths above a few dozen indicate
// a slow downstream transport rather than producer bursts.
//
// The depth distribution and back-pressure rate are exposed as metrics
// (p2p/peer/writeq/{hi,lo}/{depth,blocked}); use them to verify whether this
// constant needs tuning under real load.
const writeReqQueueSize = 128

// inflightWriteDrainTimeout bounds how long peer shutdown waits for a single
// in-flight transport write to complete before forcing teardown to continue.
// Defaulting to frameWriteTimeout keeps behavior aligned with transport write
// deadlines while preventing indefinite shutdown stalls.
var inflightWriteDrainTimeout = frameWriteTimeout

// writeRequest is a single outbound write owned by the peer write arbiter.
// Writers submit requests through Peer.writeReq and wait for the arbiter to
// report the final transport result on done.
type writeRequest struct {
	msg  Msg
	high bool
	done chan error
	pend *atomic.Int64
}

func (r *writeRequest) finish(err error) {
	if r.pend != nil {
		r.pend.Add(-1)
		r.pend = nil
	}
	r.done <- err
}

// Peer represents a connected remote node.
type Peer struct {
	rw      *conn
	running map[string]*protoRW
	log     log.Logger
	created mclock.AbsTime

	wg       sync.WaitGroup
	protoErr chan error
	closed   chan struct{}
	pingRecv chan struct{}
	disc     chan DiscReason

	// Write arbitration: writers submit requests on writeReq. Peer.run owns
	// the internal high- and low-priority queues and decides which request
	// reaches the transport next.
	writeReq chan *writeRequest
	hiPend   atomic.Int64
	loPend   atomic.Int64

	// events receives message send / receive events if set
	events *event.Feed
}

// NewPeer returns a peer for testing purposes.
func NewPeer(id enode.ID, name string, caps []Cap) *Peer {
	pipe, _ := net.Pipe()
	node := enode.SignNull(new(enr.Record), id)
	conn := &conn{fd: pipe, transport: nil, node: node, caps: caps, name: name}
	peer := newPeer(conn, nil)
	close(peer.closed) // ensures Disconnect doesn't block
	return peer
}

// ID returns the node's public key.
func (p *Peer) ID() enode.ID {
	return p.rw.node.ID()
}

// Node returns the peer's node descriptor.
func (p *Peer) Node() *enode.Node {
	return p.rw.node
}

// Name returns the node name that the remote node advertised.
func (p *Peer) Name() string {
	return p.rw.name
}

// Caps returns the capabilities (supported subprotocols) of the remote peer.
func (p *Peer) Caps() []Cap {
	// TODO: maybe return copy
	return p.rw.caps
}

// RemoteAddr returns the remote address of the network connection.
func (p *Peer) RemoteAddr() net.Addr {
	return p.rw.fd.RemoteAddr()
}

// LocalAddr returns the local address of the network connection.
func (p *Peer) LocalAddr() net.Addr {
	return p.rw.fd.LocalAddr()
}

// Disconnect terminates the peer connection with the given reason.
// It returns immediately and does not wait until the connection is closed.
func (p *Peer) Disconnect(reason DiscReason) {
	select {
	case p.disc <- reason:
	case <-p.closed:
	}
}

// String implements fmt.Stringer.
func (p *Peer) String() string {
	id := p.ID()
	return fmt.Sprintf("Peer %x %v", id[:8], p.RemoteAddr())
}

// Inbound returns true if the peer is an inbound connection
func (p *Peer) Inbound() bool {
	return p.rw.is(inboundConn)
}

func newPeer(conn *conn, protocols []Protocol) *Peer {
	protomap := matchProtocols(protocols, conn.caps, conn)
	p := &Peer{
		rw:       conn,
		running:  protomap,
		created:  mclock.Now(),
		disc:     make(chan DiscReason),
		protoErr: make(chan error, len(protomap)+1), // protocols + pingLoop
		closed:   make(chan struct{}),
		pingRecv: make(chan struct{}, 16),
		writeReq: make(chan *writeRequest, writeReqQueueSize),
		log:      log.New("id", conn.node.ID(), "conn", conn.flags),
	}
	return p
}

func (p *Peer) Log() log.Logger {
	return p.log
}

func (p *Peer) run() (remoteRequested bool, err error) {
	var (
		readErr    = make(chan error, 1)
		reason     DiscReason // sent to the peer
		activeReq  *writeRequest
		activeDone chan error // non-nil while a transport write is in flight
		hiStreak   int        // consecutive hi-priority writes since the last lo
		hiQueue    = newWriteRequestQueue()
		loQueue    = newWriteRequestQueue()
	)
	p.wg.Go(func() { p.readLoop(readErr) })
	p.wg.Go(p.pingLoop)

	// Start all protocol handlers.
	p.startProtocols()

	// Write arbiter loop. Writers submit requests through writeReq; the peer
	// owns the internal per-priority queues and is solely responsible for
	// choosing the next request that reaches the transport.
loop:
	for {
		if activeReq == nil {
			// Control-plane signals must preempt queued writes during teardown.
			select {
			case err = <-readErr:
				if r, ok := err.(DiscReason); ok {
					remoteRequested = true
					reason = r
				} else {
					reason = DiscNetworkError
				}
				break loop
			case err = <-p.protoErr:
				reason = discReasonForError(err)
				break loop
			case err = <-p.disc:
				reason = discReasonForError(err)
				break loop
			default:
			}

			drainWriteRequests(p.writeReq, hiQueue, loQueue)

			if req, high := pickNextWriteRequest(hiQueue, loQueue, hiStreak); req != nil {
				activeReq = req
				activeDone = make(chan error, 1)
				if high {
					hiStreak++
				} else {
					hiStreak = 0
				}
				go func(msg Msg) {
					activeDone <- p.rw.WriteMsg(msg)
				}(req.msg)
				continue
			}
		}

		select {
		case req := <-p.writeReq:
			if req.high {
				hiQueue.push(req)
			} else {
				loQueue.push(req)
			}
		case err = <-activeDone:
			req := activeReq
			activeReq = nil
			activeDone = nil
			if req != nil {
				req.finish(err)
			}
			if err != nil {
				reason = DiscNetworkError
				break loop
			}
		case err = <-readErr:
			if r, ok := err.(DiscReason); ok {
				remoteRequested = true
				reason = r
			} else {
				reason = DiscNetworkError
			}
			break loop
		case err = <-p.protoErr:
			reason = discReasonForError(err)
			break loop
		case err = <-p.disc:
			reason = discReasonForError(err)
			break loop
		}
	}
	close(p.closed)
	p.rw.close(reason)
	if activeReq != nil {
		select {
		case inflightErr := <-activeDone:
			activeReq.finish(inflightErr)
			if err == nil {
				err = inflightErr
			}
		case <-time.After(inflightWriteDrainTimeout):
			activeReq.finish(ErrShuttingDown)
		}
	}
	failQueuedWriteRequests(ErrShuttingDown, hiQueue)
	failQueuedWriteRequests(ErrShuttingDown, loQueue)
	drainQueuedWriteRequests(p.writeReq, ErrShuttingDown)
	p.wg.Wait()
	return remoteRequested, err
}

func (p *Peer) pingLoop() {
	ping := time.NewTimer(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ping.C:
			if err := p.writeMsg(pingMsg, nil); err != nil {
				p.protoErr <- err
				return
			}
			ping.Reset(pingInterval)

		case <-p.pingRecv:
			_ = p.writeMsg(pongMsg, nil)

		case <-p.closed:
			return
		}
	}
}

type writeRequestQueue struct {
	items []*writeRequest
	head  int
}

func newWriteRequestQueue() *writeRequestQueue {
	return &writeRequestQueue{}
}

func (q *writeRequestQueue) len() int {
	return len(q.items) - q.head
}

func (q *writeRequestQueue) storageLen() int {
	return len(q.items)
}

func (q *writeRequestQueue) push(req *writeRequest) {
	q.items = append(q.items, req)
}

func (q *writeRequestQueue) pop() *writeRequest {
	req := q.items[q.head]
	q.items[q.head] = nil
	q.head++
	if q.head == len(q.items) {
		q.items = nil
		q.head = 0
		return req
	}
	if q.head >= 32 && q.head*2 >= len(q.items) {
		q.compact()
	}
	return req
}

func (q *writeRequestQueue) compact() {
	copy(q.items, q.items[q.head:])
	newLen := len(q.items) - q.head
	for i := newLen; i < len(q.items); i++ {
		q.items[i] = nil
	}
	q.items = q.items[:newLen]
	q.head = 0
}

func (q *writeRequestQueue) remaining() []*writeRequest {
	return q.items[q.head:]
}

func pickNextWriteRequest(hiQueue, loQueue *writeRequestQueue, hiStreak int) (*writeRequest, bool) {
	if hiStreak >= writePriorityStarveLimit && loQueue.len() > 0 {
		return loQueue.pop(), false
	}
	if hiQueue.len() > 0 {
		return hiQueue.pop(), true
	}
	if loQueue.len() > 0 {
		return loQueue.pop(), false
	}
	return nil, false
}

func drainWriteRequests(src <-chan *writeRequest, hiQueue, loQueue *writeRequestQueue) {
	for {
		select {
		case req := <-src:
			if req.high {
				hiQueue.push(req)
			} else {
				loQueue.push(req)
			}
		default:
			return
		}
	}
}

func failQueuedWriteRequests(err error, queue *writeRequestQueue) {
	for _, req := range queue.remaining() {
		if req != nil {
			req.finish(err)
		}
	}
}

func drainQueuedWriteRequests(src <-chan *writeRequest, err error) {
	for {
		select {
		case req := <-src:
			if req != nil {
				req.finish(err)
			}
		default:
			return
		}
	}
}

func enqueueWriteRequest(reqCh chan<- *writeRequest, closed <-chan struct{}, req *writeRequest, blockedMetric *metrics.Meter) error {
	pend := req.pend
	if pend != nil {
		pend.Add(1)
	}
	rollback := func() {
		if pend != nil {
			pend.Add(-1)
		}
	}

	select {
	case reqCh <- req:
		return nil
	case <-closed:
		rollback()
		return ErrShuttingDown
	default:
		if blockedMetric != nil {
			blockedMetric.Mark(1)
		}
		select {
		case reqCh <- req:
			return nil
		case <-closed:
			rollback()
			return ErrShuttingDown
		}
	}
}

func (p *Peer) writeMsg(code uint64, payload []interface{}) error {
	size, r, err := rlp.EncodeToReader(payload)
	if err != nil {
		return err
	}
	req := &writeRequest{
		msg:  Msg{Code: code, Size: uint32(size), Payload: r},
		done: make(chan error, 1),
		pend: &p.loPend,
	}
	if err := enqueueWriteRequest(p.writeReq, p.closed, req, nil); err != nil {
		return err
	}
	return <-req.done
}

func (p *Peer) readLoop(errc chan<- error) {
	for {
		msg, err := p.rw.ReadMsg()
		if err != nil {
			errc <- err
			return
		}
		msg.ReceivedAt = time.Now()
		if err = p.handle(msg); err != nil {
			errc <- err
			return
		}
	}
}

func (p *Peer) handle(msg Msg) error {
	switch {
	case msg.Code == pingMsg:
		msg.Discard()
		select {
		case p.pingRecv <- struct{}{}:
		case <-p.closed:
		}
	case msg.Code == discMsg:
		var reason [1]DiscReason
		// This is the last message. We don't need to discard or
		// check errors because, the connection will be closed after it.
		rlp.Decode(msg.Payload, &reason)
		return reason[0]
	case msg.Code < baseProtocolLength:
		// ignore other base protocol messages
		return msg.Discard()
	default:
		// it's a subprotocol message
		proto, err := p.getProto(msg.Code)
		if err != nil {
			return fmt.Errorf("msg code out of range: %v", msg.Code)
		}
		if metrics.Enabled() {
			metrics.GetOrRegisterMeter(fmt.Sprintf("%s/%s/%d/%#02x", MetricsInboundTraffic, proto.Name, proto.Version, msg.Code-proto.offset), nil).Mark(int64(msg.meterSize))
		}
		select {
		case proto.in <- msg:
			return nil
		case <-p.closed:
			return io.EOF
		}
	}
	return nil
}

func countMatchingProtocols(protocols []Protocol, caps []Cap) int {
	n := 0
	for _, cap := range caps {
		for _, proto := range protocols {
			if proto.Name == cap.Name && proto.Version == cap.Version {
				n++
			}
		}
	}
	return n
}

// matchProtocols creates structures for matching named subprotocols.
func matchProtocols(protocols []Protocol, caps []Cap, rw MsgReadWriter) map[string]*protoRW {
	slices.SortFunc(caps, Cap.Cmp)
	offset := baseProtocolLength
	result := make(map[string]*protoRW)

outer:
	for _, cap := range caps {
		for _, proto := range protocols {
			if proto.Name == cap.Name && proto.Version == cap.Version {
				// If an old protocol version matched, revert it
				if old := result[cap.Name]; old != nil {
					offset -= old.Length
				}
				// Assign the new match
				result[cap.Name] = &protoRW{Protocol: proto, offset: offset, in: make(chan Msg), w: rw, closing: make(chan struct{})}
				offset += proto.Length

				continue outer
			}
		}
	}
	return result
}

func (p *Peer) startProtocols() {
	for _, proto := range p.running {
		proto.closed = p.closed
		proto.writeReq = p.writeReq
		proto.hiPend = &p.hiPend
		proto.loPend = &p.loPend
		proto.closeErr = p.rw.close
		var rw MsgReadWriter = proto
		if p.events != nil {
			rw = newMsgEventer(rw, p.events, p.ID(), proto.Name, p.RemoteAddr().String(), p.LocalAddr().String())
		}
		p.log.Trace(fmt.Sprintf("Starting protocol %s/%d", proto.Name, proto.Version))
		p.wg.Go(func() {
			err := proto.Run(p, rw)
			if err == nil {
				p.log.Trace(fmt.Sprintf("Protocol %s/%d returned", proto.Name, proto.Version))
				err = errProtocolReturned
			} else if err != io.EOF {
				p.log.Trace(fmt.Sprintf("Protocol %s/%d failed", proto.Name, proto.Version), "err", err)
			}
			p.protoErr <- err
		})
	}
}

// getProto finds the protocol responsible for handling
// the given message code.
func (p *Peer) getProto(code uint64) (*protoRW, error) {
	for _, proto := range p.running {
		if code >= proto.offset && code < proto.offset+proto.Length {
			return proto, nil
		}
	}
	return nil, newPeerError(errInvalidMsgCode, "%d", code)
}

type protoRW struct {
	Protocol
	in        chan Msg           // receives read messages
	closed    <-chan struct{}    // receives when peer is shutting down
	closing   chan struct{}      // receives when the protocol reader is force-closed locally
	writeReq  chan *writeRequest // peer-owned write ingress
	hiPend    *atomic.Int64
	loPend    *atomic.Int64
	offset    uint64
	w         MsgWriter
	closeErr  func(error)
	closeOnce sync.Once
}

// Compile-time check that protoRW honours the PriorityMsgWriter contract.
// Wrappers in the chain (msgEventer, eth.meteredMsgReadWriter, ...) MUST do
// the same; otherwise SendPriority silently falls back to the normal lane.
var _ PriorityMsgWriter = (*protoRW)(nil)

// WriteMsg writes msg through the underlying transport at normal priority.
// It implements MsgWriter.
func (rw *protoRW) WriteMsg(msg Msg) error {
	return rw.writeMsg(msg, false)
}

// WriteMsgPriority writes msg through the underlying transport, optionally
// at high priority. It implements PriorityMsgWriter.
func (rw *protoRW) WriteMsgPriority(msg Msg, high bool) error {
	return rw.writeMsg(msg, high)
}

func (rw *protoRW) writeMsg(msg Msg, high bool) (err error) {
	if msg.Code >= rw.Length {
		return newPeerError(errInvalidMsgCode, "not handled")
	}
	select {
	case <-rw.closing:
		return ErrShuttingDown
	case <-rw.closed:
		return ErrShuttingDown
	default:
	}
	msg.meterCap = rw.cap()
	msg.meterCode = msg.Code

	msg.Code += rw.offset
	depthMetric, blockedMetric := writeQueueLoDepth, writeQueueLoBlocked
	pending := rw.loPend
	if high {
		depthMetric, blockedMetric = writeQueueHiDepth, writeQueueHiBlocked
		pending = rw.hiPend
	}
	// Sample queue depth before enqueue so the histogram reflects the
	// same-priority requests already pending ahead of this request across the
	// peer-owned write model, not just the bounded ingress channel occupancy.
	if pending != nil {
		depthMetric.Update(pending.Load())
	}
	req := &writeRequest{
		msg:  msg,
		high: high,
		done: make(chan error, 1),
		pend: pending,
	}
	if pending != nil {
		pending.Add(1)
	}
	rollback := func() {
		if pending != nil {
			pending.Add(-1)
		}
	}

	select {
	case rw.writeReq <- req:
	case <-rw.closing:
		rollback()
		return ErrShuttingDown
	case <-rw.closed:
		rollback()
		return ErrShuttingDown
	default:
		if blockedMetric != nil {
			blockedMetric.Mark(1)
		}
		select {
		case rw.writeReq <- req:
		case <-rw.closing:
			rollback()
			return ErrShuttingDown
		case <-rw.closed:
			rollback()
			return ErrShuttingDown
		}
	}

	return <-req.done
}

func (rw *protoRW) ReadMsg() (Msg, error) {
	select {
	case msg := <-rw.in:
		msg.Code -= rw.offset
		return msg, nil
	case <-rw.closing:
		return Msg{}, io.EOF
	case <-rw.closed:
		return Msg{}, io.EOF
	}
}

func (rw *protoRW) Close() error {
	rw.closeOnce.Do(func() {
		close(rw.closing)
	})
	if rw.closeErr != nil {
		rw.closeErr(DiscQuitting)
	}
	return nil
}

// PeerInfo represents a short summary of the information known about a connected
// peer. Sub-protocol independent fields are contained and initialized here, with
// protocol specifics delegated to all connected sub-protocols.
type PeerInfo struct {
	Enode   string   `json:"enode"` // Node URL
	ID      string   `json:"id"`    // Unique node identifier
	Name    string   `json:"name"`  // Name of the node, including client type, version, OS, custom data
	Caps    []string `json:"caps"`  // Protocols advertised by this peer
	Network struct {
		LocalAddress  string `json:"localAddress"`  // Local endpoint of the TCP data connection
		RemoteAddress string `json:"remoteAddress"` // Remote endpoint of the TCP data connection
		Inbound       bool   `json:"inbound"`
		Trusted       bool   `json:"trusted"`
		Static        bool   `json:"static"`
	} `json:"network"`
	Protocols map[string]interface{} `json:"protocols"` // Sub-protocol specific metadata fields
}

// Info gathers and returns a collection of metadata known about a peer.
func (p *Peer) Info() *PeerInfo {
	// Gather the protocol capabilities
	var caps []string
	for _, cap := range p.Caps() {
		caps = append(caps, cap.String())
	}
	// Assemble the generic peer metadata
	info := &PeerInfo{
		Enode:     p.Node().String(),
		ID:        p.ID().String(),
		Name:      p.Name(),
		Caps:      caps,
		Protocols: make(map[string]interface{}),
	}
	info.Network.LocalAddress = p.LocalAddr().String()
	info.Network.RemoteAddress = p.RemoteAddr().String()
	info.Network.Inbound = p.rw.is(inboundConn)
	info.Network.Trusted = p.rw.is(trustedConn)
	info.Network.Static = p.rw.is(staticDialedConn)

	// Gather all the running protocol infos
	for _, proto := range p.running {
		protoInfo := interface{}("unknown")
		if query := proto.Protocol.PeerInfo; query != nil {
			if metadata := query(p.ID()); metadata != nil {
				protoInfo = metadata
			} else {
				protoInfo = "handshake"
			}
		}
		info.Protocols[proto.Name] = protoInfo
	}
	return info
}
