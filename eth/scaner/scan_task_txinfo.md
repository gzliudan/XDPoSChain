# Scan Task: txinfo

The `txinfo` task scans confirmed blocks in order and exports a minimal transaction index for offline lookup and downstream processing.

The task scans all transactions in confirmed blocks and records:

- the transaction hash,
- the recipient address, or `n` for contract creation,
- the block number.

Typical Uses:

- Building a lightweight `tx_hash -> to -> block` index
- Producing input data for ETL or audit scripts
- Inspecting coarse transaction distribution across historical ranges

## CLI Flag

```bash
XDC --datadir /path/to/datadir --scan-txinfo=[start]:[block-batch]:[tx-batch]
```

Example:

```bash
XDC --datadir /data/xdc --scan-txinfo=1:2000:20000
```

### Parameters

| Field         | Meaning                                                | Default            |
| ------------- | ------------------------------------------------------ | ------------------ |
| `start`       | First block to scan                                    | 1                  |
| `block-batch` | Maximum number of blocks processed in one cycle        | 10000              |
| `tx-batch`    | Soft cap for total transactions processed in one cycle | `block-batch * 10` |

Notes:

- Empty fields fall back to defaults, for example `--scan-txinfo=:1000`.
- The transaction limit is a soft cap. The task never splits a block in half.
- `--scan-txinfo=` enables the task with all default values.

## Output Files

The scan task writes files under the following directory structure:

```text
<datadir>/
└── scan-tasks/
    ├── txinfo_result.txt
    └── state/
        ├── txinfo_state.json
        └── txinfo_error.json
```

### Result file

The main result is written to:

```text
<datadir>/scan-tasks/txinfo_result.txt
```

Each line uses the format:

```text
<tx_hash> <to> <block_number>
```

Example:

```text
0x7f...12 0xabc...999 12345
0x8e...77 n 12345
```

Fields:

| Field          | Meaning                                                   |
| -------------- | --------------------------------------------------------- |
| `tx_hash`      | Transaction hash                                          |
| `to`           | Recipient address; `n` for contract-creation transactions |
| `block_number` | Block number containing the transaction                   |

### State file

Progress is persisted to:

```text
<datadir>/scan-tasks/state/txinfo_state.json
```

Example:

```json
{
  "block_number": 12345,
  "block_hash": "0x...",
  "start_block": 1,
  "time": "2026-04-15 12:34:56"
}
```

The state file uses the format `YYYY-MM-DD HH:MM:SS`.

### Error File

Fatal errors are recorded in:

```text
<datadir>/scan-tasks/state/txinfo_error.json
```

Example:

```json
{
  "task": "txinfo",
  "time": "2026-04-15 12:34:56",
  "error": "write failed",
  "block_number": 12345,
  "block_hash": "0x..."
}
```

## Behavior Notes

### 1. Confirmed blocks only

Like the other scan task, `txinfo` only processes blocks that have reached the configured confirmation threshold.

### 2. The transaction limit is a soft cap

The task never splits a block in half. If a single block is large, it is still processed as one unit.

### 3. Rewind and resume are supported

When the chain reorganizes, output is missing, or the configured start block moves backward, the task can:

- rewind to a safe canonical height,
- truncate stale output lines,
- rebuild the result from the canonical chain.

## Troubleshooting

If startup fails with `corrupted scan task state file` and points to `txinfo_state.json`:

- Step 1: Check `<datadir>/scan-tasks/state/txinfo_error.json` for the exact parse error.
- Step 2: Back up both files:
  - `<datadir>/scan-tasks/state/txinfo_state.json`
  - `<datadir>/scan-tasks/txinfo_result.txt`
- Step 3: Remove only `txinfo_state.json` and restart the node.
- Step 4: Verify logs show txinfo task reinitialization and forward progress.

Optional manual resume:

- Instead of deleting the state file, provide a valid `txinfo_state.json` with a known canonical `block_number` and `block_hash`.
