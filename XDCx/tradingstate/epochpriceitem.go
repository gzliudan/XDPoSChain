package tradingstate

import (
	"math/big"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/crypto"
)

type EpochPriceItem struct {
	Epoch     uint64      `json:"epoch"`
	Orderbook common.Hash `json:"orderbook"`
	Hash      common.Hash `json:"hash"`
	Price     *big.Int    `json:"price"`
}

func (item *EpochPriceItem) ComputeHash() common.Hash {
	return crypto.Keccak256Hash(new(big.Int).SetUint64(item.Epoch).Bytes(), item.Orderbook.Bytes())
}
