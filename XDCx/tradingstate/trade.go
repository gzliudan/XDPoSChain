package tradingstate

import (
	"math/big"
	"time"

	"github.com/XinFinOrg/XDPoSChain/common"
	"github.com/XinFinOrg/XDPoSChain/crypto/keccak"
)

const (
	TradeStatusSuccess = "SUCCESS"

	TradeTakerOrderHash = "takerOrderHash"
	TradeMakerOrderHash = "makerOrderHash"
	TradeTimestamp      = "timestamp"
	TradeQuantity       = "quantity"
	TradeMakerExchange  = "makerExAddr"
	TradeMaker          = "uAddr"
	TradeBaseToken      = "bToken"
	TradeQuoteToken     = "qToken"
	TradePrice          = "tradedPrice"
	MakerOrderType      = "makerOrderType"
	MakerFee            = "makerFee"
	TakerFee            = "takerFee"
)

type Trade struct {
	Taker          common.Address `json:"taker"`
	Maker          common.Address `json:"maker"`
	BaseToken      common.Address `json:"baseToken"`
	QuoteToken     common.Address `json:"quoteToken"`
	MakerOrderHash common.Hash    `json:"makerOrderHash"`
	TakerOrderHash common.Hash    `json:"takerOrderHash"`
	MakerExchange  common.Address `json:"makerExchange"`
	TakerExchange  common.Address `json:"takerExchange"`
	Hash           common.Hash    `json:"hash"`
	TxHash         common.Hash    `json:"txHash"`
	PricePoint     *big.Int       `json:"pricepoint"`
	Amount         *big.Int       `json:"amount"`
	MakeFee        *big.Int       `json:"makeFee"`
	TakeFee        *big.Int       `json:"takeFee"`
	Status         string         `json:"status"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	TakerOrderSide string         `json:"takerOrderSide"`
	TakerOrderType string         `json:"takerOrderType"`
	MakerOrderType string         `json:"makerOrderType"`
}

// ComputeHash returns hashes the trade
// The OrderHash, Amount, Taker and TradeNonce attributes must be
// set before attempting to compute the trade orderBookHash
func (t *Trade) ComputeHash() common.Hash {
	sha := keccak.NewLegacyKeccak256()
	sha.Write(t.MakerOrderHash.Bytes())
	sha.Write(t.TakerOrderHash.Bytes())
	return common.BytesToHash(sha.Sum(nil))
}
