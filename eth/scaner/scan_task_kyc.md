# Scan Task: kyc

The `kyc` task scans governance transactions related to `voteInvalidKYC` and emits structured results derived from transaction receipts and logs. It is intended for auditing masternode and KYC invalidation activity.

The task only looks at transactions that match both conditions:

- the destination address is `common.MasternodeVotingSMCBinary`,
- the method selector is `voteInvalidKYC` (`0xf2ee3c7d`).

It then uses **transaction receipts** and **event logs** to determine:

- whether the transaction succeeded,
- whether invalidation actually happened,
- whether the current block author or coinbase candidate is affected,
- whether a normal transaction appears later in the same block.

> Classification is based on receipt logs only. The task does not need to read historical parent state, which makes it safer on pruned or incomplete historical-state setups.

Typical Uses:

- Auditing whether `voteInvalidKYC` calls actually took effect
- Reviewing governance activity across a historical block range
- Checking the relative ordering of special and normal transactions inside the same block

## CLI Flag

```bash
XDC --datadir /path/to/datadir --scan-kyc=[start]:[block-batch]
```

Example:

```bash
XDC --datadir /data/xdc --scan-kyc=1:5000
```

### Parameters

| Field         | Meaning                                         | Default |
| ------------- | ----------------------------------------------- | ------- |
| `start`       | First block to scan                             | 1       |
| `block-batch` | Maximum number of blocks processed in one cycle | 10000   |

Notes:

- Empty fields use defaults, for example `--scan-kyc=5:`.
- `--scan-kyc=` enables the task with default values.

## Output Files

The scan task writes files under the following directory structure:

```text
<datadir>/
└── scan-tasks/
    ├── kyc_result.txt
    └── state/
        ├── kyc_state.json
        └── kyc_error.json
```

### Result file

The main result is written to:

```text
<datadir>/scan-tasks/kyc_result.txt
```

The file starts with a fixed header:

```text
block_number block_hash tx_index tx_hash tx_type tx_succeeded invalidated changes_coinbase_owner has_later_normal_tx
```

Each record then uses the format:

```text
<block_number> <block_hash> <tx_index> <tx_hash> voteInvalidKYC <tx_succeeded> <invalidated> <changes_coinbase_owner> <has_later_normal_tx>
```

Example:

```text
12888 0xabc...def 3 0x123...999 voteInvalidKYC true true false true
```

Fields:

| Field                    | Meaning                                                                             |
| ------------------------ | ----------------------------------------------------------------------------------- |
| `block_number`           | Block number                                                                        |
| `block_hash`             | Block hash                                                                          |
| `tx_index`               | Transaction index within the block                                                  |
| `tx_hash`                | Transaction hash                                                                    |
| `tx_type`                | Currently always `voteInvalidKYC`                                                   |
| `tx_succeeded`           | Whether the receipt status is successful                                            |
| `invalidated`            | Whether an invalidation event was observed in the receipt logs                      |
| `changes_coinbase_owner` | Whether the invalidated set includes the current block author or coinbase candidate |
| `has_later_normal_tx`    | Whether a normal transaction appears later in the same block                        |

### State file

Progress is persisted to:

```text
<datadir>/scan-tasks/state/kyc_state.json
```

Example:

```json
{
  "block_number": 12888,
  "block_hash": "0x...",
  "start_block": 1,
  "time": "2026-04-15 12:34:56"
}
```

The state file uses the format `YYYY-MM-DD HH:MM:SS`.

### Error File

Fatal errors are recorded in:

```text
<datadir>/scan-tasks/state/kyc_error.json
```

Example:

```json
{
  "task": "kyc",
  "time": "2026-04-15 12:34:56",
  "error": "incomplete receipts for kyc scan at block 12888: txs=3 receipts=2",
  "block_number": 12888,
  "block_hash": "0x..."
}
```

## Behavior Notes

### 1. Confirmed blocks only

Like the other scan task, `kyc` only processes blocks that have reached the configured confirmation threshold.

### 2. Pre-activation blocks are skipped for classification

Before the owner-fee-routing activation point, the task still advances its progress but does not emit KYC classification results.

### 3. Complete receipts are required

If the number of receipts does not match the number of block transactions, the task stops with an error instead of producing a partial or misleading result.

### 4. Rewind and resume are supported

When the chain reorganizes, output is missing, or the configured start block moves backward, the task can:

- rewind to a safe canonical height,
- truncate stale output lines,
- rebuild the result from the canonical chain.

## Troubleshooting

If startup fails with `corrupted scan task state file` and points to `kyc_state.json`:

- Step 1: Check `<datadir>/scan-tasks/state/kyc_error.json` for the exact parse error.
- Step 2: Back up both files:
  - `<datadir>/scan-tasks/state/kyc_state.json`
  - `<datadir>/scan-tasks/kyc_result.txt`
- Step 3: Remove only `kyc_state.json` and restart the node.
- Step 4: Verify logs show kyc task reinitialization and forward progress.

Optional manual resume:

- Instead of deleting the state file, provide a valid `kyc_state.json` with a known canonical `block_number` and `block_hash`.
