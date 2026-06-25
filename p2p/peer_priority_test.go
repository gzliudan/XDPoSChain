// Copyright 2025 The XDPoSChain Authors
// This file is part of the XDPoSChain library.
//
// The XDPoSChain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The XDPoSChain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the XDPoSChain library. If not, see <http://www.gnu.org/licenses/>.

package p2p

import (
	"bytes"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XinFinOrg/XDPoSChain/event"
	"github.com/XinFinOrg/XDPoSChain/metrics"
	"github.com/XinFinOrg/XDPoSChain/p2p/enode"
)

// readCode reads one message from rw, discards its body and returns the code.
func readCode(t *testing.T, rw MsgReader) uint64 {
	t.Helper()
	msg, err := rw.ReadMsg()
	if err != nil {
		t.Fatalf("ReadMsg: %v", err)
	}
	if err := msg.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	return msg.Code
}

func awaitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

type discardWriter struct{}

func (discardWriter) WriteMsg(msg Msg) error {
	if msg.Payload == nil {
		return nil
	}
	return msg.Discard()
}

func awaitQueueDepth(t *testing.T, ch <-chan *writeRequest, want int, label string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(ch) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: got depth %d want %d", label, len(ch), want)
}

func TestProtoRWWriteQueueDepthSamplesWritersAhead(t *testing.T) {
	metrics.Enable()
	writeQueueHiDepth.Clear()

	var hiPend atomic.Int64
	hiPend.Store(1)
	writeReq := make(chan *writeRequest, 2)
	writeReq <- &writeRequest{high: true, done: make(chan error, 1), pend: &hiPend}
	closed := make(chan struct{})
	rw := &protoRW{
		Protocol: Protocol{Length: 1},
		closed:   closed,
		writeReq: writeReq,
		hiPend:   &hiPend,
		w:        discardWriter{},
	}

	result := make(chan error, 1)
	go func() {
		result <- rw.WriteMsgPriority(Msg{Code: 0, Payload: bytes.NewReader(nil)}, true)
	}()

	awaitQueueDepth(t, writeReq, 2, "priority queue to contain preload plus new writer")
	after := writeQueueHiDepth.Snapshot()
	if got := after.Count(); got != 1 {
		t.Fatalf("histogram samples: got %d want 1", got)
	}
	if got := after.Sum(); got != 1 {
		t.Fatalf("queued writers ahead: got %d want 1", got)
	}

	<-writeReq
	req := <-writeReq
	req.done <- nil
	if err := <-result; err != nil {
		t.Fatalf("WriteMsgPriority: %v", err)
	}
	close(closed)
}

func TestProtoRWWriteQueueDepthIncludesPendingRequestsOutsideIngress(t *testing.T) {
	metrics.Enable()
	writeQueueHiDepth.Clear()

	var hiPend atomic.Int64
	hiPend.Store(1)
	writeReq := make(chan *writeRequest, 1)
	closed := make(chan struct{})
	rw := &protoRW{
		Protocol: Protocol{Length: 1},
		closed:   closed,
		writeReq: writeReq,
		hiPend:   &hiPend,
		w:        discardWriter{},
	}

	result := make(chan error, 1)
	go func() {
		result <- rw.WriteMsgPriority(Msg{Code: 0, Payload: bytes.NewReader(nil)}, true)
	}()

	awaitQueueDepth(t, writeReq, 1, "priority ingress to receive the new writer")
	after := writeQueueHiDepth.Snapshot()
	if got := after.Count(); got != 1 {
		t.Fatalf("histogram samples: got %d want 1", got)
	}
	if got := after.Sum(); got != 1 {
		t.Fatalf("queued writers ahead: got %d want 1", got)
	}

	req := <-writeReq
	req.done <- nil
	if err := <-result; err != nil {
		t.Fatalf("WriteMsgPriority: %v", err)
	}
	close(closed)
}

func TestProtoRWWriteQueueBlockedMeterCountsFullQueueWait(t *testing.T) {
	metrics.Enable()

	writeReq := make(chan *writeRequest, 1)
	writeReq <- &writeRequest{high: true, done: make(chan error, 1)}
	closed := make(chan struct{})
	rw := &protoRW{
		Protocol: Protocol{Length: 1},
		closed:   closed,
		writeReq: writeReq,
		hiPend:   new(atomic.Int64),
		w:        discardWriter{},
	}

	before := writeQueueHiBlocked.Snapshot().Count()
	result := make(chan error, 1)
	go func() {
		result <- rw.WriteMsgPriority(Msg{Code: 0, Payload: bytes.NewReader(nil)}, true)
	}()

	select {
	case err := <-result:
		t.Fatalf("WriteMsgPriority returned before queue space was freed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	requests := make(chan *writeRequest, 1)
	go func() {
		<-writeReq
		requests <- <-writeReq
	}()

	req := <-requests
	req.done <- nil
	if err := <-result; err != nil {
		t.Fatalf("WriteMsgPriority: %v", err)
	}
	after := writeQueueHiBlocked.Snapshot().Count()
	if got := after - before; got != 1 {
		t.Fatalf("blocked meter delta: got %d want 1", got)
	}
	close(closed)
}

func TestEnqueueWriteRequestPendingAccountingRace(t *testing.T) {
	t.Helper()

	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(2))

	for i := 0; i < 1000; i++ {
		var pend atomic.Int64
		reqCh := make(chan *writeRequest)
		closed := make(chan struct{})
		req := &writeRequest{done: make(chan error, 1), pend: &pend}
		finished := make(chan struct{})

		go func() {
			received := <-reqCh
			received.finish(nil)
			close(finished)
		}()

		if err := enqueueWriteRequest(reqCh, closed, req, nil); err != nil {
			t.Fatalf("enqueueWriteRequest: %v", err)
		}
		<-finished
		if got := pend.Load(); got != 0 {
			t.Fatalf("iteration %d: pending count = %d, want 0", i, got)
		}
		if err := <-req.done; err != nil {
			t.Fatalf("iteration %d: completion error = %v", i, err)
		}
		close(closed)
	}
}

func TestWriteRequestQueuePreservesFIFOAndReusesStorage(t *testing.T) {
	queue := newWriteRequestQueue()
	first := &writeRequest{done: make(chan error, 1)}
	second := &writeRequest{done: make(chan error, 1)}
	third := &writeRequest{done: make(chan error, 1)}

	queue.push(first)
	queue.push(second)
	if got := queue.pop(); got != first {
		t.Fatalf("first pop: got %p want %p", got, first)
	}
	queue.push(third)

	if got := queue.pop(); got != second {
		t.Fatalf("second pop: got %p want %p", got, second)
	}
	if got := queue.pop(); got != third {
		t.Fatalf("third pop: got %p want %p", got, third)
	}
	if got := queue.len(); got != 0 {
		t.Fatalf("queue length: got %d want 0", got)
	}
	if got := queue.storageLen(); got != 0 {
		t.Fatalf("storage length after draining: got %d want 0", got)
	}
}

type writeGateTransport struct {
	transport
	started chan struct{}
	release chan struct{}
	blocked int32
}

func newWriteGateTransport(inner transport) *writeGateTransport {
	return &writeGateTransport{
		transport: inner,
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (t *writeGateTransport) WriteMsg(msg Msg) error {
	if atomic.CompareAndSwapInt32(&t.blocked, 0, 1) {
		close(t.started)
		<-t.release
	}
	return t.transport.WriteMsg(msg)
}

func (t *writeGateTransport) unblockFirstWrite() {
	close(t.release)
}

type writeNilGateTransport struct {
	transport
	started chan struct{}
	release chan struct{}
	blocked int32
}

type stuckWriteTransport struct {
	transport
	started chan struct{}
	block   chan struct{}
	blocked int32
}

func newStuckWriteTransport(inner transport) *stuckWriteTransport {
	return &stuckWriteTransport{
		transport: inner,
		started:   make(chan struct{}),
		block:     make(chan struct{}),
	}
}

func (t *stuckWriteTransport) WriteMsg(msg Msg) error {
	if atomic.CompareAndSwapInt32(&t.blocked, 0, 1) {
		close(t.started)
	}
	<-t.block
	return t.transport.WriteMsg(msg)
}

func newWriteNilGateTransport(inner transport) *writeNilGateTransport {
	return &writeNilGateTransport{
		transport: inner,
		started:   make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (t *writeNilGateTransport) WriteMsg(msg Msg) error {
	if atomic.CompareAndSwapInt32(&t.blocked, 0, 1) {
		close(t.started)
		<-t.release
	}
	if msg.Payload != nil {
		_ = msg.Discard()
	}
	return nil
}

func (t *writeNilGateTransport) unblockFirstWrite() {
	close(t.release)
}

type concurrentWriteGateTransport struct {
	transport
	started    chan struct{}
	release    chan struct{}
	concurrent chan struct{}
	blocked    int32
	active     int32
}

func newConcurrentWriteGateTransport(inner transport) *concurrentWriteGateTransport {
	return &concurrentWriteGateTransport{
		transport:  inner,
		started:    make(chan struct{}),
		release:    make(chan struct{}),
		concurrent: make(chan struct{}, 1),
	}
}

func (t *concurrentWriteGateTransport) WriteMsg(msg Msg) error {
	if atomic.AddInt32(&t.active, 1) > 1 {
		select {
		case t.concurrent <- struct{}{}:
		default:
		}
	}
	if atomic.CompareAndSwapInt32(&t.blocked, 0, 1) {
		close(t.started)
		<-t.release
	}
	err := t.transport.WriteMsg(msg)
	atomic.AddInt32(&t.active, -1)
	return err
}

func (t *concurrentWriteGateTransport) unblockFirstWrite() {
	close(t.release)
}

type writeQueueHooks struct {
	hiQueued chan struct{}
	loQueued chan struct{}
	stop     chan struct{}
	proto    *protoRW
	origReq  chan *writeRequest
	closed   <-chan struct{}
}

// hookProtoWriteRequests replaces a protoRW's write ingress with an observable
// proxy, allowing tests to confirm request admission without depending on the
// peer arbiter's internal per-priority queue representation.
func hookProtoWriteRequests(t *testing.T, rw MsgReadWriter) *writeQueueHooks {
	t.Helper()

	proto, ok := rw.(*protoRW)
	if !ok {
		t.Fatalf("unexpected MsgReadWriter type %T", rw)
	}
	hooks := &writeQueueHooks{
		hiQueued: make(chan struct{}, writeReqQueueSize),
		loQueued: make(chan struct{}, writeReqQueueSize),
		stop:     make(chan struct{}),
		proto:    proto,
		origReq:  proto.writeReq,
		closed:   proto.closed,
	}
	proxy := make(chan *writeRequest, writeReqQueueSize)
	proto.writeReq = proxy

	go forwardWriteRequests(proxy, hooks.origReq, hooks.closed, hooks.hiQueued, hooks.loQueued, hooks.stop)

	return hooks
}

func forwardWriteRequests(in <-chan *writeRequest, out chan *writeRequest, closed <-chan struct{}, hiAck, loAck chan<- struct{}, stop <-chan struct{}) {
	for {
		select {
		case req := <-in:
			select {
			case <-closed:
				req.done <- ErrShuttingDown
				continue
			default:
			}
			select {
			case out <- req:
				if req.high {
					hiAck <- struct{}{}
				} else {
					loAck <- struct{}{}
				}
			case <-closed:
				req.done <- ErrShuttingDown
			case <-stop:
				req.done <- ErrShuttingDown
				return
			}
		case <-closed:
			drainQueuedWriteRequests(in, ErrShuttingDown)
			drainQueuedWriteRequests(out, ErrShuttingDown)
			return
		case <-stop:
			drainQueuedWriteRequests(in, ErrShuttingDown)
			return
		}
	}
}

func (h *writeQueueHooks) close() {
	h.proto.writeReq = h.origReq
	close(h.stop)
}

// TestPeerWritePriorityPreemption verifies that a high-priority write is
// served before a low-priority write that was enqueued first, as long as
// both are pending when the previous write finishes.
func TestPeerWritePriorityPreemption(t *testing.T) {
	rwc := make(chan MsgReadWriter, 1)
	stop := make(chan struct{})
	proto := Protocol{
		Name:   "a",
		Length: 64,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-stop
			return nil
		},
	}
	var gate *writeGateTransport
	closer, remote, _, _ := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newWriteGateTransport(inner)
		return gate
	})
	defer closer()
	defer close(stop)

	rw := <-rwc
	hooks := hookProtoWriteRequests(t, rw)
	defer hooks.close()

	// Slot 1: low priority. The send blocks at the transport because nobody
	// is reading from `remote` yet. While it blocks, the arbiter is parked
	// on activeDone.
	first := make(chan error, 1)
	go func() { first <- SendItems(rw, 1) }()
	awaitSignal(t, gate.started, "first write to reach the transport")

	// Enqueue a low-priority write, then a high-priority write. The hi/lo
	// request channels are buffered, so both enqueue immediately and wait
	// on their proceed signal.
	loCh := make(chan error, 1)
	hiCh := make(chan error, 1)
	go func() { loCh <- SendItems(rw, 2) }()
	awaitSignal(t, hooks.loQueued, "low-priority request to enqueue")
	go func() { hiCh <- SendPriority(rw, 3, []uint{}) }()
	awaitSignal(t, hooks.hiQueued, "high-priority request to enqueue")
	gate.unblockFirstWrite()

	// Drain the first message. After this, the arbiter releases slot 1
	// (no error) and picks the next pending request, preferring hi.
	if got, want := readCode(t, remote), uint64(baseProtocolLength+1); got != want {
		t.Fatalf("first message code: got %d, want %d", got, want)
	}
	if err := <-first; err != nil {
		t.Fatalf("first write: %v", err)
	}

	// The next message on the wire must be the high-priority one (code 3),
	// even though the low-priority one (code 2) was enqueued first.
	if got, want := readCode(t, remote), uint64(baseProtocolLength+3); got != want {
		t.Fatalf("preemption failed: next code = %d, want hi=%d", got, want)
	}
	if err := <-hiCh; err != nil {
		t.Fatalf("hi write: %v", err)
	}

	// Finally the low-priority one is served.
	if got, want := readCode(t, remote), uint64(baseProtocolLength+2); got != want {
		t.Fatalf("lo not served last: code = %d, want lo=%d", got, want)
	}
	if err := <-loCh; err != nil {
		t.Fatalf("lo write: %v", err)
	}
}

// TestPeerPingLoopUsesQueuedWriteRequest verifies that pingLoop does not write
// directly to the transport while another write is in flight.
func TestPeerPingLoopUsesQueuedWriteRequest(t *testing.T) {
	rwc := make(chan MsgReadWriter, 1)
	stop := make(chan struct{})
	proto := Protocol{
		Name:   "a",
		Length: 64,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-stop
			return nil
		},
	}
	var gate *concurrentWriteGateTransport
	closer, remote, peer, _ := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newConcurrentWriteGateTransport(inner)
		return gate
	})
	defer closer()
	defer close(stop)

	rw := <-rwc

	first := make(chan error, 1)
	go func() { first <- SendItems(rw, 1) }()
	awaitSignal(t, gate.started, "first write to reach the transport")

	peer.pingRecv <- struct{}{}

	select {
	case <-gate.concurrent:
		t.Fatal("pingLoop wrote to transport concurrently with an in-flight write")
	case <-time.After(100 * time.Millisecond):
	}

	gate.unblockFirstWrite()

	firstCode := readCode(t, remote)
	if err := <-first; err != nil {
		t.Fatalf("first write: %v", err)
	}
	secondCode := readCode(t, remote)
	firstWant := uint64(baseProtocolLength + 1)
	secondWant := uint64(pongMsg)
	if !((firstCode == firstWant && secondCode == secondWant) || (firstCode == secondWant && secondCode == firstWant)) {
		t.Fatalf("unexpected message codes: got %d then %d, want %d and %d in any order", firstCode, secondCode, firstWant, secondWant)
	}
}

// TestPeerWriteStarvationGuard verifies that after writePriorityStarveLimit
// consecutive high-priority writes, a pending low-priority write is forced
// through ahead of the next high-priority one.
func TestPeerWriteStarvationGuard(t *testing.T) {
	rwc := make(chan MsgReadWriter, 1)
	stop := make(chan struct{})
	proto := Protocol{
		Name:   "a",
		Length: 64,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-stop
			return nil
		},
	}
	var gate *writeGateTransport
	closer, remote, _, _ := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newWriteGateTransport(inner)
		return gate
	})
	defer closer()
	defer close(stop)

	rw := <-rwc
	hooks := hookProtoWriteRequests(t, rw)
	defer hooks.close()

	// Block the arbiter on an initial low-priority in-flight write so we
	// can pre-load both queues deterministically.
	first := make(chan error, 1)
	go func() { first <- SendItems(rw, 0) }()
	awaitSignal(t, gate.started, "first write to reach the transport")

	// Enqueue one low-priority write (code 1) ahead of any high-priority
	// ones to make the starvation case observable.
	loCh := make(chan error, 1)
	go func() { loCh <- SendItems(rw, 1) }()
	awaitSignal(t, hooks.loQueued, "low-priority request to enqueue")

	// Then enqueue writePriorityStarveLimit+1 high-priority writes with
	// codes 2..N. The first writePriorityStarveLimit of them must be
	// served before the pending low-priority one is forced through.
	const extraHi = writePriorityStarveLimit + 1
	hiCh := make(chan error, extraHi)
	for i := 0; i < extraHi; i++ {
		code := uint64(2 + i)
		go func() { hiCh <- SendPriority(rw, code, []uint{}) }()
	}
	for i := 0; i < extraHi; i++ {
		awaitSignal(t, hooks.hiQueued, "high-priority request to enqueue")
	}
	gate.unblockFirstWrite()

	// Drain the in-flight initial write (code 0).
	if got, want := readCode(t, remote), uint64(baseProtocolLength+0); got != want {
		t.Fatalf("initial code: got %d, want %d", got, want)
	}
	if err := <-first; err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Read writePriorityStarveLimit messages: all should be high-priority
	// (codes in [2, 2+extraHi)).
	for i := 0; i < writePriorityStarveLimit; i++ {
		got := readCode(t, remote)
		if got < baseProtocolLength+2 || got >= baseProtocolLength+2+extraHi {
			t.Fatalf("write %d: expected high-priority code, got %d", i, got)
		}
	}

	// The next message must be the low-priority one (code 1), forced
	// through by the starvation guard.
	if got, want := readCode(t, remote), uint64(baseProtocolLength+1); got != want {
		t.Fatalf("starvation guard failed: next code = %d, want lo=%d", got, want)
	}
	if err := <-loCh; err != nil {
		t.Fatalf("lo write: %v", err)
	}

	// Drain the remaining high-priority write so the test goroutines exit
	// cleanly.
	_ = readCode(t, remote)
	for i := 0; i < extraHi; i++ {
		if err := <-hiCh; err != nil {
			t.Fatalf("hi write %d: %v", i, err)
		}
	}
}

// TestSendPriorityFallback verifies that SendPriority falls back to the
// regular WriteMsg path on writers that do not implement PriorityMsgWriter.
func TestSendPriorityFallback(t *testing.T) {
	mw := &capturingWriter{}
	if err := SendPriority(mw, 42, []uint{1, 2}); err != nil {
		t.Fatalf("SendPriority: %v", err)
	}
	if !mw.called {
		t.Fatal("WriteMsg was not called on fallback writer")
	}
	if mw.code != 42 {
		t.Fatalf("code: got %d, want 42", mw.code)
	}
}

type capturingWriter struct {
	called bool
	code   uint64
}

func (w *capturingWriter) WriteMsg(msg Msg) error {
	w.called = true
	w.code = msg.Code
	return msg.Discard()
}

// priorityCapturingRW is a MsgReadWriter that records whether the high or low
// lane was used for the last write. It is used to verify that a wrapping
// MsgReadWriter (such as msgEventer) preserves the PriorityMsgWriter
// interface to the underlying transport.
type priorityCapturingRW struct {
	last Msg
	high bool
	used bool // true if WriteMsgPriority was used
}

func (rw *priorityCapturingRW) ReadMsg() (Msg, error)  { return Msg{}, io.EOF }
func (rw *priorityCapturingRW) WriteMsg(msg Msg) error { rw.last = msg; rw.used = false; return nil }
func (rw *priorityCapturingRW) WriteMsgPriority(msg Msg, high bool) error {
	rw.last = msg
	rw.high = high
	rw.used = true
	return nil
}

// TestMsgEventerForwardsPriority verifies that msgEventer preserves the
// PriorityMsgWriter contract: a SendPriority call through the wrapper still
// reaches the underlying writer's high-priority lane, and the corresponding
// PeerEventTypeMsgSend event is emitted.
func TestMsgEventerForwardsPriority(t *testing.T) {
	inner := &priorityCapturingRW{}
	feed := new(event.Feed)
	ch := make(chan *PeerEvent, 1)
	sub := feed.Subscribe(ch)
	defer sub.Unsubscribe()

	ev := newMsgEventer(inner, feed, enode.ID{}, "test", "remote:30303", "local:30303")

	if _, ok := MsgReadWriter(ev).(PriorityMsgWriter); !ok {
		t.Fatal("msgEventer does not implement PriorityMsgWriter")
	}
	if err := SendPriority(ev, 7, []uint{1}); err != nil {
		t.Fatalf("SendPriority: %v", err)
	}
	if !inner.used {
		t.Fatal("priority lane was not used on inner writer")
	}
	if !inner.high {
		t.Fatal("high flag was not propagated to inner writer")
	}
	if inner.last.Code != 7 {
		t.Fatalf("code: got %d, want 7", inner.last.Code)
	}
	select {
	case e := <-ch:
		if e.Type != PeerEventTypeMsgSend {
			t.Fatalf("event type: got %v, want %v", e.Type, PeerEventTypeMsgSend)
		}
		if e.LocalAddress != "local:30303" {
			t.Fatalf("local address: got %q, want %q", e.LocalAddress, "local:30303")
		}
		if e.RemoteAddress != "remote:30303" {
			t.Fatalf("remote address: got %q, want %q", e.RemoteAddress, "remote:30303")
		}
	case <-time.After(time.Second):
		t.Fatal("no PeerEventTypeMsgSend emitted")
	}
}

// TestPeerWriteLoEventuallyServedUnderSustainedHiPressure verifies the
// strict starvation guard: even when high-priority writes are arriving
// continuously (both preloaded and produced in the background while the
// peer is draining), a single low-priority write is forced through after
// at most writePriorityStarveLimit consecutive high-priority writes.
//
// The deterministic upper bound is exactly writePriorityStarveLimit. Once
// the starvation guard trips and a low-priority write is pending, the next
// scheduled request must come from the low-priority lane.
func TestPeerWriteLoEventuallyServedUnderSustainedHiPressure(t *testing.T) {
	rwc := make(chan MsgReadWriter, 1)
	stop := make(chan struct{})
	proto := Protocol{
		Name:   "a",
		Length: 4096,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-stop
			return nil
		},
	}
	var gate *writeGateTransport
	closer, remote, _, _ := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newWriteGateTransport(inner)
		return gate
	})
	defer closer()
	defer close(stop)

	rw := <-rwc
	hooks := hookProtoWriteRequests(t, rw)
	defer hooks.close()

	// Park the arbiter on an initial in-flight write so we can pre-load
	// both lanes deterministically before any traffic reaches the wire.
	first := make(chan error, 1)
	go func() { first <- SendItems(rw, 0) }()
	awaitSignal(t, gate.started, "first write to reach the transport")

	// Enqueue the single low-priority write (code 1) that the test will
	// look for on the wire.
	loCh := make(chan error, 1)
	go func() { loCh <- SendItems(rw, 1) }()
	awaitSignal(t, hooks.loQueued, "low-priority request to enqueue")

	// Pre-load a burst of high-priority writes so the hi lane is already
	// saturated before draining begins.
	const preHi = writePriorityStarveLimit * 3
	hiCh := make(chan error, preHi)
	for i := 0; i < preHi; i++ {
		code := uint64(100 + i)
		go func() { hiCh <- SendPriority(rw, code, []uint{}) }()
	}
	for i := 0; i < preHi; i++ {
		awaitSignal(t, hooks.hiQueued, "preloaded hi to enqueue")
	}

	// Start a producer that keeps issuing high-priority writes while the
	// test drains the wire, modelling unbounded sustained hi pressure.
	// It exits on producerStop or when the peer closes (deferred teardown
	// unblocks any in-flight SendPriority call with ErrShuttingDown).
	producerStop := make(chan struct{})
	go func() {
		code := uint64(10000)
		for {
			select {
			case <-producerStop:
				return
			default:
			}
			if err := SendPriority(rw, code, []uint{}); err != nil {
				return
			}
			code++
		}
	}()

	gate.unblockFirstWrite()

	// Drain the initial in-flight write (code 0).
	if got, want := readCode(t, remote), uint64(baseProtocolLength+0); got != want {
		t.Fatalf("initial code: got %d, want %d", got, want)
	}
	if err := <-first; err != nil {
		t.Fatalf("initial write: %v", err)
	}

	// Read messages until we see the lo (code 1). A strict starvation
	// guard must schedule it after exactly writePriorityStarveLimit hi writes.
	const bound = writePriorityStarveLimit
	hiBefore := 0
	for i := 0; i < bound; i++ {
		got := readCode(t, remote)
		if got == baseProtocolLength+1 {
			close(producerStop)
			if err := <-loCh; err != nil {
				t.Fatalf("lo write: %v", err)
			}
			return
		}
		hiBefore++
	}
	if got := readCode(t, remote); got == baseProtocolLength+1 {
		close(producerStop)
		if err := <-loCh; err != nil {
			t.Fatalf("lo write: %v", err)
		}
		return
	}
	close(producerStop)
	t.Fatalf("lo write not served within %d hi writes under sustained hi pressure (saw %d hi)", bound, hiBefore)
}

func TestPeerWriteShutdownPreservesInflightTransportError(t *testing.T) {
	rwc := make(chan MsgReadWriter, 1)
	stop := make(chan struct{})
	proto := Protocol{
		Name:   "a",
		Length: 64,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-stop
			return nil
		},
	}
	var gate *writeGateTransport
	closer, _, peer, _ := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newWriteGateTransport(inner)
		return gate
	})
	defer close(stop)

	rw := <-rwc

	first := make(chan error, 1)
	go func() { first <- SendItems(rw, 1) }()
	awaitSignal(t, gate.started, "first write to reach the transport")

	secondReq := &writeRequest{
		msg:  Msg{Code: baseProtocolLength + 2, Size: 0, Payload: bytes.NewReader(nil)},
		done: make(chan error, 1),
	}
	if err := enqueueWriteRequest(peer.writeReq, peer.closed, secondReq, nil); err != nil {
		t.Fatalf("enqueue second request: %v", err)
	}

	closer()
	gate.unblockFirstWrite()

	if err := <-first; err == nil || err == ErrShuttingDown {
		t.Fatalf("first write error: got %v, want non-nil transport error distinct from %v", err, ErrShuttingDown)
	}
	if err := <-secondReq.done; err != ErrShuttingDown {
		t.Fatalf("second write error: got %v, want %v", err, ErrShuttingDown)
	}
}

func TestPeerDisconnectPreservesReasonWhenInflightWriteReturnsNil(t *testing.T) {
	rwc := make(chan MsgReadWriter, 1)
	proto := Protocol{
		Name:   "a",
		Length: 64,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-p.closed
			return nil
		},
	}

	var gate *writeNilGateTransport
	closer, _, peer, errc := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newWriteNilGateTransport(inner)
		return gate
	})
	defer closer()

	rw := <-rwc
	first := make(chan error, 1)
	go func() { first <- SendItems(rw, 1) }()
	awaitSignal(t, gate.started, "first write to reach the transport")

	peer.Disconnect(DiscRequested)
	gate.unblockFirstWrite()

	if err := <-first; err != nil {
		t.Fatalf("first write error: got %v, want nil", err)
	}
	if err := <-errc; err != DiscRequested {
		t.Fatalf("peer run error: got %v, want %v", err, DiscRequested)
	}
}

func TestPeerDisconnectDoesNotHangOnStuckInflightWrite(t *testing.T) {
	origTimeout := inflightWriteDrainTimeout
	inflightWriteDrainTimeout = 50 * time.Millisecond
	defer func() { inflightWriteDrainTimeout = origTimeout }()

	rwc := make(chan MsgReadWriter, 1)
	proto := Protocol{
		Name:   "a",
		Length: 64,
		Run: func(p *Peer, rw MsgReadWriter) error {
			rwc <- rw
			<-p.closed
			return nil
		},
	}

	var gate *stuckWriteTransport
	closer, _, peer, errc := testPeerWithTransport([]Protocol{proto}, func(inner transport) transport {
		gate = newStuckWriteTransport(inner)
		return gate
	})
	defer closer()

	rw := <-rwc
	writeErr := make(chan error, 1)
	go func() { writeErr <- SendItems(rw, 1) }()

	awaitSignal(t, gate.started, "first write to reach stuck transport")
	peer.Disconnect(DiscRequested)

	select {
	case err := <-errc:
		if err != DiscRequested {
			t.Fatalf("peer run error: got %v, want %v", err, DiscRequested)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("peer run did not return while in-flight write was stuck")
	}

	select {
	case err := <-writeErr:
		if err != ErrShuttingDown {
			t.Fatalf("write error: got %v, want %v", err, ErrShuttingDown)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("write call did not return after peer shutdown")
	}
}
