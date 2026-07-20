# Flowbee v2 Phase 1 candidate evidence

This is an evidence manifest, not activation approval. Populate it for the exact
commit and pinned binary proposed for the Phase 1 canary. A repository test name
means the proof exists; it does **not** mean that test or any live gate passed for
the candidate.

Status convention: `[ ]` is required and unverified. `LIVE-ONLY` must be run
against the exact daemon, credentials, endpoints, database, and binary used by the
canary. Do not turn a skipped live test into a green result.

## Candidate identity

- [ ] Commit: `________________`
- [ ] `git status --porcelain` result: `________________`
- [ ] Binary: `/absolute/path/________________`
- [ ] Binary SHA-256: `________________`
- [ ] `flowbee version`: `________________`
- [ ] Driver artifact/version/hash: `________________`
- [ ] Driver endpoint inventory path/hash: `________________`
- [ ] Flowbee config and service-env path/hash: `________________`
- [ ] Database path: `________________`
- [ ] Pre-canary backup path/SHA-256: `________________`
- [ ] Prior binary and saved environment: `________________`
- [ ] Old PID / candidate PID / activation timestamp: `________________`

## Static candidate gates

Run from the clean candidate commit and attach the complete logs.

- [ ] `git diff --check`
- [ ] `go run ./tools/archcheck`
- [ ] `go run ./tools/laddercheck`
- [ ] `go vet ./...`
- [ ] `go test ./... -count=1 -timeout=300s`
- [ ] `go test ./... -short -race`
- [ ] `go test ./test/acceptance -count=1 -timeout=300s`
- [ ] `go test -race ./internal/store ./internal/driver ./internal/alertingress ./internal/deadman -count=1 -timeout=300s`
- [ ] Build the pinned artifact from this commit and record `shasum -a 256` above.

## Repository proof map

| Requirement | Named repository proof | Candidate evidence |
|---|---|---|
| Admission is durable and lost acknowledgements cannot create a second epic | `TestAddEpicRunV2AdmissionIsIdempotentAndCreatesReviewObligation`; `TestV2AdmissionWithDistinctFreshReviewerIsExactlyOnceAcrossLostAck` | [ ] log |
| Real CI green creates a durable review obligation; absent checks and moved heads do not | `TestEpicArtifactRealGreenEntersDurableReviewDispatchState`; `TestEpicArtifactNeverTreatsGreenByAbsenceAsGreen`; `TestEpicArtifactHeadAdvanceCancelsOldEffectAndRequiresFreshCI` | [ ] log |
| An interruption between build completion and review dispatch self-heals exactly once | `TestEpicReviewReconcilerRecoversInterruptedHandoffIdempotently`; `TestEpicReviewHandoffRecoversInFreshProcessAndBecomesClaimable` | [ ] log |
| Owned epic PRs do not collide with legacy adoption | `TestLegacyAdoptSweepFencesOwnedBranchOnlyUnderV2Flag`; `TestTargetedAdoptRechecksEpicOwnershipInsideInsertTransaction` | [ ] log |
| Builder/reviewer capacity is fresh, identity-bound, distinct-family, and fail-closed | `TestV2AdmissionRejectsNoReviewerWithoutPartialRows`; `TestV2AdmissionRejectsFreshSameFamilyCapacity`; `TestNativeReviewCapacityHoldRearmsOnlyAfterCompleteGeneration`; `TestCapacityGenerationProjectsIdentityDriftAsAuthDead` | [ ] log |
| Review wake-up uses one immutable, exact Driver action; no lateral/direct session send | `TestNativeReviewClaimCreatesOneImmutableFullyBoundDriverWake`; `TestReviewWakeForbidsLateralRouteWithZeroDriverMutation`; `TestNativeReviewSyntheticControlBindingInsertedAfterStartupCannotClaim` | [ ] log |
| Every overdue non-terminal delivery has recovery or visible attention | `TestDeliveryAgnosticBackstopSurfacesEveryOverdueState`; `TestPoisonFactIsQuarantinedWhileLoopRemainsHealthy`; `TestReconcilerWatchdogPersistsOneAlertAndFencesOldIncarnation` | [ ] log |
| Exact two-domain endpoint resolution has no default fallback or cross-domain leakage | `TestLoadDriverEndpointInventoryRequiresExplicitExactTopology`; `TestDriverEndpointInventoryNeverSynthesizesLegacyDefault`; `TestDriverEndpointInventoryRejectsCollapsedOrIncompleteIsolation`; `TestEndpointResolverKeepsExternalAndManagedDomainsSeparate`; `TestEndpointResolverMissingKeyHasNoFallbackOrPortCalls`; `TestRuntimeResolvesExternalAndManagedActionsOnlyToExactEndpoint` | [ ] log |
| Driver store/server/pane/run changes fence stale authority | `TestEndpointResolverReadinessRefreshesServerIncarnationAndFencesOldActions`; `TestEndpointResolverIncarnationRefreshNeverWeakensExactBoundary`; `TestRuntimeOldStoreKeyIsFencedAfterEndpointStoreReset`; `TestV25ObservedRouteFencesDomainServerAndRunBeforeGrantOrSend` | [ ] log |
| External Interactor adoption and managed actor ensure are durable and restart-safe | `TestActorLifecycleRuntimeRussClaudeAdoptLostResponseRecoversWithoutResend`; `TestActorLifecycleRuntimeManagedEnsureRestartUsesDurableReceipt`; `TestActorLifecycleRuntimeExternalReattachRejectsStaleRun`; `TestActorLifecycleRuntimeReleaseAndNoCrossEndpointFallback` | [ ] log |
| Transport receipt is not stage success; crash-uncertain delivery is never blindly resent | `TestConversationRuntimeRoutesOnceAndRequiresSeparateProcessingEvidence`; `TestConversationRuntimeRestartAfterClaimNeverBlindlySends`; `TestConversationRuntimeSendUncertaintyNeverResends`; `TestWorkIntentRuntimeCrashUncertainNeverBlindlyResends` | [ ] log |
| Builder launch/relaunch is single-seat, exact-incarnation, and restart-safe | `TestAdmissionLaunchCrashRedispatchesExactlyOnce`; `TestBuilderLaunchStoreResetFencesCommittedTargetBeforeEnsure`; `TestConcurrentAdmissionsCannotDoubleBookBuilderCapacity`; `TestBuilderDoneParksOnlyAfterExactDriverStopAndKeepsScopeReserved` | [ ] log |
| Merge, conflict re-review, and cleanup converge or become a durable hold | `TestExactHeadMergeToCleanupConverges`; `TestCrashAfterMergeEffectBeforeFactRecoversWithoutResend`; `TestCrashAfterCleanupDeleteVerifiesAbsenceWithoutResend`; `TestConflictResolutionNewHeadRequiresFreshCIAndReview`; `TestDeadLetteredMergeAndCleanupRearmSameAction` | [ ] log |
| Alerts survive ingress/projection crashes and reach exactly the bound Interactor | `TestStoreAcceptorCommitsExactBodyAndControlAlertBeforeSignedAck`; `TestControlAlertProjectionHoldsThenCommitsExactlyOnceToCurrentInteractor`; `TestControlAlertStaysOutstandingUntilSeparateInteractorEvidence`; `TestP1DeadmanNotificationCrashRecoveryToExactInteractorEvidence` | [ ] log |
| Watchdog is independently supervised and control-only degradation cannot mask a real failure | `TestWatchdogOnePassWiresHealthStateAndAuthenticatedWebhook`; `TestDriverControlDegradationDoesNotMaskLaterDeadmanEpisodes`; `TestDeadmanPublisherRejectsProxy2xxAndReplaysExactBody` | [ ] log |
| Signed watchdog heartbeat is restart-durable, project/identity-bound, creates no human alert, and dynamically opens/closes readiness | `TestHeartbeatSurvivesRestartWithStableSequenceAndNoHumanNotification`; `TestStoreAcceptorHeartbeatAdvancesLeaseWithoutHumanAlert`; `TestStoreAcceptorHeartbeatRejectsSpoofAndProjectMismatch`; `TestFirstBootIngressHeartbeatConvergesDynamicReadinessWithoutHumanAlert`; `TestWatchdogIntervalCannotOutliveReadinessLease` | [ ] log |
| Project readiness is recomputed from exact current actor, endpoint, reviewer, builder, capacity, and watchdog facts | `TestHealthEndpointReevaluatesPhase1ProjectReadiness`; `TestProjectActivationDistinguishesConfiguredInventoryFromLiveCapacity`; `TestRequirePhase1ProjectLiveReadyRejectsIncompleteExactProject`; `TestProjectActivationRejectsLegacyActorBindingWithoutLifecycle` | [ ] log |
| Migrations are preceded by a verified snapshot and overlapping writers are fenced | `TestMigrateWithRollbackSnapshotProtectsExistingDatabase`; `TestMigrateWithRollbackSnapshotFailsClosedWhenSnapshotCannotBeWritten`; `TestEveryProductionMigrationCallerTakesWriterLockFirst`; `TestBackupRoundTrip` | [ ] log |
| Rollback is explicit and cannot silently reopen legacy pane control | `TestServeV2SelectionPersistsAndOnlyExplicitServeRollbackClears`; `TestLegacyPaneRuntimeActivationPredicate`; restore writer-lock and WAL round-trip tests in `cmd/flowbee/restore_test.go` | [ ] log |

## Live-only activation gates

None of these gates is asserted passed by this document.

### 1. Exact Driver topology and capabilities

- [ ] `LIVE-ONLY` External/default endpoint resolves the adopted project Interactor
  (`russ-claude`) by exact host, store, server-domain, session, pane incarnation,
  and agent run. Record all stable IDs: `________________`.
- [ ] `LIVE-ONLY` Managed-dedicated endpoint resolves Flowbee-created Orchestrator
  and worker lifecycle targets by exact host/store/domain. Record IDs:
  `________________`.
- [ ] `LIVE-ONLY` Prove there is no single-default fallback and no external ↔
  managed-dedicated leakage. Evidence: `________________`.
- [ ] `LIVE-ONLY` On each endpoint, `GET /v2/meta` advertises
  `features.control_principal_origin=true` and authenticated
  `GET /v2/control/capabilities` returns the exact v1 capability format,
  `principal_id=flowbee-control`, `supported=true`, `authorized=true`, and
  `missing_scopes=[]`. Save redacted responses.

### 2. Lifecycle and live UDS conformance

- [ ] `LIVE-ONLY` Prove Flowbee commits the immutable lifecycle/action intent before
  any Driver effect.
- [ ] `LIVE-ONLY` Adopt the existing external Interactor without mutating its tmux
  identity; ensure an isolated managed Orchestrator/worker; restart Flowbee after a
  lost response and prove receipt recovery without resend.
- [ ] `LIVE-ONLY` Prove exact release/stop and positive absence for the isolated
  managed target. No raw tmux, `send-keys`, or standalone `tmux-send` invocation is
  permitted.
- [ ] `LIVE-ONLY` Against **both** canary endpoints' actual UDS and bearer, run the
  read/capability/observation portion without lifecycle mutation:

  ```bash
  FLOWBEE_DRIVER_LIVE_TEST=1 \
  FLOWBEE_DRIVER_SOCKET=/exact/api.sock \
  FLOWBEE_DRIVER_TOKEN_FILE=/owner-only/control.token \
    go test ./internal/driver -run '^TestLiveDriverV24Conformance$' -count=1 -v
  ```

- [ ] `LIVE-ONLY` On the **managed-dedicated endpoint only**, rerun that command with
  `FLOWBEE_DRIVER_LIVE_LIFECYCLE=1` and
  `FLOWBEE_DRIVER_LIVE_CONTROL_ORIGIN=1` to prove isolated ensure → presence →
  control-origin insertion → exact stop → positive absence. Do not use managed
  `Ensure` as conformance against the external/default domain.
- [ ] `LIVE-ONLY` On the external/default endpoint, prove the project actor lifecycle
  adopts the existing Interactor by exact watch/session/pane/run, survives a Flowbee
  restart/lost response, and never mutates or replaces that tmux session. Keep the
  adopted route active for the canary; exercise exact release only during rollback.
- [ ] `LIVE-ONLY` Attach proof for exact replay returning the original receipt,
  changed-body conflict, route denial, stale recipient/run fencing, and
  crash-uncertain recovery without resend. A submitted receipt is not stage proof.
- [ ] `LIVE-ONLY` Run `TestPhase1ServeLiveDriverObservationSmoke` using the same
  pinned candidate, daemons, bearers, and endpoint inventory; record skips as failure
  to satisfy this gate.

### 3. Capacity, backup, service, and dashboard

- [ ] `LIVE-ONLY` Record a fresh identity-bound local Codex builder and a fresh
  distinct-family reviewer; prove stale/drifted capacity fails closed.
- [ ] `LIVE-ONLY` Stop/identify writers, take the pre-migration backup, verify it,
  hash it, and record writer-lock evidence. Save the previous pinned binary and
  service environment before changing either.
- [ ] `LIVE-ONLY` Run the pinned candidate only—never `go run` or a dirty-worktree
  binary—and prove the managed service points to its exact path and hash.
- [ ] `LIVE-ONLY` Run `flowbee doctor --offline`, `/configz`, authenticated
  `/dashboard`, and Tailnet sign-in/CSRF smoke with owner-only key/grant files.
- [ ] `LIVE-ONLY` Confirm the independent watchdog and signed ingress use the exact
  project ID and a `0600` secret file. On first boot, prove `/healthz` reports only
  `external_watchdog_lease_missing_or_stale` while the signed ingress remains
  reachable; one accepted heartbeat must make readiness green without a Flowbee
  restart and without creating a `control_alert` or Interactor message.
- [ ] `LIVE-ONLY` Stop only the watchdog. After two minutes, prove `/healthz` closes
  readiness with `external_watchdog_lease_missing_or_stale`; restart it and prove
  the next durable sequence restores readiness. While Flowbee itself is unavailable,
  prove the watchdog retains firing/resolved notifications locally and delivers each
  once through the signed ingress after recovery.
- [ ] `LIVE-ONLY` After readiness is green, independently stale or revoke an actor
  incarnation, exact endpoint capability, and capacity generation. Each fact must
  close the `phase1_project` readiness projection on a later health request; restoring
  exact evidence must recover without restarting Flowbee.

### 4. Incident and completion drill

- [ ] `LIVE-ONLY` Submit one focused dashboard request and prove durable conversation
  and work-intent rows precede agent acceptance.
- [ ] `LIVE-ONLY` Approve any typed gate by exact version/hash and prove automatic
  promotion through admission without a second human “go.”
- [ ] `LIVE-ONLY` Restart the Orchestrator after Driver insertion but before separate
  processing evidence; prove no blind resend and no stage advance from the receipt.
- [ ] `LIVE-ONLY` Restart Flowbee after admission but before builder launch evidence;
  prove exactly one epic, physical seat lease, and current lifecycle action.
- [ ] `LIVE-ONLY` On a real CI-green PR, interrupt build-complete → review-dispatch.
  Prove exactly one recovered review and one exact-project Interactor alert/hold.
- [ ] `LIVE-ONLY` Complete review, merge, and cleanup; prove the seat, lifecycle
  target, worktree, branch, actions, and attention converge or show a durable hold.

### 5. Rollback rehearsal

- [ ] `LIVE-ONLY` Before the flip, rehearse stopping the exact candidate PID,
  explicitly disabling Phase 1/v2 in the writer-owned serve start, restoring the
  saved service environment and prior pinned binary, and leaving additive data intact.
- [ ] `LIVE-ONLY` Prove the prior service uses the intended DB/listener with
  `flowbee doctor --offline`, `/configz`, and dashboard smoke.
- [ ] `LIVE-ONLY` Prove rollback neither converts a control-origin action to
  `on_behalf_of_session_id` nor re-enables direct pane actuation for v2-owned work.
- [ ] `LIVE-ONLY` Document emergency restore only as a stopped-writers procedure;
  do not restore merely to erase a hold or pending action.

## Notification authority: Interactor only

Human-facing notifications are project-scoped workflow obligations delivered to the
project's exact Interactor session through Driver grants and receipts. There is no
Matrix implementation in P1 and no Slack, email, generic provider webhook, or other
outbound human-notification sink.

The sole webhook in this path is **inbound**: the independently supervised watchdog's
`FLOWBEE_ALERT_WEBHOOK_URL` points to Flowbee's signed private
`/v1/control-alerts/ingress`. Flowbee durably accepts that incident as a
`control_alert`, projects it to the exact Interactor, and retains a visible route hold
until Driver delivery is possible. `flowbee serve` must not be configured with an
outbound notification URL, provider fallback, or global Interactor fallback.

The watchdog's signed `external_watchdog_heartbeat` uses the same ingress but is a
readiness fact, not a human notification: it advances only the exact-project watchdog
lease and must create no `control_alert` or Interactor message. Only firing/resolved
incident envelopes enter the Interactor notification path.

## Approval

- [ ] Candidate/static evidence reviewed by: `________________`
- [ ] Every live-only gate above completed on: `________________`
- [ ] Stop conditions absent: no duplicate effect, cross-project route,
  stale-incarnation send, green-by-absence, missing Interactor alert/hold, or
  non-terminal state without a next action/visible hold.
- [ ] GO / NO-GO decision and signer: `________________`
