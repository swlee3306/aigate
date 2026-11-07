# prune-defer report

Change: the post-restore background prune is no longer started by Restore. Restore marks it pending
(phase `prune_deferred`, duration 0); `TestVM.GateFinished()` starts it (own context, 5-minute bound,
panic-safe, same pruneMu and OnPrune). steps.verify calls it via an optional `gateFinisher` interface
with `defer`, registered right after a successful Restore (so it also covers Executor failure, gate error
and panic), only when RestoreEachJob is on.

Safety nets: the next Restore starts a never-started pending prune before it deletes the server and
waits for it with the existing bounded wait; the existing wait for an in-flight prune is unchanged.
GateFinished with nothing pending is a no-op, twice starts once. No guard in prune.go was touched.

Existing tests changed (start-at-restore timing deliberately replaced by an explicit `v.GateFinished()`
after Restore): TestQuotaOKSkipsPrePruneAndPrunesInBackground, TestRestoreReturnsBeforeSlowBackgroundPruneCompletes,
TestNextRestoreWaitsForBackgroundPrune (also for the second restore's prune), TestWaitTimeoutProceedsWithWarning...,
TestBackgroundPrunePanicDoesNotCrash (both restores), TestPanickingOnPruneCallbackDoesNotCrash,
TestTaskContextCancelDoesNotCancelBackgroundPrune. All others untouched.

New tests: vm: DoesNotStartBeforeGateFinished (deterministic via bgDone/pending, no sleeps),
GateFinishedStartsThePruneExactlyOnce, GateFinishedWithNothingPendingIsANoOp (also zero-value TestVM, prune off),
QuotaTightPrunesBeforeAndGateFinishedDoesNothing, NextRestoreStartsAPendingPruneThatNeverStarted (order
DeleteServer,Prune,DeleteServer). steps: GateFinished after the gate, on gate error, on gate panic, on
Executor error, not without RestoreEachJob, not after a failed restore.
Mutation check: removing the defer fails 4 steps tests; starting at restore time fails the vm test.
Note: implementation was written before the tests; the new tests were then verified to fail under those mutations.

Verification: go vet ./... clean; go test -race ./internal/orch/... ./cmd/... -count=1 ok;
vm+steps 10x -race under CPU load (one `yes` per core) ok; go test ./... -count=1 ok; sh infra/test/run.sh ALL PASSED.
Docs: package doc in vm/agent.go, test.go comments, docs/10 section 6 hypothesis sentence (numbers untouched).
