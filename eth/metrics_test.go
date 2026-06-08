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

package eth

import (
	"io"
	"testing"

	"github.com/XinFinOrg/XDPoSChain/p2p"
)

// priorityRecordingRW records whether the priority lane was used for the
// last WriteMsg* call on it.
type priorityRecordingRW struct {
	last     p2p.Msg
	high     bool
	priority bool // true if WriteMsgPriority was called
}

func (rw *priorityRecordingRW) ReadMsg() (p2p.Msg, error) { return p2p.Msg{}, io.EOF }

func (rw *priorityRecordingRW) WriteMsg(msg p2p.Msg) error {
	rw.last = msg
	rw.priority = false
	return nil
}

func (rw *priorityRecordingRW) WriteMsgPriority(msg p2p.Msg, high bool) error {
	rw.last = msg
	rw.high = high
	rw.priority = true
	return nil
}

// TestMeteredMsgReadWriterForwardsPriority verifies that meteredMsgReadWriter
// implements p2p.PriorityMsgWriter and forwards priority writes to the
// underlying writer's high-priority lane. Without this, BFT consensus
// messages (VoteMsg/TimeoutMsg/SyncInfoMsg) would silently fall back to the
// normal lane on nodes that have metrics enabled.
func TestMeteredMsgReadWriterForwardsPriority(t *testing.T) {
	inner := &priorityRecordingRW{}
	mrw := &meteredMsgReadWriter{MsgReadWriter: inner}
	mrw.Init(eth63)

	if _, ok := p2p.MsgReadWriter(mrw).(p2p.PriorityMsgWriter); !ok {
		t.Fatal("meteredMsgReadWriter does not implement PriorityMsgWriter")
	}

	if err := p2p.SendPriority(mrw, VoteMsg, []uint{}); err != nil {
		t.Fatalf("SendPriority: %v", err)
	}
	if !inner.priority {
		t.Fatal("priority lane was not used on inner writer")
	}
	if !inner.high {
		t.Fatal("high flag was not propagated to inner writer")
	}
	if inner.last.Code != VoteMsg {
		t.Fatalf("code: got %d, want %d", inner.last.Code, VoteMsg)
	}
}

// nonPriorityRW only implements p2p.MsgReadWriter and is used to verify the
// fallback path in meteredMsgReadWriter.WriteMsgPriority.
type nonPriorityRW struct {
	last p2p.Msg
}

func (rw *nonPriorityRW) ReadMsg() (p2p.Msg, error) { return p2p.Msg{}, io.EOF }
func (rw *nonPriorityRW) WriteMsg(msg p2p.Msg) error {
	rw.last = msg
	return nil
}

func TestMeteredMsgReadWriterPriorityFallback(t *testing.T) {
	inner := &nonPriorityRW{}
	mrw := &meteredMsgReadWriter{MsgReadWriter: inner}
	mrw.Init(eth63)

	if err := mrw.WriteMsgPriority(p2p.Msg{Code: VoteMsg}, true); err != nil {
		t.Fatalf("WriteMsgPriority: %v", err)
	}
	if inner.last.Code != VoteMsg {
		t.Fatalf("code: got %d, want %d", inner.last.Code, VoteMsg)
	}
}
