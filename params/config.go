// Copyright 2016 The go-ethereum Authors
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

package params

import (
	"fmt"
	"math/big"

	"github.com/XinFinOrg/XDPoSChain/common"
)

const (
	ConsensusEngineVersion1 = "v1"
	ConsensusEngineVersion2 = "v2"
)

var (
	XDCMainnetGenesisHash = common.HexToHash("9326145f8a2c8c00bbe13afc7d7f3d9c868b5ef39d89f2f4e9390e9720298624") // XDC Mainnet genesis hash to enforce below configs on
	MainnetGenesisHash    = common.HexToHash("8d13370621558f4ed0da587934473c0404729f28b0ff1d50e5fdd840457a2f17") // Mainnet genesis hash to enforce below configs on
	TestnetGenesisHash    = common.HexToHash("dffc8ae3b45965404b4fd73ce7f0e13e822ac0fc23ce7e95b42bc5f1e57023a5") // Testnet genesis hash to enforce below configs on
)

var (
	XDPoSV2Config = &V2{
		TimeoutWorkerDuration: 50,
		CertThreshold:         common.MaxMasternodesV2*2/3 + 1,
	}
	TestXDPoSV2Config = &V2{
		TimeoutWorkerDuration: 10,
		CertThreshold:         3,
		WaitPeriod:            1,
		MinePeriod:            2,
		SwitchBlock:           big.NewInt(900),
	}
	DevnetXDPoSV2Config = &V2{
		SwitchBlock:           big.NewInt(9999999), // Temporary set it to very high
		TimeoutWorkerDuration: 50,
		CertThreshold:         6,
		WaitPeriod:            2,
		MinePeriod:            10,
	}

	// XDPoSChain mainnet config
	XDCMainnetChainConfig = &ChainConfig{
		ChainId:        big.NewInt(88),
		HomesteadBlock: big.NewInt(1),
		EIP150Block:    big.NewInt(2),
		EIP150Hash:     common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000"),
		EIP155Block:    big.NewInt(3),
		EIP158Block:    big.NewInt(3),
		ByzantiumBlock: big.NewInt(4),
		XDPoS: &XDPoSConfig{
			Period:              2,
			Epoch:               900,
			Reward:              250,
			RewardCheckpoint:    900,
			Gap:                 5,
			FoudationWalletAddr: common.HexToAddress("0x0000000000000000000000000000000000000068"),
			V2:                  XDPoSV2Config,
		},
	}

	// MainnetChainConfig is the chain parameters to run a node on the main network.
	MainnetChainConfig = &ChainConfig{
		ChainId:             big.NewInt(1),
		HomesteadBlock:      big.NewInt(1150000),
		DAOForkBlock:        big.NewInt(1920000),
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(2463000),
		EIP150Hash:          common.HexToHash("0x2086799aeebeae135c246c65021c82b4e15a2c451340993aacfd2751886514f0"),
		EIP155Block:         big.NewInt(2675000),
		EIP158Block:         big.NewInt(2675000),
		ByzantiumBlock:      big.NewInt(4370000),
		ConstantinopleBlock: nil,
		Ethash:              new(EthashConfig),
	}

	// TestnetChainConfig contains the chain parameters to run a node on the Ropsten test network.
	TestnetChainConfig = &ChainConfig{
		ChainId:             big.NewInt(3),
		HomesteadBlock:      big.NewInt(0),
		DAOForkBlock:        nil,
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(0),
		EIP150Hash:          common.HexToHash("0x62e0fde86e34c263e250fbcd5ca4598ba8ca10a1d166c8526bb127e10b313311"),
		EIP155Block:         big.NewInt(10),
		EIP158Block:         big.NewInt(10),
		ByzantiumBlock:      big.NewInt(1700000),
		ConstantinopleBlock: nil,
		Ethash:              new(EthashConfig),
	}

	// RinkebyChainConfig contains the chain parameters to run a node on the Rinkeby test network.
	RinkebyChainConfig = &ChainConfig{
		ChainId:             big.NewInt(4),
		HomesteadBlock:      big.NewInt(1),
		DAOForkBlock:        nil,
		DAOForkSupport:      true,
		EIP150Block:         big.NewInt(2),
		EIP150Hash:          common.HexToHash("0x9b095b36c15eaf13044373aef8ee0bd3a382a5abb92e402afa44b8249c3a90e9"),
		EIP155Block:         big.NewInt(3),
		EIP158Block:         big.NewInt(3),
		ByzantiumBlock:      big.NewInt(1035301),
		ConstantinopleBlock: nil,
		XDPoS: &XDPoSConfig{
			Period: 15,
			Epoch:  30000,
			V2:     XDPoSV2Config,
		},
	}

	// AllEthashProtocolChanges contains every protocol change (EIPs) introduced
	// and accepted by the Ethereum core developers into the Ethash consensus.
	//
	// This configuration is intentionally not using keyed fields to force anyone
	// adding flags to the config to also have to set these fields.
	AllEthashProtocolChanges = &ChainConfig{big.NewInt(1337), big.NewInt(0), nil, false, big.NewInt(0), common.Hash{}, big.NewInt(0), big.NewInt(0), big.NewInt(0), nil, new(EthashConfig), nil, nil}

	// AllXDPoSProtocolChanges contains every protocol change (EIPs) introduced
	// and accepted by the Ethereum core developers into the XDPoS consensus.
	//
	// This configuration is intentionally not using keyed fields to force anyone
	// adding flags to the config to also have to set these fields.
	AllXDPoSProtocolChanges  = &ChainConfig{big.NewInt(89), big.NewInt(0), nil, false, big.NewInt(0), common.Hash{}, big.NewInt(0), big.NewInt(0), big.NewInt(0), nil, nil, nil, &XDPoSConfig{Period: 0, Epoch: 30000}}
	AllCliqueProtocolChanges = &ChainConfig{big.NewInt(1337), big.NewInt(0), nil, false, big.NewInt(0), common.Hash{}, big.NewInt(0), big.NewInt(0), big.NewInt(0), nil, nil, &CliqueConfig{Period: 0, Epoch: 30000}, nil}

	// XDPoS config with v2 engine after block 901
	TestXDPoSMockChainConfig = &ChainConfig{big.NewInt(1337), big.NewInt(0), nil, false, big.NewInt(0), common.Hash{}, big.NewInt(0), big.NewInt(0), big.NewInt(0), nil, new(EthashConfig), nil, &XDPoSConfig{Epoch: 900, Gap: 450, SkipValidation: true, V2: TestXDPoSV2Config}}

	TestChainConfig = &ChainConfig{big.NewInt(1), big.NewInt(0), nil, false, big.NewInt(0), common.Hash{}, big.NewInt(0), big.NewInt(0), big.NewInt(0), nil, new(EthashConfig), nil, nil}
	TestRules       = TestChainConfig.Rules(new(big.Int))
)

// ChainConfig is the core config which determines the blockchain settings.
//
// ChainConfig is stored in the database on a per block basis. This means
// that any network, identified by its genesis block, can have its own
// set of configuration options.
type ChainConfig struct {
	ChainId *big.Int `json:"chainId"` // Chain id identifies the current chain and is used for replay protection

	HomesteadBlock *big.Int `json:"homesteadBlock,omitempty"` // Homestead switch block (nil = no fork, 0 = already homestead)

	DAOForkBlock   *big.Int `json:"daoForkBlock,omitempty"`   // TheDAO hard-fork switch block (nil = no fork)
	DAOForkSupport bool     `json:"daoForkSupport,omitempty"` // Whether the nodes supports or opposes the DAO hard-fork

	// EIP150 implements the Gas price changes (https://github.com/ethereum/EIPs/issues/150)
	EIP150Block *big.Int    `json:"eip150Block,omitempty"` // EIP150 HF block (nil = no fork)
	EIP150Hash  common.Hash `json:"eip150Hash,omitempty"`  // EIP150 HF hash (needed for header only clients as only gas pricing changed)

	EIP155Block *big.Int `json:"eip155Block,omitempty"` // EIP155 HF block
	EIP158Block *big.Int `json:"eip158Block,omitempty"` // EIP158 HF block

	ByzantiumBlock      *big.Int `json:"byzantiumBlock,omitempty"`      // Byzantium switch block (nil = no fork, 0 = already on byzantium)
	ConstantinopleBlock *big.Int `json:"constantinopleBlock,omitempty"` // Constantinople switch block (nil = no fork, 0 = already activated)

	// Various consensus engines
	Ethash *EthashConfig `json:"ethash,omitempty"`
	Clique *CliqueConfig `json:"clique,omitempty"`
	XDPoS  *XDPoSConfig  `json:"XDPoS,omitempty"`
}

// EthashConfig is the consensus engine configs for proof-of-work based sealing.
type EthashConfig struct{}

// String implements the stringer interface, returning the consensus engine details.
func (c *EthashConfig) String() string {
	return "ethash"
}

// CliqueConfig is the consensus engine configs for proof-of-authority based sealing.
type CliqueConfig struct {
	Period uint64 `json:"period"` // Number of seconds between blocks to enforce
	Epoch  uint64 `json:"epoch"`  // Epoch length to reset votes and checkpoint
}

// String implements the stringer interface, returning the consensus engine details.
func (c *CliqueConfig) String() string {
	return "clique"
}

// XDPoSConfig is the consensus engine configs for delegated-proof-of-stake based sealing.
type XDPoSConfig struct {
	Period              uint64         `json:"period"`              // Number of seconds between blocks to enforce
	Epoch               uint64         `json:"epoch"`               // Epoch length to reset votes and checkpoint
	Reward              uint64         `json:"reward"`              // Block reward - unit Ether
	RewardCheckpoint    uint64         `json:"rewardCheckpoint"`    // Checkpoint block for calculate rewards.
	Gap                 uint64         `json:"gap"`                 // Gap time preparing for the next epoch
	FoudationWalletAddr common.Address `json:"foudationWalletAddr"` // Foundation Address Wallet
	WaitPeriod          int            `json:"waitPeriod"`          // Miner wait period
	SkipValidation      bool           //Skip Block Validation for testing purpose
	V2                  *V2            `json:"v2"`
}

type V2 struct {
	WaitPeriod            int      `json:"waitPeriod"`            // Miner wait period to check mine event
	MinePeriod            int      `json:"minePeriod"`            // Miner mine period to mine a block
	SwitchBlock           *big.Int `json:"switchBlock"`           // v1 to v2 switch block number
	TimeoutWorkerDuration int64    `json:"timeoutWorkerDuration"` // Duration in ms
	CertThreshold         int      `json:"certificateThreshold"`  // Necessary number of messages from master nodes to form a certificate
}

// String implements the stringer interface, returning the consensus engine details.
func (c *XDPoSConfig) String() string {
	return "XDPoS"
}

/**
ConsensusVersion will return the consensus version to use for the provided block number. The returned int represent its version
TODO: It's a dummy value for now until the 2.0 consensus engine is fully implemented.
*/
func (c *XDPoSConfig) BlockConsensusVersion(num *big.Int) string {
	if c.V2 != nil && c.V2.SwitchBlock != nil && num.Cmp(c.V2.SwitchBlock) > 0 {
		return ConsensusEngineVersion2
	}
	return ConsensusEngineVersion1
}

// String implements the fmt.Stringer interface.
func (c *ChainConfig) String() string {
	var engine interface{}
	switch {
	case c.Ethash != nil:
		engine = c.Ethash
	case c.XDPoS != nil:
		engine = c.XDPoS
	default:
		engine = "unknown"
	}
	return fmt.Sprintf("{ChainID: %v Homestead: %v DAO: %v DAOSupport: %v EIP150: %v EIP155: %v EIP158: %v Byzantium: %v Constantinople: %v Engine: %v}",
		c.ChainId,
		c.HomesteadBlock,
		c.DAOForkBlock,
		c.DAOForkSupport,
		c.EIP150Block,
		c.EIP155Block,
		c.EIP158Block,
		c.ByzantiumBlock,
		c.ConstantinopleBlock,
		engine,
	)
}

// IsHomestead returns whether num is either equal to the homestead block or greater.
func (c *ChainConfig) IsHomestead(num *big.Int) bool {
	return isForked(c.HomesteadBlock, num)
}

// IsDAO returns whether num is either equal to the DAO fork block or greater.
func (c *ChainConfig) IsDAOFork(num *big.Int) bool {
	return isForked(c.DAOForkBlock, num)
}

func (c *ChainConfig) IsEIP150(num *big.Int) bool {
	return isForked(c.EIP150Block, num)
}

func (c *ChainConfig) IsEIP155(num *big.Int) bool {
	return isForked(c.EIP155Block, num)
}

func (c *ChainConfig) IsEIP158(num *big.Int) bool {
	return isForked(c.EIP158Block, num)
}

func (c *ChainConfig) IsByzantium(num *big.Int) bool {
	return isForked(c.ByzantiumBlock, num)
}

func (c *ChainConfig) IsConstantinople(num *big.Int) bool {
	return isForked(c.ConstantinopleBlock, num)
}

// IsPetersburg returns whether num is either
// - equal to or greater than the PetersburgBlock fork block,
// - OR is nil, and Constantinople is active
func (c *ChainConfig) IsPetersburg(num *big.Int) bool {
	return isForked(common.TIPXDCXCancellationFee, num)
}

// IsIstanbul returns whether num is either equal to the Istanbul fork block or greater.
func (c *ChainConfig) IsIstanbul(num *big.Int) bool {
	return isForked(common.TIPXDCXCancellationFee, num)
}

func (c *ChainConfig) IsTIP2019(num *big.Int) bool {
	return isForked(common.TIP2019Block, num)
}

func (c *ChainConfig) IsTIPSigning(num *big.Int) bool {
	return isForked(common.TIPSigning, num)
}

func (c *ChainConfig) IsTIPRandomize(num *big.Int) bool {
	return isForked(common.TIPRandomize, num)
}

// IsTIPIncreaseMasternodes using for increase masternodes from 18 to 40

// Time update: 23-07-2019
func (c *ChainConfig) IsTIPIncreaseMasternodes(num *big.Int) bool {
	return isForked(common.TIPIncreaseMasternodes, num)
}

func (c *ChainConfig) IsTIPNoHalvingMNReward(num *big.Int) bool {
	return isForked(common.TIPNoHalvingMNReward, num)
}
func (c *ChainConfig) IsTIPXDCX(num *big.Int) bool {
	if common.IsTestnet {
		return isForked(common.TIPXDCXTestnet, num)
	} else {
		return isForked(common.TIPXDCX, num)
	}
}

func (c *ChainConfig) IsTIPXDCXLending(num *big.Int) bool {
	return isForked(common.TIPXDCXLending, num)
}

func (c *ChainConfig) IsTIPXDCXCancellationFee(num *big.Int) bool {
	return isForked(common.TIPXDCXCancellationFee, num)
}

// GasTable returns the gas table corresponding to the current phase (homestead or homestead reprice).
//
// The returned GasTable's fields shouldn't, under any circumstances, be changed.
func (c *ChainConfig) GasTable(num *big.Int) GasTable {
	if num == nil {
		return GasTableHomestead
	}
	switch {
	case c.IsEIP158(num):
		return GasTableEIP158
	case c.IsEIP150(num):
		return GasTableEIP150
	default:
		return GasTableHomestead
	}
}

// CheckCompatible checks whether scheduled fork transitions have been imported
// with a mismatching chain configuration.
func (c *ChainConfig) CheckCompatible(newcfg *ChainConfig, height uint64) *ConfigCompatError {
	bhead := new(big.Int).SetUint64(height)

	// Iterate checkCompatible to find the lowest conflict.
	var lasterr *ConfigCompatError
	for {
		err := c.checkCompatible(newcfg, bhead)
		if err == nil || (lasterr != nil && err.RewindTo == lasterr.RewindTo) {
			break
		}
		lasterr = err
		bhead.SetUint64(err.RewindTo)
	}
	return lasterr
}

func (c *ChainConfig) checkCompatible(newcfg *ChainConfig, head *big.Int) *ConfigCompatError {
	if isForkIncompatible(c.HomesteadBlock, newcfg.HomesteadBlock, head) {
		return newCompatError("Homestead fork block", c.HomesteadBlock, newcfg.HomesteadBlock)
	}
	if isForkIncompatible(c.DAOForkBlock, newcfg.DAOForkBlock, head) {
		return newCompatError("DAO fork block", c.DAOForkBlock, newcfg.DAOForkBlock)
	}
	if c.IsDAOFork(head) && c.DAOForkSupport != newcfg.DAOForkSupport {
		return newCompatError("DAO fork support flag", c.DAOForkBlock, newcfg.DAOForkBlock)
	}
	if isForkIncompatible(c.EIP150Block, newcfg.EIP150Block, head) {
		return newCompatError("EIP150 fork block", c.EIP150Block, newcfg.EIP150Block)
	}
	if isForkIncompatible(c.EIP155Block, newcfg.EIP155Block, head) {
		return newCompatError("EIP155 fork block", c.EIP155Block, newcfg.EIP155Block)
	}
	if isForkIncompatible(c.EIP158Block, newcfg.EIP158Block, head) {
		return newCompatError("EIP158 fork block", c.EIP158Block, newcfg.EIP158Block)
	}
	if c.IsEIP158(head) && !configNumEqual(c.ChainId, newcfg.ChainId) {
		return newCompatError("EIP158 chain ID", c.EIP158Block, newcfg.EIP158Block)
	}
	if isForkIncompatible(c.ByzantiumBlock, newcfg.ByzantiumBlock, head) {
		return newCompatError("Byzantium fork block", c.ByzantiumBlock, newcfg.ByzantiumBlock)
	}
	if isForkIncompatible(c.ConstantinopleBlock, newcfg.ConstantinopleBlock, head) {
		return newCompatError("Constantinople fork block", c.ConstantinopleBlock, newcfg.ConstantinopleBlock)
	}
	return nil
}

// isForkIncompatible returns true if a fork scheduled at s1 cannot be rescheduled to
// block s2 because head is already past the fork.
func isForkIncompatible(s1, s2, head *big.Int) bool {
	return (isForked(s1, head) || isForked(s2, head)) && !configNumEqual(s1, s2)
}

// isForked returns whether a fork scheduled at block s is active at the given head block.
func isForked(s, head *big.Int) bool {
	if s == nil || head == nil {
		return false
	}
	return s.Cmp(head) <= 0
}

func configNumEqual(x, y *big.Int) bool {
	if x == nil {
		return y == nil
	}
	if y == nil {
		return x == nil
	}
	return x.Cmp(y) == 0
}

// ConfigCompatError is raised if the locally-stored blockchain is initialised with a
// ChainConfig that would alter the past.
type ConfigCompatError struct {
	What string
	// block numbers of the stored and new configurations
	StoredConfig, NewConfig *big.Int
	// the block number to which the local chain must be rewound to correct the error
	RewindTo uint64
}

func newCompatError(what string, storedblock, newblock *big.Int) *ConfigCompatError {
	var rew *big.Int
	switch {
	case storedblock == nil:
		rew = newblock
	case newblock == nil || storedblock.Cmp(newblock) < 0:
		rew = storedblock
	default:
		rew = newblock
	}
	err := &ConfigCompatError{what, storedblock, newblock, 0}
	if rew != nil && rew.Sign() > 0 {
		err.RewindTo = rew.Uint64() - 1
	}
	return err
}

func (err *ConfigCompatError) Error() string {
	return fmt.Sprintf("mismatching %s in database (have %d, want %d, rewindto %d)", err.What, err.StoredConfig, err.NewConfig, err.RewindTo)
}

// Rules wraps ChainConfig and is merely syntatic sugar or can be used for functions
// that do not have or require information about the block.
//
// Rules is a one time interface meaning that it shouldn't be used in between transition
// phases.
type Rules struct {
	ChainId                                                 *big.Int
	IsHomestead, IsEIP150, IsEIP155, IsEIP158               bool
	IsByzantium, IsConstantinople, IsPetersburg, IsIstanbul bool
}

func (c *ChainConfig) Rules(num *big.Int) Rules {
	chainId := c.ChainId
	if chainId == nil {
		chainId = new(big.Int)
	}
	return Rules{
		ChainId:          new(big.Int).Set(chainId),
		IsHomestead:      c.IsHomestead(num),
		IsEIP150:         c.IsEIP150(num),
		IsEIP155:         c.IsEIP155(num),
		IsEIP158:         c.IsEIP158(num),
		IsByzantium:      c.IsByzantium(num),
		IsConstantinople: c.IsConstantinople(num),
		IsPetersburg:     c.IsPetersburg(num),
		IsIstanbul:       c.IsIstanbul(num),
	}
}
