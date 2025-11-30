# Scan Tasks

- [txinfo](./scan_task_txinfo.md) — Exports basic transaction index data from confirmed blocks.
- [kyc](./scan_task_kyc.md) — Scans `voteInvalidKYC` transactions and classifies them from receipt logs.

Both tasks share `--scan-confirmations` (default `0`, scan up to the chain head).
