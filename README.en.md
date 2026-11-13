[한국어](README.md) | **English**

# aigate: a verified AI-coding workflow with signed approvals

A personal "AI-native" development environment. Subscription models (Claude, Codex) write the code. A deterministic **gate** decides whether the work is actually done. Anything irreversible needs a **human signature** made with a passkey in a web console.

It is a one-person system running on a home OpenStack lab. It is also an engineering exercise in making an automated agent safe to leave unattended: small trusted components, fail-closed behavior and tests that try to break the rules.

> Language note: the design documents in `docs/` are written in Korean, because they are working documents for the author. This README, the code identifiers and the operator-facing UI strings tell you what you need; the docs have the full reasoning.

## What it does

```
 you ──passkey──▶ web console ┐
                              ▼
        ┌───────────────  orchd (Go, SQLite)  ───────────────┐
        │  job queue · state machine · policy · approvals    │
        └───┬──────────────────┬──────────────────┬──────────┘
            │ ssh (limited key) │ ssh (limited key) │ unix socket
            ▼                   ▼                   ▼
      agent VM             test VM            orch-broker
      Claude / Codex       restored from      holds the model API key,
      commits locally      a snapshot per     provider tokens and the
      only                 job, no secrets    project registry
```

1. **Gate (`aigate`)** runs tests on the base commit and on the head commit and compares. Verdicts: `ok`, `behavior_failure`, `verification_incomplete`, `check_weakened`. A test that passed on base must still run and pass on head. Weakening a test is allowed but flagged, and needs a human to approve the diff hash.
2. **Orchestrator (`orchd`)** queues jobs, runs the agent in an isolated VM, sends the result to a separate test VM restored from a clean snapshot, and records evidence. Publishing is limited to an allowlist of actions (for example pushing a work branch, never `main`).
3. **Broker (`orch-broker`)** is the only component that holds credentials. It checks every request against a strict schema and an allowlist of provider API calls, and it changes the project registry only after verifying a **WebAuthn signature** over the exact change.
4. **Web console** is plain JavaScript embedded in the `orchd` binary (no framework, no build step, no browser storage). The browser recomputes the hash of every change it is asked to sign and refuses if the server's description does not match.

## Design choices worth reading

- **Evidence over claims.** The gate never reads model output; it runs and compares. "Done" requires a report.
- **Separation of duties.** The component that runs untrusted code has no credentials. The component with credentials never runs untrusted code and exposes only a narrow, schema-checked interface.
- **Signed, bounded changes.** Registry changes (create, link, delete, key removal) are canonical JSON, hashed, and signed with a passkey. The broker verifies freshness, single use and equality before acting.
- **Irreversible actions are narrow.** Deleting a remote repository or a deploy key is only possible for resources this tool created or registered, after a live identity check, with hourly and daily caps, and never as an automatic retry.
- **Fail closed.** Ambiguous results (timeouts, partial failures) stop the flow and leave the state readable; the UI says what may have happened and nothing retries on its own.
- **Adversarial tests.** Each milestone ships a suite that tries to break its own rules, plus a mutation table (mutants run on a copy; survivors are explained).

## Repository map

| Path | Contents |
|---|---|
| `cmd/aigate`, `internal/gate`, `internal/adapter` | The gate and its test-runner adapters (Go `go test`, pytest, vitest/jest) |
| `cmd/orchd`, `internal/orch/*` | Orchestrator: API, store (SQLite), VM control, publishing, project clone/removal, embedded web console (`internal/orch/web`) |
| `cmd/orch-broker`, `internal/broker`, `internal/registrymsg` | Credential broker, provider clients (GitLab, GitHub), signed registry messages |
| `cmd/orch` | Operator CLI |
| `internal/webauthn`, `internal/approval`, `internal/ledger` | Passkey verification, approval records, append-only history |
| `e2e/` | End-to-end scenarios for the gate |
| `infra/` | Provisioning scripts (OpenStack VMs, nftables egress rules, cloud-init, systemd units) and their offline tests |
| `docs/` | Numbered requirements, designs, plans, results and operations runbooks (Korean) |

## Build and test

Requires Go (see `go.mod`) and Node.js for the web-console tests.

```
make build                                  # bin/aigate
go test ./internal/... ./cmd/... -count=1   # unit and integration tests
node --test "internal/orch/web/test/*.test.js"
go test ./e2e -count=1                      # gate end-to-end scenarios
```

At the time of the last full run (2026-11-13): 1,787 top-level Go tests across 38 packages with `-race`, and 317 web-console tests, all passing.

## Install and operate

Provisioning and installation are scripted and documented, not one-click:

- `infra/README.md` covers creating the three VMs on OpenStack, the egress firewall that must be verified before any agent runs, and the systemd units.
- `docs/10-OPERATIONS-orchestrator.md` holds the step-by-step runbooks, written as a series of cutovers (backup, stop, upgrade in a fixed order, verify, roll back). Each says which results were observed on the real server and which are only expected from the code.

## Status and verification boundaries

This is a working personal system, built in milestones (gate, orchestrator, web console W1 to W3g). The results documents separate what was **observed** from what was **inferred**.

- Observed on the real server: signed project creation and linking for GitLab and GitHub, per-repository deploy keys, clone, publish, and (W3g) deleting a remote repository, the local copy and the registry entry with one signature, and detaching a deploy key together with the removal. See `docs/29` and `docs/31`.
- Not verified, and said so in the docs: removing a shared GitLab deploy key from a second project on a real GitLab, real-passkey signing for every screen, and some error screens that were only exercised against fakes.
- Known open decision: binding the deploy-key fingerprint into the signed change for GitLab (`docs/31` section 8).

## Not included

This repository has no real credentials, hostnames or addresses. Network values in docs and scripts are examples. Provider tokens live only in the broker's token store on the server and are entered through a web screen, never through the command line.

## License

MIT. See `LICENSE`.
