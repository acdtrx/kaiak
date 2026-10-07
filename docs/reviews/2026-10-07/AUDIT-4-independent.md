# Audit D — fourth review

## Verdict

**Changes requested.** Three High findings allow budget under-enforcement in the
new restart/retention paths. Four reproduction tests fail against the submitted
implementation, including both file-mode and control-plane-mode variants of the
removed-scope test. There is also one Low documentation contradiction. No additional
control-plane defect was confirmed.

Reviewed the supplied plain copy against `AGENTS.md`, `docs/kaiak.md`, and the gateway
and control-protocol contracts. No production code was changed. Tests added:

- `gateway/internal/control/audit_d_test.go`
- `gateway/internal/limits/audit_d_test.go`

## High

### D-H1 — A foreign-instance ack destroys the only restart copy of unsaved spend

**Frequency: rare. Severity: High.** Requires retaining a data directory across an
instance-ID change, then another restart before covering totals have been saved.

**Location:** `gateway/internal/control/usage.go:653` deletes the foreign batch on
ack; `usage.go:322` rebuilds foreign records regardless of the saved cursor.

**Mechanism:** The new restoration path correctly rebuilds another instance's
spooled records into own usage. However, `sendOutstanding` immediately removes that
batch's file when its delivery is acknowledged. Neither `totals.json` nor any other
durable file has to include its spend yet. A crash after the removal and before a
covering save leaves the next boot with the old base and no record to rebuild.

The reproduction saves a zero base, spools a $1 record, changes instance ID, and
verifies that the rebuilt usage exhausts a $1 budget. It delivers the foreign batch
through the real sender to the fake control plane, receives its ack, and recreates
the client and limiter without a totals save. The spent budget then admits another
priced request. The control plane still has its authoritative record; **the loss is
from local budget enforcement**, until fresh totals arrive. During an outage, that
can last through the boot's outage grace. The unsafe interval can also precede a
clean shutdown if no fresh totals arrive before its final save.

**Contract conflict:** `GATEWAY.md`'s Usage spool explicitly permits deleting another
instance's batch on ack. That exception contradicts Restart keeps the last totals'
promise that the saved base and spool retain the spend across a crash, including
foreign batches. Both code and this exception need correction.

**Fix sketch:** Persist foreign acknowledgements as non-resend entries and retain
their records until a completed totals save is proven to include their delivery.
For example, persist an association with a subsequent batch of the current instance
whose saved coverage proves the earlier foreign deliveries were counted. Keep
foreign entries excluded from the own-instance stale-totals clock.

**Confirmed failing test:** `TestAuditDForeignAckCrashLosesRestoredSpend`.

```text
crash after foreign ack admitted a request against the already spent 1 USD budget
```

### D-H2 — Reordered restored epochs break the generation-prefix assumption

**Frequency: rare. Severity: High.** Requires multiple retained epochs and loss or
rejection of the spool index; no malformed batch or malicious control plane is needed.

**Location:** `gateway/internal/control/usage.go:471`, with generation assignment at
`gateway/internal/control/spool.go:139` and `spool.go:151`; the resulting prefix is
retired at `gateway/internal/limits/shared.go:185`.

**Mechanism:** When the index is missing, unreadable or rejected, all surviving batch
files are queued under their original IDs and a fresh current epoch is created.
Restoration sorts the older epochs lexically and assigns generations in that order.
Random epoch order is not historical delivery order. A previously counted epoch can
therefore receive a later generation than a still-unsent epoch.

`countedGeneration` scans the entire queue and returns the maximum covered
generation. The limiter then discards **every** generation up to that number,
including the earlier-in-sort-order batch which the totals do not cover. The fact
that future sends are serialized does not establish ordering for batches already
counted before this restart.

The reproduction leaves a counted `bbbb…/1` file and an unsent `aaaa…/1` file, each
costing $0.50, with a saved zero base and no index. This state can arise when one
index loss starts a new epoch during a delivery outage and a later restart loses
the replacement index while the old acknowledged file is still retained. Restoration
assigns the unsent batch generation 1 and the counted batch generation 2. Complete
totals covering only `bbbb…/1` retire both: a $1 budget sees only $0.50 and admits
another request. The missing amount returns only when the unsent batch is counted
and pushed.

**Fix sketch:** Track coverage independently for restored batches. Advance a scalar
retirement frontier only across a contiguous, demonstrably covered/resolved prefix;
alternatively retire explicit generations. Do not infer coverage of another epoch
from its new position in the restored queue. Apply the same rule when marking
acknowledged entries covered for the outage clock.

**Confirmed failing test:** `TestAuditDLostIndexMustNotRetireAnUnsentEpoch`.

```text
covering only the old counted epoch retired through generation 2 and forgot the unsent 0.5 USD
```

### D-H3 — Retained counters can be pruned while requests still reference them

**Frequency: occasional. Severity: High.** A group is deleted/recreated while a
request is running; the window-boundary variant is rarer.

**Location:** `gateway/internal/limits/limits.go:315`–`318`; settlement still writes
the original pointer at `limits.go:590`–`596`.

**Mechanism:** `pruneRetainedLocked` equates `w.used == 0` with having no outstanding
request references. USD reservations always reserve zero. Deleting a group with no
settled local cost therefore immediately removes its month counter from `retained`,
even though a running priced request holds that counter. Settlement later adds the
cost to the detached object. Recreating the group allocates a different counter,
starting without this spend. `limits.json` also omits the detached object.

Token reservations have the same lifetime problem at rollover: pruning rolls the
window, clears the old reserved amount, and drops the counter. A request that ends
in the new hour settles into that detached counter, so its current-hour usage is
lost when the scope returns.

In file mode the group budget permanently loses the settled amount for the window;
global/ancestor counters do not repair that group's enforcement. In control-plane
mode the loss lasts until covering totals arrive, and can persist through an outage.
The existing recreate test starts with settled cost already present, which masks
the zero-amount reservation case.

**Fix sketch:** Track outstanding reservation references separately from reserved
units, including zero-amount holds, and keep the counter discoverable until all
references settle. Reference lifetime must survive a window rollover, even though
the old reserved units do not. Once the last reference settles, prune only when
there is also no current own usage.

**Confirmed failing tests:**

- `TestAuditDRemovedScopeKeepsZeroAmountReservations` — fails in both modes.
- `TestAuditDRemovedScopeReservationSurvivesWindowRollover` — file-mode snapshot
  pruning between rollover and settlement loses the new hour's tokens.

```text
recreated scope counts 0 nano-USD, want 1000000000 settled by its running request
spent group budget admitted another priced request
recreated scope counts 0 tokens, want 100 settled in the new hour
```

## Low

### D-L1 — The limits contract still says an ack permits spool removal

**Frequency: occasional. Severity: Low.** A documentation inconsistency encountered
when maintaining or operating persistence, not a separate runtime defect.

**Location:** `docs/specs/GATEWAY.md:1517`–`1518`.

**Mechanism:** The paragraph says an ack “tells the client the batch can leave the
spool.” For own-instance batches, the implementation and the updated Usage spool,
Restart keeps the last totals, and control-protocol sections require a completed
covering totals save. The old sentence describes the unsafe deletion point this
change is intended to remove.

**Fix sketch:** Say that an ack ends delivery/retries; own usage retires on applied
coverage, and its durable batch leaves only after saved coverage. Resolve the foreign
exception separately as D-H1. No executable reproduction applies to prose.

## Checked and found sound

- **Same-instance save/delete ordering:** bases and `counted_through` are copied
  together under the limiter lock. The writer receives the cursor of the snapshot
  actually written, not a later push. Batch deletion follows the atomic,
  file-and-directory-synced totals write. A process crash before saving leaves the
  old base plus batch; after saving but before deletion, restoration excludes the
  covered batch; after deletion, the saved base holds it. This reasoning excludes
  the foreign-instance exception above and storage failures beyond process crashes.
- **Acknowledgement/index ordering:** an own ack records the epoch's acknowledged
  sequence before moving the queue entry; queued and acknowledged entries remain
  visible to concurrent coverage processing under one mutex. A saved batch ahead
  of the index moves `next_sequence` past itself on restart. Missing indexes start
  fresh epochs rather than reuse IDs. Current-format files survive an old-format
  index; old-format batch files are intentionally discarded. D-H2 concerns the
  later interpretation of these restored entries, not sequence reuse.
- **Ordinary crash rebuild and the interval between saves:** uncovered queued and
  acknowledged same-instance batches rebuild own usage; saved coverage excludes
  records already in the base. Existing crash/restart tests pass. The unsealed
  filling batch remains the documented crash-loss allowance.
- **Outage clock in normal send order:** later acks preserve the oldest uncovered
  ack's timestamp; coverage clears it; an ack already covered by the last totals
  does not start it. Restored acknowledged batches start waiting at boot. Foreign
  acks do not enter this clock. Acks/status do not act as stream contact. The
  10,000-entry memory bound preserves the wait timestamp when evicting the oldest
  entry; forgetting coverage detail can conservatively overcount until a retained
  later batch is covered, as documented.
- **Late totals and zero-cost batches:** an open heartbeat stream does not mask
  acknowledged-but-uncovered usage past the grace. The control feed pushes cursor
  changes even when a batch changes no window. Thus zero-token/zero-cost records
  can finish their wait without requiring a changed usage window.
- **Malformed totals and reconnects:** malformed decoded totals terminate the
  stream; each new connection resets the complete-totals flag. The control feed's
  join watermark excludes both cached reads and reads issued before joining.
  Failed reads do not supply cached first totals. Existing reconnect, read-failure,
  cursor-only and store-rollback tests pass.
- **Counter allocation and ordinary retention:** both semantic validators count
  two fixed counters per scope plus distinct effective per-minute types, including
  inherited defaults, against 50,000. Only sliding-minute windows allocate the
  second buckets. Settled retained counts survive ordinary delete/recreate and
  file snapshots; expired pushed windows are pruned hourly. Reservation lifetime
  is the exception in D-H3.
- **Changes-only totals:** the normal active-counter path updates listed windows,
  minute shares only when the live count changes, and own-usage counters when the
  retirement frontier advances. Retained-counter pruning additionally scans
  `retained` on every push. Already-covered *queued* entries can report the same
  generation again; the limiter's monotone guard makes repeated retirement a no-op.
  Thus the “only newly covered” comment is stronger than the queued-entry behavior,
  but I found no separate accounting defect from this repetition.
- **Locks and shutdown:** index mutations take `persistMu` before `mu`; cleanup
  releases `mu` before obtaining `persistMu`. The periodic writer is owned by the
  background wait group and completes before the final shutdown save. No new data
  race was reported by the executed race-enabled suites.
- **Removed behavior:** no production references to `clearedAt`,
  `RestoredGeneration`, or `effective-limits-exceeded` remain in the reviewed paths.
  The restored uncounted lump is gone; its old-format occurrence is a rejection
  fixture. Totals persistence runs periodically and at shutdown, not on every push.

## Verification

The sandbox initially denied Go's default cache and local test-server ports. Checks
were rerun with `GOCACHE=/tmp/kaiak-audit-d-go-cache` and local-server access. The
initial environmental failures are not findings.

| Check | Result |
| --- | --- |
| `scripts/check-gateway.sh` | Formatting, vet and staticcheck passed. Race suite ran; only the two new limits test functions failed in that run. The control reproduction file was added afterward. All other packages, including gateway e2e, passed. |
| `go test -race -count=1 ./internal/control ./internal/limits` after both test files existed | Failed only the four `TestAuditD…` functions above; no race report. Existing tests in both packages passed. |
| `go test -race ./internal/control ./internal/limits -run TestAuditD -count=1 -v` | All four reproduction functions failed with the assertions quoted above. |
| `cd control && npm ci --ignore-scripts`, then `npm test` | Installed successfully; 614 tests, 613 passed, 0 failed, 1 existing store-channel test skipped because the memory store's channel never drops changes. |
| `cd control && npm run lint` | Passed: TypeScript and boundary lint. |
| `go test -race -tags crosshalf -run '^TestAcrossHalves' -count=1 ./e2e` | Passed, 69.674 s. |
| Live-test kit: vet, pinned staticcheck, `go run . -self-test` | Passed for all six provider types and the two-backend vLLM case. Run separately because the intentional failing audit tests stop the gateway script before this stage. |

`scripts/check-all.sh` was not claimed green: its gateway stage necessarily fails
with these reproductions retained. Its later control/lint/cross-half checks were
executed separately. No tests were weakened or removed.

Full local logs: `/tmp/audit-d-gateway-full.log`, `/tmp/audit-d-packages.log`,
`/tmp/audit-d-reproductions.log`, `/tmp/audit-d-control-baseline-unrestricted.log`,
`/tmp/audit-d-control-lint.log`, `/tmp/audit-d-crosshalf.log`.
