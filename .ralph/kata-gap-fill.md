# Kata Driver Gap Fill

Close test gaps in nomad-driver-kata. Primary goal: run the real integration test
(nix, real Nomad + containerd + Kata). Secondary: raise unit coverage on the
0%-covered paths. Current baseline: kata package 54.9% statement coverage.

## Environment facts (verified 2026-07-22)

- Build/test: `nix develop --command go test ./...` (repo has flake.nix; always use nix)
- Integration test: `nix run .#integration-test` — REQUIRES ROOT. `sudo -n` fails
  (no-new-privileges flag set, sudo needs password). /dev/kvm exists and is 0666.
- VCS is jujutsu (jj), NOT git. Commit with `jj commit -m "..."` after each logical change.
- Existing integration script: tests/integration.nix (Phase 1: single-VM job, exec,
  signals, extra_hosts, VM sharing; Phase 2: multi-VM bridge networking). It is
  thorough — do NOT rewrite it. If it can run, run it as-is and fix driver bugs it finds.
- Known bug: parseMetricProto panics on nil input (noted in kata/stats_test.go work).

## Rules for the loop agent

- TDD: failing test first, then minimal code, then green. Never delete a failing test.
- Smallest reasonable changes. No rewrites of existing implementations.
- Max 2 checklist items per iteration. Update this file (checklist + Notes) every iteration.
- If an item is blocked (e.g. needs root), write the blocker under Notes, mark the
  item `[B]` blocked, and move on to unit-testable items. Do not spin on blocked items.
- Test output must be pristine: capture and assert expected errors.

## Checklist

- [x] Baseline: run `nix develop --command go test ./kata/ -coverprofile=/tmp/kata-cov.out`
      and record per-function 0% list in Notes
- [B] Integration test attempt: try `sudo -n nix run .#integration-test`. If root is
      unavailable, record exact failure in Notes, mark [B], and continue
- [ ] Unit: Driver.SetConfig — decode plugin config, error paths (bad config, invalid
      duration, bad consul_grpc_addr) using the existing fake/recorder client
- [x] Unit: Driver.ConfigSchema, TaskConfigSchema, TaskEvents — trivial but 0%
- [x] Fix: parseMetricProto nil-input panic — failing test first, then guard
- [ ] Unit: imageGC — needs ticker/interval injection; smallest refactor to let
      a test drive one GC cycle with the fake client (no behavior change)
- [ ] Unit: nsCtx and any pure helpers in containerd.go reachable without a live daemon
- [ ] Unit: CreateContainer OCI spec assembly — only if the spec-building can be tested
      via the existing interface/fakes WITHOUT restructuring containerd.go; otherwise
      mark deferred with reasoning in Notes
- [ ] Final: full suite green via `nix develop --command go test ./... -cover`, record
      new total coverage in Notes, all work committed with jj

## Verification

- `nix develop --command go test ./kata/ -coverprofile=/tmp/kata-cov.out` → ok 0.252s coverage: 54.9%
- `sudo -n nix run .#integration-test` → FAIL: "The no new privileges flag is set, which prevents sudo from running as root." [BLOCKED]

## Final Verification

- Exact monitor-rerunnable command: `nix develop --command go test ./... -cover`
- Working directory: /home/travis/dev/Personal/nomad-driver-kata
- Required preserved artifacts: source + test files (committed via jj)
- Result: (fill in)

## Notes

### Iteration 1 (2026-07-22)

- Baseline coverage: 54.9% statement coverage.
- Per-function 0% list:
  - containerd.go: NewContainerdClient, nsCtx, Close, Version, EnsureImage, ImageConfig,
    CreateSandboxMetadata, DeleteSandboxMetadata, CreateContainer, DeleteContainer,
    StartTaskDetached, RunTask, MonitorTask, KillTask, DeleteTask, TaskRunning,
    Exec, ExecStreaming, Metrics, Cleanup, GarbageCollect
  - driver.go: ConfigSchema, SetConfig, imageGC, TaskConfigSchema, TaskEvents
- Integration test blocked: no root access. sudo fails with "no new privileges" flag.
  /dev/kvm exists (0666) but root is required for integration test.
- Recorder already implements the full Containerd interface — good foundation for unit tests.

### Iteration 2 (2026-07-22)

- Fixed: parseMetricProto nil-input panic — added `if metric == nil` guard at stats.go:35
- Added: TestParseMetricProtoNilData (was skipped, now passes)
- Added: TestConfigSchema, TestTaskConfigSchema, TestTaskEvents (all trivially 100%)
- Coverage: 54.9% → 55.3%
- Remaining 0% functions:
  - containerd.go: all 21 methods (require live containerd; recorder covers logic paths via driver tests)
  - driver.go: SetConfig, imageGC
- Next targets: SetConfig error paths, imageGC refactor for testability
