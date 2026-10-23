# XDPoS domain

Vocabulary for the XDPoS chain: the block schedule its validators run to, the block processing steps applied before the first transaction, the chain data those blocks are stored in, and the wire protocol its nodes speak.

## Language

### Chain data

**executed marker**:
A per-block record that this node ran the block itself, written when the block is executed and read together with the state its header names.
_Avoid_: execution flag, sync marker
_See_: [ADR-0001](docs/adr/0001-bound-executed-marker-to-state-lifetime.md)

**state window**:
The recent span of a chain whose state a pruning node still holds; it ends `TriesInMemory` blocks below the head.
_Avoid_: pruning window, retention window
_See_: [ADR-0001](docs/adr/0001-bound-executed-marker-to-state-lifetime.md)

**fork ancestry**:
The depth below the head that the downloader treats as still reorganisable, `MaxForkAncestry` blocks; below it a chain segment is immutable.
_Avoid_: reorg depth, rollback limit
_See_: [ADR-0001](docs/adr/0001-bound-executed-marker-to-state-lifetime.md)

### Block processing

**block level hooks**:
The state changes block processing applies before the first transaction of a block: the TIPSigning removal of the legacy block signers account, the Multicall3 install after Prague whenever the account has no code and the EIP-2935 parent hash write. The DAO fork change is not one of them: only the paths that process or generate a block apply it, each on its own at `DAOForkBlock`, the way upstream `core.PreExecution` leaves it out.
_Avoid_: pre-execution calls, system calls
_See_: [ApplyBlockLevelHooks](core/state_processor.go)

### Wire protocol

**eth capability**:
The devp2p capability XDPoSChain advertises under the name "eth"; the label names the wire protocol, not compatibility with Ethereum's wire format.
_Avoid_: xdc capability
_See_: [ADR-0003](docs/adr/0003-split-eth-wire-layer-into-protocols-eth.md)

**xdc version**:
A wire-protocol version of the eth capability — `xdc165`, `xdc164`, `xdc100` — numbered apart from geth's eth6x series on purpose.
_Avoid_: eth version, protocol version
_See_: [ADR-0003](docs/adr/0003-split-eth-wire-layer-into-protocols-eth.md)

**BFT message**:
One of the three consensus messages the eth capability carries: vote, timeout, and sync info.
_Avoid_: consensus message
_See_: [ADR-0003](docs/adr/0003-split-eth-wire-layer-into-protocols-eth.md)
