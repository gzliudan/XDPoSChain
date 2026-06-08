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

// writeSlot is a single-use handshake between a protoRW writer and the
// per-Peer write arbiter in Peer.run. The writer enqueues a slot on either
// the high- or low-priority request channel and waits for the arbiter to
// grant the slot by closing proceed. The writer then performs the actual
// transport write and reports the result on done.
type writeSlot struct {
	proceed chan struct{} // arbiter closes this to grant the write slot
	done    chan error    // writer reports the write result (buffered, len 1)
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

	// Write arbitration: protoRW writers submit writeSlot requests on hiReq
	// (high priority) or loReq (low priority). Peer.run picks one, grants
	// it, waits for completion, then schedules the next. These channels are
	// created in newPeer and never reassigned afterwards.
	hiReq chan *writeSlot
	loReq chan *writeSlot

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
		hiReq:    make(chan *writeSlot, writeReqQueueSize),
		loReq:    make(chan *writeSlot, writeReqQueueSize),
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
		activeDone chan error // non-nil while a write is in flight
		hiStreak   int        // consecutive hi-priority writes since the last lo
	)
	p.wg.Go(func() { p.readLoop(readErr) })
	p.wg.Go(p.pingLoop)

	// Start all protocol handlers.
	p.startProtocols()

	// Write arbiter loop, folded into the main select. The arbiter accepts
	// writeSlot requests only when no write is currently in flight
	// (activeDone == nil), prefers hi over lo, and enforces a starvation
	// guard that biases lo every writePriorityStarveLimit consecutive hi.
	//
	// Note: the hi-over-lo preference is best-effort, not strict. It is
	// strictly honoured only by the non-blocking probe at the top of the
	// loop body. In the blocking select below, if both hiReq and loReq
	// become ready simultaneously the Go runtime picks one at random; the
	// next iteration will re-apply the hi bias, so a sustained hi load is
	// still served ahead of lo on average.
loop:
	for {
		// When no write is in flight, try to pick the next request with a
		// priority bias before falling into the blocking select.
		if activeDone == nil {
			// Control-plane signals must preempt queued writes during teardown.
			// Otherwise a steady stream of ready write requests can keep this
			// loop busy long enough to starve Disconnect / read errors.
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

			if hiStreak >= writePriorityStarveLimit {
				// Bias toward lo: try lo non-blocking first; if no lo is
				// pending, fall through to the blocking select that
				// accepts either.
				select {
				case slot := <-p.loReq:
					close(slot.proceed)
					activeDone = slot.done
					hiStreak = 0
					continue
				default:
				}
			} else {
				// Bias toward hi: try hi non-blocking first; if no hi is
				// pending, fall through to the blocking select that
				// accepts either.
				select {
				case slot := <-p.hiReq:
					close(slot.proceed)
					activeDone = slot.done
					hiStreak++
					continue
				default:
				}
			}
		}

		// Only enable the request channels when no write is in flight.
		var hiReq, loReq <-chan *writeSlot
		if activeDone == nil {
			hiReq, loReq = p.hiReq, p.loReq
		}

		select {
		case slot := <-hiReq:
			close(slot.proceed)
			activeDone = slot.done
			hiStreak++
		case slot := <-loReq:
			close(slot.proceed)
			activeDone = slot.done
			hiStreak = 0
		case err = <-activeDone:
			// Active write finished. Allow the next one to be scheduled.
			activeDone = nil
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

func writeQueuedSlot(reqCh chan<- *writeSlot, closed <-chan struct{}) (*writeSlot, error) {
	slot := &writeSlot{
		proceed: make(chan struct{}),
		done:    make(chan error, 1),
	}
	select {
	case reqCh <- slot:
	case <-closed:
		return nil, ErrShuttingDown
	}
	return slot, nil
}

func (p *Peer) writeMsg(code uint64, payload []interface{}) error {
	slot, err := writeQueuedSlot(p.loReq, p.closed)
	if err != nil {
		return err
	}
	select {
	case <-slot.proceed:
	case <-p.closed:
		return ErrShuttingDown
	}
	err = SendItems(p.rw, code, payload...)
	slot.done <- err
	return err
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
				result[cap.Name] = &protoRW{Protocol: proto, offset: offset, in: make(chan Msg), w: rw}
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
		proto.hiReq = p.hiReq
		proto.loReq = p.loReq
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
	in     chan Msg        // receives read messages
	closed <-chan struct{} // receives when peer is shutting down
	// hiReq/loReq are written once by startProtocols before any concurrent
	// writer is started and are read-only thereafter. Do not reassign them
	// from goroutines other than the one that called startProtocols.
	hiReq  chan<- *writeSlot // high-priority write request queue
	loReq  chan<- *writeSlot // normal-priority write request queue
	offset uint64
	w      MsgWriter
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
	msg.meterCap = rw.cap()
	msg.meterCode = msg.Code

	msg.Code += rw.offset
	reqCh := rw.loReq
	depthMetric, blockedMetric := writeQueueLoDepth, writeQueueLoBlocked
	if high {
		reqCh = rw.hiReq
		depthMetric, blockedMetric = writeQueueHiDepth, writeQueueHiBlocked
	}
	slot, err := writeQueuedSlot(reqCh, rw.closed)
	if err != nil {
		return err
	}
	// Sample queue depth before enqueue so the histogram reflects the
	// back-pressure observed by this writer (slots ahead of it), independent
	// of how quickly the arbiter drains afterwards. This is still a racy
	// snapshot under concurrent producers, but it is a meaningful upper
	// bound on what this writer waited behind, whereas sampling after the
	// send can be drained to ~0 by the arbiter and underreports congestion.
	depthMetric.Update(int64(len(reqCh)))
	// Enqueue the request. Try non-blocking first so we can observe
	// saturation events; on fallback the writer waits like before.
	if len(reqCh) == cap(reqCh) {
		blockedMetric.Mark(1)
	}
	// Wait for the arbiter to grant the slot.
	select {
	case <-slot.proceed:
	case <-rw.closed:
		return ErrShuttingDown
	}
	// Perform the actual write and report the result. done is buffered, so
	// this send never blocks; the arbiter consumes it asynchronously.
	err = rw.w.WriteMsg(msg)
	slot.done <- err
	return err
}

func (rw *protoRW) ReadMsg() (Msg, error) {
	select {
	case msg := <-rw.in:
		msg.Code -= rw.offset
		return msg, nil
	case <-rw.closed:
		return Msg{}, io.EOF
	}
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
