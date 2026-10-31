package ethtest

import (
	"fmt"
	"io"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/core/types"
	"github.com/XinFinOrg/XDPoSChain/rlp"
)

// getBlockHeadersData represents a block header query payload for eth protocol.
type getBlockHeadersData struct {
	Origin  hashOrNumber
	Amount  uint64
	Skip    uint64
	Reverse bool
}

// blockHeadersData represents a list of block header payloads.
type blockHeadersData []*types.Header

// getBlockBodiesData represents a block body query payload for eth protocol.
type getBlockBodiesData []common.Hash

// blockBody represents a block body payload in response.
type blockBody struct {
	Transactions []*types.Transaction
	Uncles       []*types.Header
}

// blockBodiesData represents a list of block body payloads.
type blockBodiesData []*blockBody

// getReceiptsData represents a receipts query payload for eth protocol.
type getReceiptsData []common.Hash

// receiptsData represents a receipts response list aligned to requested blocks.
type receiptsData []types.Receipts

// getNodeDataPacket represents a node data query payload for eth protocol.
type getNodeDataPacket []common.Hash

// nodeDataPacket represents a node data response list aligned to requested hashes.
type nodeDataPacket [][]byte

// getPooledTransactionsData represents a pooled transactions query payload for
// eth protocol.
type getPooledTransactionsData []common.Hash

// The *66 packet types below are the xdc/166 wrappers around the legacy
// payloads above. Over xdc/166 every request and response of the five
// req/rep pairs is a two-element RLP list [RequestId, legacy-payload], and
// the receiver of a request echoes the request id back on the response. The
// legacy payload is a named exported field rather than an embedded one: the
// rlp package encodes only exported fields, and an embedded unexported type
// name would be silently dropped.

// getBlockHeadersPacket66 is the xdc/166 block header query carrying the
// request id to be echoed back on the response.
type getBlockHeadersPacket66 struct {
	RequestId uint64
	Request   *getBlockHeadersData
}

// blockHeadersPacket66 is the xdc/166 block headers response carrying the
// request id of the answered query.
type blockHeadersPacket66 struct {
	RequestId uint64
	Headers   blockHeadersData
}

// getBlockBodiesPacket66 is the xdc/166 block body query.
type getBlockBodiesPacket66 struct {
	RequestId uint64
	Hashes    getBlockBodiesData
}

// blockBodiesPacket66 is the xdc/166 block bodies response.
type blockBodiesPacket66 struct {
	RequestId uint64
	Bodies    blockBodiesData
}

// getNodeDataPacket66 is the xdc/166 node data query.
type getNodeDataPacket66 struct {
	RequestId uint64
	Hashes    getNodeDataPacket
}

// nodeDataPacket66 is the xdc/166 node data response.
type nodeDataPacket66 struct {
	RequestId uint64
	Data      nodeDataPacket
}

// getReceiptsPacket66 is the xdc/166 receipts query.
type getReceiptsPacket66 struct {
	RequestId uint64
	Hashes    getReceiptsData
}

// receiptsPacket66 is the xdc/166 receipts response.
type receiptsPacket66 struct {
	RequestId uint64
	Receipts  receiptsData
}

// getPooledTransactionsPacket66 is the xdc/166 pooled transactions query.
type getPooledTransactionsPacket66 struct {
	RequestId uint64
	Hashes    getPooledTransactionsData
}

// pooledTransactionsPacket66 is the xdc/166 pooled transactions response.
type pooledTransactionsPacket66 struct {
	RequestId uint64
	Txs       types.Transactions
}

// hashOrNumber is a union for specifying header request origin.
type hashOrNumber struct {
	Hash   common.Hash
	Number uint64
}

// EncodeRLP encodes either hash or number (but never both) for origin.
func (hn *hashOrNumber) EncodeRLP(w io.Writer) error {
	if hn.Hash == (common.Hash{}) {
		return rlp.Encode(w, hn.Number)
	}
	if hn.Number != 0 {
		return fmt.Errorf("both origin hash (%x) and number (%d) provided", hn.Hash, hn.Number)
	}
	return rlp.Encode(w, hn.Hash)
}

// DecodeRLP decodes origin into either hash or number depending on RLP item size.
func (hn *hashOrNumber) DecodeRLP(s *rlp.Stream) error {
	_, size, _ := s.Kind()
	origin, err := s.Raw()
	if err != nil {
		return err
	}
	switch {
	case size == 32:
		return rlp.DecodeBytes(origin, &hn.Hash)
	case size <= 8:
		return rlp.DecodeBytes(origin, &hn.Number)
	default:
		return fmt.Errorf("invalid input size %d for origin", size)
	}
}
