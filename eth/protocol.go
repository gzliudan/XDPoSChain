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

package eth

import (
	"fmt"
	"io"
	"math/big"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core"
	"github.com/XinFinOrg/XDPoSChain/core/forkid"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/event"
	"github.com/XinFinOrg/XDPoSChain/rlp"
)

// Constants to match up protocol versions and messages
const (
	eth63   = 63
	eth64   = 64
	eth65   = 65
	xdpos2  = 100 //xdpos2.1 = eth62+eth63
	xdpos22 = 101 //xdpos2.2 = eth65
)

// XDC needs the below functions because direct number equality doesn't work (eg. version >= 63)
// we should try to match protocols 1 to 1 from now on, bump xdpos along with any new eth (eg. eth66 = xdpos23 only)
// try to follow the exact comparison from go-ethereum as much as possible (eg. version >= 63 <> isEth63OrHigher(version))

func isEth63(version int) bool {
	switch {
	case version == 63:
		return true
	case version == 100:
		return true
	default:
		return false
	}
}
func isEth64(version int) bool {
	switch {
	case version == 64:
		return true
	default:
		return false
	}
}
func isEth65(version int) bool {
	switch {
	case version == 65:
		return true
	case version == 101:
		return true
	default:
		return false
	}
}

func isEth63OrHigher(version int) bool {
	return isEth63(version) || isEth64(version) || isEth65(version)
}

func isEth64OrHigher(version int) bool {
	return isEth64(version) || isEth65(version)
}

func isEth65OrHigher(version int) bool {
	return isEth65(version)
}

// protocolName is the official short name of the protocol used during capability negotiation.
const protocolName = "eth"

// ProtocolVersions are the supported versions of the eth protocol (first is primary).
var ProtocolVersions = []uint{xdpos22, xdpos2, eth65, eth64, eth63}

// protocolLengths are the number of implemented message corresponding to different protocol versions.
var protocolLengths = map[uint]uint64{xdpos22: 227, xdpos2: 227, eth65: 17, eth64: 17, eth63: 17}

const protocolMaxMsgSize = 10 * 1024 * 1024 // Maximum cap on the size of a protocol message

// eth protocol message codes
const (
	StatusMsg          = 0x00
	NewBlockHashesMsg  = 0x01
	TransactionMsg     = 0x02
	GetBlockHeadersMsg = 0x03
	BlockHeadersMsg    = 0x04
	GetBlockBodiesMsg  = 0x05
	BlockBodiesMsg     = 0x06
	NewBlockMsg        = 0x07
	OrderTxMsg         = 0x08
	LendingTxMsg       = 0x09
	// Protocol messages belonging to eth/63
	GetNodeDataMsg = 0x0d
	NodeDataMsg    = 0x0e
	GetReceiptsMsg = 0x0f
	ReceiptsMsg    = 0x10

	// New protocol message codes introduced in eth65
	//
	// Previously these message ids were used by some legacy and unsupported
	// eth protocols, reown them here.
	NewPooledTransactionHashesMsg = 0x28 //originally 0x08 but clash with OrderTxMsg
	GetPooledTransactionsMsg      = 0x29 //originally 0x09 but clash with LendingTxMsg
	PooledTransactionsMsg         = 0x0a

	// Protocol messages belonging to xdpos2/100
	VoteMsg     = 0xe0
	TimeoutMsg  = 0xe1
	SyncInfoMsg = 0xe2
)

type errCode int

const (
	ErrMsgTooLarge = iota
	ErrDecode
	ErrInvalidMsgCode
	ErrProtocolVersionMismatch
	ErrNetworkIDMismatch
	ErrGenesisMismatch
	ErrForkIDRejected
	ErrNoStatusMsg
	ErrExtraStatusMsg
)

func (e errCode) String() string {
	return errorToString[int(e)]
}

// XXX change once legacy code is out
var errorToString = map[int]string{
	ErrMsgTooLarge:             "Message too long",
	ErrDecode:                  "Invalid message",
	ErrInvalidMsgCode:          "Invalid message code",
	ErrProtocolVersionMismatch: "Protocol version mismatch",
	ErrNetworkIDMismatch:       "Network ID mismatch",
	ErrGenesisMismatch:         "Genesis mismatch",
	ErrForkIDRejected:          "Fork ID rejected",
	ErrNoStatusMsg:             "No status message",
	ErrExtraStatusMsg:          "Extra status message",
}

type txPool interface {
	// Has returns an indicator whether txpool has a transaction
	// cached with the given hash.
	Has(hash common.Hash) bool

	// Get retrieves the transaction from local txpool with given
	// tx hash.
	Get(hash common.Hash) *types.Transaction

	// AddRemotes should add the given transactions to the pool.
	AddRemotes([]*types.Transaction) []error

	// Pending should return pending transactions.
	// The slice should be modifiable by the caller.
	Pending() (map[common.Address]types.Transactions, error)

	// SubscribeNewTxsEvent should return an event subscription of
	// NewTxsEvent and send events to the given channel.
	SubscribeNewTxsEvent(chan<- core.NewTxsEvent) event.Subscription
}

type orderPool interface {
	// AddRemotes should add the given transactions to the pool.
	AddRemotes([]*types.OrderTransaction) []error

	// Pending should return pending transactions.
	// The slice should be modifiable by the caller.
	Pending() (map[common.Address]types.OrderTransactions, error)

	// SubscribeTxPreEvent should return an event subscription of
	// TxPreEvent and send events to the given channel.
	SubscribeTxPreEvent(chan<- core.OrderTxPreEvent) event.Subscription
}

type lendingPool interface {
	// AddRemotes should add the given transactions to the pool.
	AddRemotes([]*types.LendingTransaction) []error

	// Pending should return pending transactions.
	// The slice should be modifiable by the caller.
	Pending() (map[common.Address]types.LendingTransactions, error)

	// SubscribeTxPreEvent should return an event subscription of
	// TxPreEvent and send events to the given channel.
	SubscribeTxPreEvent(chan<- core.LendingTxPreEvent) event.Subscription
}

// statusData63 is the network packet for the status message for eth/63.
type statusData63 struct {
	ProtocolVersion uint32
	NetworkId       uint64
	TD              *big.Int
	CurrentBlock    common.Hash
	GenesisBlock    common.Hash
}

// statusData is the network packet for the status message for eth/64 and later.
type statusData struct {
	ProtocolVersion uint32
	NetworkID       uint64
	TD              *big.Int
	Head            common.Hash
	Genesis         common.Hash
	ForkID          forkid.ID
}

// newBlockHashesData is the network packet for the block announcements.
type newBlockHashesData []struct {
	Hash   common.Hash // Hash of one particular block being announced
	Number uint64      // Number of one particular block being announced
}

// getBlockHeadersData represents a block header query.
type getBlockHeadersData struct {
	Origin  hashOrNumber // Block from which to retrieve headers
	Amount  uint64       // Maximum number of headers to retrieve
	Skip    uint64       // Blocks to skip between consecutive headers
	Reverse bool         // Query direction (false = rising towards latest, true = falling towards genesis)
}

// hashOrNumber is a combined field for specifying an origin block.
type hashOrNumber struct {
	Hash   common.Hash // Block hash from which to retrieve headers (excludes Number)
	Number uint64      // Block hash from which to retrieve headers (excludes Hash)
}

// EncodeRLP is a specialized encoder for hashOrNumber to encode only one of the
// two contained union fields.
func (hn *hashOrNumber) EncodeRLP(w io.Writer) error {
	if hn.Hash == (common.Hash{}) {
		return rlp.Encode(w, hn.Number)
	}
	if hn.Number != 0 {
		return fmt.Errorf("both origin hash (%x) and number (%d) provided", hn.Hash, hn.Number)
	}
	return rlp.Encode(w, hn.Hash)
}

// DecodeRLP is a specialized decoder for hashOrNumber to decode the contents
// into either a block hash or a block number.
func (hn *hashOrNumber) DecodeRLP(s *rlp.Stream) error {
	_, size, _ := s.Kind()
	origin, err := s.Raw()
	if err == nil {
		switch {
		case size == 32:
			err = rlp.DecodeBytes(origin, &hn.Hash)
		case size <= 8:
			err = rlp.DecodeBytes(origin, &hn.Number)
		default:
			err = fmt.Errorf("invalid input size %d for origin", size)
		}
	}
	return err
}

// newBlockData is the network packet for the block propagation message.
type newBlockData struct {
	Block *types.Block
	TD    *big.Int
}

// sanityCheck verifies that the values are reasonable, as a DoS protection
func (request *newBlockData) sanityCheck() error {
	if err := request.Block.SanityCheck(); err != nil {
		return err
	}
	//TD at mainnet block #7753254 is 76 bits. If it becomes 100 million times
	// larger, it will still fit within 100 bits
	if tdlen := request.TD.BitLen(); tdlen > 100 {
		return fmt.Errorf("too large block TD: bitlen %d", tdlen)
	}
	return nil
}

// blockBody represents the data content of a single block.
type blockBody struct {
	Transactions []*types.Transaction // Transactions contained within a block
	Uncles       []*types.Header      // Uncles contained within a block
}

// blockBodiesData is the network packet for block content distribution.
type blockBodiesData []*blockBody
