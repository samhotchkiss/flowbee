package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/advisor"
	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/alerting"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/driverbridge"
	"github.com/samhotchkiss/flowbee/internal/epicexec"
	"github.com/samhotchkiss/flowbee/internal/epicflow"
	"github.com/samhotchkiss/flowbee/internal/epicsupervisor"
	"github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/projectbreaker"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/watchdog"
	"github.com/samhotchkiss/flowbee/internal/webhook"
	actorprotocol "github.com/samhotchkiss/flowbee/protocol/flowbee/v2"
)

// rejectSyntheticDriverControlBinding prevents a deprecated session-shaped
// flowbee-control identity from coexisting with Driver's authenticated v2.4
// control-principal origin. Flowbee-authored messages are owned by the exact
// token-bound principal, never by a fabricated agent session.
func rejectSyntheticDriverControlBinding(ctx context.Context, db *sql.DB) error {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings
		WHERE worker_identity=? AND role=? AND state='active'`,
		store.DriverControlIdentity, store.DriverControlRole).Scan(&count)
	if err != nil {
		return fmt.Errorf("inspect Driver control bindings: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("GAP-FD-003: synthetic flowbee-control session bindings cannot authorize the Driver control principal; remove %d synthetic binding(s)", count)
	}
	return nil
}

func driverControlReadiness(ctx context.Context, v2Enabled bool, port driver.DriverPort) api.DriverControlReadiness {
	if !v2Enabled {
		return api.DriverControlReadiness{Status: "disabled"}
	}
	if port == nil {
		return api.DriverControlReadiness{
			Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver control-principal capability was not initialized; actions remain durably held.",
		}
	}
	capability, err := port.ControlOriginCapability(ctx)
	if err != nil {
		return api.DriverControlReadiness{
			Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver control-principal capability is unavailable or unauthorized: " + err.Error(),
		}
	}
	return api.DriverControlReadiness{
		Required: true, Available: true, Status: "ready",
		Reason: "Tmux Driver authenticated control-principal origin ready for " + capability.PrincipalID + ".",
	}
}

// selectDurableEpicReviewHandoffV2 makes the session-control boundary a
// writer-owned database fact instead of process-local environment memory. An
// absent environment variable reuses the last selected mode; 1 and 0 are
// explicit activation/rollback requests and are persisted while serve holds the
// exclusive writer lock. Offline mutating CLIs consult the same fact before
// they can reach any legacy raw-tmux implementation.
func selectDurableEpicReviewHandoffV2(ctx context.Context, st *store.Store) (bool, error) {
	persisted, err := st.DurableEpicReviewHandoffV2(ctx)
	if err != nil {
		return true, fmt.Errorf("read durable epic-review-handoff v2 activation: %w", err)
	}
	raw, explicit := os.LookupEnv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2")
	if !explicit {
		return persisted, nil
	}
	var selected bool
	switch strings.TrimSpace(raw) {
	case "1":
		selected = true
	case "", "0":
		selected = false
	default:
		return true, fmt.Errorf("FLOWBEE_EPIC_REVIEW_HANDOFF_V2 must be 0 or 1")
	}
	if err := st.SetDurableEpicReviewHandoffV2(ctx, selected); err != nil {
		return true, fmt.Errorf("persist epic-review-handoff v2 activation: %w", err)
	}
	return selected, nil
}

func readOwnerOnlySecret(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return "", errors.New("open secret file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file must be regular and owner-only (mode 0600 or stricter)")
	}
	if info.Size() <= 0 || info.Size() > 8192 {
		return "", fmt.Errorf("secret file has invalid size %d", info.Size())
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil {
		return "", err
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", errors.New("secret file is empty")
	}
	return secret, nil
}

// runServe boots the control plane: load config -> open store -> migrate ->
// open health + private listeners -> block until signal -> graceful shutdown.
// printServeSystemd emits a ready-to-install systemd unit + env file so the CONTROL
// PLANE runs as a managed service — the same durability the fleet gets (clean
// `systemctl restart`, reboot survival, Restart=always, journald logs). The control
// plane is the most critical component; running it under bare nohup is the one
// production gap left after the fleet got systemd. Secrets print as placeholders.
func printServeSystemd() {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	user := envOr("USER", "sam")
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/home/" + user
	}
	envPath := home + "/.flowbee/serve.env"
	cfgPath := envOr("FLOWBEE_CONFIG", home+"/.flowbee/flowbee.yaml")

	fmt.Printf("# 1. Write %s  (chmod 600 — holds the GitHub token):\n", envPath)
	fmt.Printf("FLOWBEE_CONFIG=%s\n", cfgPath)
	fmt.Printf("FLOWBEE_GITHUB_TOKEN=<your-github-token>\n")
	if v := os.Getenv("FLOWBEE_MIRROR_PATH"); v != "" {
		fmt.Printf("FLOWBEE_MIRROR_PATH=%s\n", v)
	}
	if v := os.Getenv("FLOWBEE_GIT_REMOTE"); v != "" {
		fmt.Printf("FLOWBEE_GIT_REMOTE=%s\n", v)
	}
	if os.Getenv("FLOWBEE_ALLOW_SELF_MERGE") != "" {
		fmt.Printf("FLOWBEE_ALLOW_SELF_MERGE=1\n")
	}
	if os.Getenv("FLOWBEE_WEBHOOK_SECRET") != "" {
		fmt.Printf("FLOWBEE_WEBHOOK_SECRET=<webhook-secret>\n")
	}
	// Security stanza — REQUIRED for the unit to start: the worker API binds a
	// non-loopback addr by default, so `flowbee serve` REFUSES TO START without either
	// worker auth or an explicit insecure opt-in. Carry the operator's actual choice
	// into the env so the installed unit boots; if neither is set, default to the
	// trusted-private-network opt-in with a loud warning (the common single-operator
	// deployment) rather than emitting a unit that dies on `systemctl enable`. systemd
	// EnvironmentFile honors full-line `#` comments, so the guidance lives inline.
	switch {
	case os.Getenv("FLOWBEE_WORKER_AUTH_SECRET") != "":
		fmt.Printf("FLOWBEE_WORKER_AUTH_SECRET=<shared-worker-secret>\n")
		if v := os.Getenv("FLOWBEE_ENROLLED_IDENTITIES"); v != "" {
			fmt.Printf("FLOWBEE_ENROLLED_IDENTITIES=%s\n", v)
		} else {
			fmt.Printf("# enroll each worker: run `flowbee token --identity <name>` and list them here:\n")
			fmt.Printf("# FLOWBEE_ENROLLED_IDENTITIES=worker-a,worker-b\n")
		}
	case os.Getenv("FLOWBEE_INSECURE") != "":
		fmt.Printf("# OPEN worker API — trusted private network (e.g. Tailscale) ONLY:\n")
		fmt.Printf("FLOWBEE_INSECURE=1\n")
	default:
		fmt.Printf("# Pick ONE. Trusted private network (e.g. Tailscale)? keep FLOWBEE_INSECURE=1.\n")
		fmt.Printf("# Otherwise DELETE it and set FLOWBEE_WORKER_AUTH_SECRET +\n")
		fmt.Printf("# FLOWBEE_ENROLLED_IDENTITIES (run `flowbee token --identity <name>` per worker):\n")
		fmt.Printf("FLOWBEE_INSECURE=1\n")
	}
	if os.Getenv("FLOWBEE_EPIC_REVIEW_HANDOFF_V2") == "1" {
		fmt.Printf("FLOWBEE_EPIC_REVIEW_HANDOFF_V2=1\n")
		for _, key := range []string{
			"FLOWBEE_ALERT_WEBHOOK_URL", "FLOWBEE_ALERT_WEBHOOK_SECRET_FILE",
			"FLOWBEE_EXTERNAL_WATCHDOG_ID", "FLOWBEE_DRIVER_SOCKET",
			"FLOWBEE_DRIVER_TOKEN_FILE", "FLOWBEE_DRIVER_INSTANCE_REF",
		} {
			if value := os.Getenv(key); value != "" {
				fmt.Printf("%s=%s\n", key, value)
			}
		}
	}
	if os.Getenv("FLOWBEE_CAPACITY_ROUTING_V2") == "1" || os.Getenv("FLOWBEE_CAPACITY_V2") == "1" {
		fmt.Printf("FLOWBEE_CAPACITY_ROUTING_V2=1\n")
		for _, key := range []string{"FLOWBEE_CAPACITY_LOCAL_HOST_ID", "FLOWBEE_CAPACITY_COLLECTOR_ID", "FLOWBEE_CAPACITY_COLLECT_INTERVAL"} {
			if value := os.Getenv(key); value != "" {
				fmt.Printf("%s=%s\n", key, value)
			}
		}
	}
	if os.Getenv("FLOWBEE_PHASE1_DASHBOARD") == "1" {
		fmt.Printf("FLOWBEE_PHASE1_DASHBOARD=1\n")
		for _, key := range []string{"FLOWBEE_HUMAN_SESSION_KEY_FILE", "FLOWBEE_HUMAN_GRANTS_FILE"} {
			if value := os.Getenv(key); value != "" {
				fmt.Printf("%s=%s\n", key, value)
			}
		}
	}
	fmt.Printf("\n# 2. Write /etc/systemd/system/flowbee-serve.service:\n")
	fmt.Printf(`[Unit]
Description=Flowbee control plane
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
EnvironmentFile=%s
ExecStart=%s serve
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, user, envPath, self)
	fmt.Printf("\n# 3. Enable + start (clean restart any time with `systemctl restart flowbee-serve`):\n")
	fmt.Printf("sudo systemctl daemon-reload && sudo systemctl enable --now flowbee-serve\n")
	fmt.Printf("journalctl -u flowbee-serve -f   # tail logs; the startup line shows the build SHA\n")
}

func runServe(args []string) error {
	for _, a := range args {
		if a == "--systemd" {
			printServeSystemd()
			return nil
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	contract, err := actorprotocol.Load()
	if err != nil {
		return fmt.Errorf("validate embedded actor protocol: %w", err)
	}
	contractHash, err := actorprotocol.BundleHash()
	if err != nil {
		return fmt.Errorf("hash embedded actor protocol: %w", err)
	}
	logger.Info("actor protocol validated", "protocol", contract.Protocol.ID,
		"version", contract.Version(), "bundle_hash", contractHash)

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// SIGUSR1/SIGHUP = graceful RE-EXEC: an in-place restart that re-reads flowbee.yaml. It
	// cancels ctx so every listener + loop shuts down cleanly — the SAME path as SIGTERM — then
	// replaces the process image (reexecSelf). This is a full, clean config reload (the
	// `flowbee repo add --reload` path) with NONE of the concurrent-mutation risk of live-
	// rewiring the per-repo managers. SIGUSR1 because a nohup-launched CP ignores SIGHUP.
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()
	reexec := make(chan struct{}, 1)
	go func() {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP, syscall.SIGUSR1)
		defer signal.Stop(hup)
		select {
		case <-hup:
			reexec <- struct{}{}
			cancel()
		case <-ctx.Done():
		}
	}()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.AcquireWriterLock(); err != nil {
		return err
	}

	if err := store.MigrateUp(ctx, st.DB); err != nil {
		return err
	}
	logger.Info("migrations applied")

	// self-provision the control-plane mirror from the GitHub token if it's
	// configured but absent — so the operator need not pre-clone it, and a PRIVATE
	// repo works (a plain `git clone` would fail for lack of credentials).
	ensureControlMirror(logger, cfg)

	st.NoEligibleWorkerDelay = cfg.NoEligibleWorker()
	// V2 activation is durable: after the first explicit 1, an omitted environment
	// variable cannot silently reopen legacy raw-tmux control. An explicit 0 while
	// holding the writer lock is the rollback operation.
	st.EnableEpicReviewHandoffV2, err = selectDurableEpicReviewHandoffV2(ctx, st)
	if err != nil {
		return err
	}
	st.EnableCapacityV2 = os.Getenv("FLOWBEE_CAPACITY_ROUTING_V2") == "1" ||
		os.Getenv("FLOWBEE_CAPACITY_V2") == "1"
	phase1Enabled := os.Getenv("FLOWBEE_PHASE1_DASHBOARD") == "1"
	driverControl := driverControlReadiness(ctx, st.EnableEpicReviewHandoffV2, nil)
	controlState := newDriverControlState(driverControl)
	// A database binding is inventory, not authority. Keep every Flowbee-authored
	// message seam closed until Driver negotiates a real non-session control
	// origin (GAP-FD-003). Observation and lifecycle control remain available.
	st.EnableDriverControlOrigin = driverControl.Available
	st.DriverControlOriginGate = controlState.Available
	var capacityInterval time.Duration
	if st.EnableCapacityV2 && !st.EnableEpicReviewHandoffV2 {
		return fmt.Errorf("capacity routing v2 requires FLOWBEE_EPIC_REVIEW_HANDOFF_V2=1")
	}
	if st.EnableCapacityV2 {
		capacityInterval, err = capacityCollectorInterval()
		if err != nil {
			return err
		}
	}
	if phase1Enabled && !st.EnableEpicReviewHandoffV2 {
		return fmt.Errorf("Phase 1 dashboard automation requires FLOWBEE_EPIC_REVIEW_HANDOFF_V2=1")
	}
	var v2Reconcilers *durableReconcilerSet
	if st.EnableEpicReviewHandoffV2 {
		if err := epicflow.ValidateRegistry(); err != nil {
			return fmt.Errorf("epic v2 recovery contract: %w", err)
		}
		alertURL, alertSecretFile := os.Getenv("FLOWBEE_ALERT_WEBHOOK_URL"), os.Getenv("FLOWBEE_ALERT_WEBHOOK_SECRET_FILE")
		if alertURL == "" || alertSecretFile == "" {
			return fmt.Errorf("epic review handoff v2 requires FLOWBEE_ALERT_WEBHOOK_URL and owner-only FLOWBEE_ALERT_WEBHOOK_SECRET_FILE")
		}
		alertSecret, alertSecretErr := readOwnerOnlySecret(alertSecretFile)
		if alertSecretErr != nil {
			return fmt.Errorf("read alert webhook secret: %w", alertSecretErr)
		}
		if os.Getenv("FLOWBEE_EXTERNAL_WATCHDOG_ID") == "" {
			return fmt.Errorf("epic review handoff v2 requires FLOWBEE_EXTERNAL_WATCHDOG_ID")
		}
		driverSocket, driverTokenFile := os.Getenv("FLOWBEE_DRIVER_SOCKET"), os.Getenv("FLOWBEE_DRIVER_TOKEN_FILE")
		if driverSocket == "" || driverTokenFile == "" {
			return fmt.Errorf("epic review handoff v2 requires FLOWBEE_DRIVER_SOCKET and FLOWBEE_DRIVER_TOKEN_FILE")
		}
		driverToken, tokenErr := readOwnerOnlySecret(driverTokenFile)
		if tokenErr != nil {
			return fmt.Errorf("read Driver control token: %w", tokenErr)
		}
		driverPort := driver.NewUDSPort(driverSocket, driverToken)
		if err := driverPort.Check(ctx); err != nil {
			return fmt.Errorf("tmux-driver readiness: %w", err)
		}
		driverControl = probeDriverControlState(ctx, controlState, st.DB, driverPort)
		st.EnableDriverControlOrigin = driverControl.Available
		reconcilerOwner := fmt.Sprintf("serve-%d", os.Getpid())
		reconcilerGrace := map[string]time.Duration{
			"review_handoff":            3 * time.Minute,
			"review_verdict":            3 * time.Minute,
			"delivery_backstop":         3 * time.Minute,
			"alert_drainer":             time.Minute,
			"driver_observer":           30 * time.Second,
			"driver_control_capability": 30 * time.Second,
			"driver_executor":           30 * time.Second,
			"builder_lifecycle":         30 * time.Second,
			"builder_launch":            30 * time.Second,
			"epic_effects":              30 * time.Second,
			"project_breaker_probe":     time.Minute,
			"reconciler_watchdog":       time.Minute,
		}
		if phase1Enabled {
			reconcilerGrace["work_intent_promotion"] = time.Minute
			reconcilerGrace["work_intent_driver"] = 30 * time.Second
			reconcilerGrace["work_intent_admission"] = time.Minute
			reconcilerGrace["decision_response_driver"] = 30 * time.Second
			reconcilerGrace["conversation_driver"] = 30 * time.Second
		}
		if st.EnableCapacityV2 {
			reconcilerGrace["capacity_pools"] = time.Minute
			// The external watchdog must not call a healthy non-default cadence
			// overdue. Conversely, this grace remains below the five-minute
			// route-freshness window for every accepted cadence, so a dead
			// collector is visible before the scheduler ages out all capacity.
			reconcilerGrace["capacity_collector"] = capacityInterval + 30*time.Second
		}
		v2Reconcilers, tokenErr = beginDurableReconcilers(ctx, st, reconcilerOwner, time.Now(), reconcilerGrace)
		if tokenErr != nil {
			return fmt.Errorf("initialize v2 reconciler supervision: %w", tokenErr)
		}
		if driverControl.Available {
			logger.Info("epic review handoff v2 enabled with authenticated Driver control-principal routing",
				"driver_control_status", driverControl.Status)
		} else {
			logger.Warn("epic review handoff v2 enabled with control-plane Driver messaging held",
				"driver_control_status", driverControl.Status, "contract_gap", driverControl.Gap,
				"reason", driverControl.Reason)
			if phase1Enabled {
				logger.Warn("Phase 1 dashboard enabled; Flowbee-authored Driver automation is not live",
					"driver_control_status", driverControl.Status, "contract_gap", driverControl.Gap)
			}
		}
		// Re-prove both the advertised feature and this bearer token's exact
		// control-principal capability throughout the process lifetime. A failed
		// probe atomically fences new materialization/claims; a later exact proof
		// reopens them. The supervised loop itself remains healthy while held so
		// observation/lifecycle and effect verification continue.
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					before := controlState.Snapshot()
					var after api.DriverControlReadiness
					err := v2Reconcilers.tick(ctx, "driver_control_capability", now, func() error {
						after = probeDriverControlState(ctx, controlState, st.DB, driverPort)
						return nil
					})
					if err != nil {
						logger.Error("Driver control capability supervision failed", "err", err)
						continue
					}
					if before.Available != after.Available || before.Status != after.Status || before.Reason != after.Reason {
						if after.Available {
							logger.Info("Driver control-principal capability restored; pending sends may resume")
						} else {
							logger.Warn("Driver control-principal capability lost; new sends held",
								"status", after.Status, "contract_gap", after.Gap, "reason", after.Reason)
						}
					}
				}
			}
		}()
		driverInstanceRef := os.Getenv("FLOWBEE_DRIVER_INSTANCE_REF")
		if driverInstanceRef == "" {
			driverInstanceRef = "local-driver"
		}
		driverObserver := driver.ObservationIngestor{InstanceRef: driverInstanceRef, Port: driverPort,
			Store: driver.ObservationSQLStore{DB: st.DB}}
		var observationRep driver.ObservationFoldResult
		startupNow := time.Now()
		if oerr := v2Reconcilers.tick(ctx, "driver_observer", startupNow, func() error {
			var err error
			observationRep, err = driverObserver.Tick(ctx)
			return err
		}); oerr != nil {
			return fmt.Errorf("startup tmux-driver observation reconcile: %w", oerr)
		}
		logger.Info("startup tmux-driver observation reconcile complete",
			"events", observationRep.Inserted, "deduplicated", observationRep.Deduplicated,
			"snapshot_replaced", observationRep.SnapshotReplaced, "store_reset", observationRep.StoreReset)
		go func() {
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					var rep driver.ObservationFoldResult
					err := v2Reconcilers.tick(ctx, "driver_observer", now, func() error {
						var err error
						rep, err = driverObserver.Tick(ctx)
						return err
					})
					if err != nil {
						logger.Error("tmux-driver observation reconcile failed", "err", err)
					} else if rep.Inserted+rep.Deduplicated > 0 || rep.SnapshotReplaced || rep.StoreReset || rep.CursorGap {
						logger.Info("tmux-driver observation reconcile complete", "events", rep.Inserted,
							"deduplicated", rep.Deduplicated, "snapshot_replaced", rep.SnapshotReplaced,
							"store_reset", rep.StoreReset, "cursor_gap", rep.CursorGap, "caught_up", rep.CaughtUp)
					}
				}
			}
		}()
		var rep store.EpicReviewReconcileResult
		startupNow = time.Now()
		rerr := v2Reconcilers.tick(ctx, "review_handoff", startupNow, func() error {
			var err error
			rep, err = st.ReconcileEpicReviewHandoffs(ctx, startupNow, 5*time.Minute)
			return err
		})
		if rerr != nil {
			return fmt.Errorf("startup epic review handoff reconcile: %w", rerr)
		}
		logger.Info("startup epic review handoff reconcile complete", "scanned", rep.Scanned, "dispatched", rep.Dispatched)
		var verdictRep store.EpicReviewVerdictStallResult
		startupNow = time.Now()
		rerr = v2Reconcilers.tick(ctx, "review_verdict", startupNow, func() error {
			var err error
			verdictRep, err = st.ReconcileEpicReviewVerdictStalls(ctx, startupNow, 20*time.Minute, 3)
			return err
		})
		if rerr != nil {
			return fmt.Errorf("startup epic review verdict reconcile: %w", rerr)
		}
		logger.Info("startup epic review verdict reconcile complete", "scanned", verdictRep.Scanned,
			"requeued", verdictRep.Requeued, "escalated", verdictRep.Escalated)
		var backstopRep store.EpicDeliveryBackstopResult
		startupNow = time.Now()
		rerr = v2Reconcilers.tick(ctx, "delivery_backstop", startupNow, func() error {
			var err error
			backstopRep, err = st.ReconcileEpicDeliveryBackstops(ctx, startupNow)
			return err
		})
		if rerr != nil {
			return fmt.Errorf("startup epic delivery backstop: %w", rerr)
		}
		logger.Info("startup epic delivery backstop complete", "scanned", backstopRep.Scanned, "alerted", backstopRep.Alerted)
		alertDrainer := alerting.Drainer{
			Store: st, Sink: alerting.WebhookSink{URL: alertURL, Secret: alertSecret},
			Owner: fmt.Sprintf("serve-%d", os.Getpid()), ClaimTTL: time.Minute,
			MaximumTries: 5, Batch: 50,
		}
		var alertRep alerting.Report
		startupNow = time.Now()
		if derr := v2Reconcilers.tick(ctx, "alert_drainer", startupNow, func() error {
			var err error
			alertRep, err = alertDrainer.Tick(ctx, startupNow)
			return err
		}); derr != nil {
			logger.Error("startup alert drain failed", "err", derr)
		} else if alertRep.Published+alertRep.Retried+alertRep.DeadLettered > 0 {
			logger.Info("startup alert drain complete", "published", alertRep.Published, "retried", alertRep.Retried, "dead_lettered", alertRep.DeadLettered)
		}
		if st.EnableCapacityV2 {
			capacityRuntime, capacityErr := newProductionCapacityCollector(st, cfg,
				os.Getenv("FLOWBEE_CAPACITY_LOCAL_HOST_ID"), os.Getenv("FLOWBEE_CAPACITY_COLLECTOR_ID"))
			if capacityErr != nil {
				return fmt.Errorf("initialize capacity collector: %w", capacityErr)
			}
			startupNow = time.Now()
			var startupGeneration store.CapacityGeneration
			if capacityErr := v2Reconcilers.tick(ctx, "capacity_collector", startupNow, func() error {
				var err error
				startupGeneration, err = capacityRuntime.collect(ctx, startupNow)
				return err
			}); capacityErr != nil {
				return fmt.Errorf("startup live capacity collection: %w", capacityErr)
			}
			logger.Info("startup live capacity generation committed", "generation", startupGeneration.ID,
				"seats", len(startupGeneration.ExpectedSeatIDs), "observations", len(startupGeneration.Observations),
				"interval", capacityInterval.String())
			go func() {
				ticker := time.NewTicker(capacityInterval)
				defer ticker.Stop()
				runCapacityCollectorLoop(ctx, ticker.C, func(now time.Time) {
					var generation store.CapacityGeneration
					err := v2Reconcilers.tick(ctx, "capacity_collector", now, func() error {
						var err error
						generation, err = capacityRuntime.collect(ctx, now)
						return err
					})
					if err != nil {
						logger.Error("live capacity collection failed; active generation will age out fail-closed", "err", err)
					} else {
						logger.Info("live capacity generation committed", "generation", generation.ID,
							"seats", len(generation.ExpectedSeatIDs), "observations", len(generation.Observations))
					}
				})
			}()

			buildProvider := envOr("FLOWBEE_BUILD_PROVIDER", "codex")
			reviewProvider := envOr("FLOWBEE_REVIEW_PROVIDER", "grok")
			operationsProvider := envOr("FLOWBEE_OPERATIONS_PROVIDER", "grok")
			reconcilePools := func(now time.Time) (store.CapacityPoolReconcileResult, error) {
				requirements, err := st.CapacityPoolDemand(ctx, buildProvider, reviewProvider, operationsProvider)
				if err != nil {
					return store.CapacityPoolReconcileResult{}, err
				}
				return st.ReconcileCapacityPools(ctx, requirements, now, 5*time.Minute, 15*time.Minute)
			}
			startupNow = time.Now()
			var startupPoolRep store.CapacityPoolReconcileResult
			if perr := v2Reconcilers.tick(ctx, "capacity_pools", startupNow, func() error {
				var err error
				startupPoolRep, err = reconcilePools(startupNow)
				return err
			}); perr != nil {
				return fmt.Errorf("startup capacity-pool reconcile: %w", perr)
			}
			logger.Info("startup capacity-pool reconcile complete", "checked", startupPoolRep.Checked,
				"pending", startupPoolRep.Pending, "alerted", startupPoolRep.Alerted,
				"resolved", startupPoolRep.Resolved)
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var rep store.CapacityPoolReconcileResult
						err := v2Reconcilers.tick(ctx, "capacity_pools", now, func() error {
							var err error
							rep, err = reconcilePools(now)
							return err
						})
						if err != nil {
							logger.Error("capacity-pool reconcile failed", "err", err)
						} else if rep.Pending+rep.Alerted+rep.Resolved > 0 {
							logger.Info("capacity-pool reconcile pass", "checked", rep.Checked,
								"pending", rep.Pending, "alerted", rep.Alerted, "resolved", rep.Resolved)
						}
					}
				}
			}()
		}
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					var rep alerting.Report
					err := v2Reconcilers.tick(ctx, "alert_drainer", now, func() error {
						var err error
						rep, err = alertDrainer.Tick(ctx, now)
						return err
					})
					if err != nil {
						logger.Error("alert drain failed", "err", err)
					} else if rep.Published+rep.Retried+rep.DeadLettered > 0 {
						logger.Info("alert drain complete", "published", rep.Published, "retried", rep.Retried, "dead_lettered", rep.DeadLettered)
					}
				}
			}
		}()
		driverRuntime := driver.Runtime{
			Port: driverPort, Store: driver.SQLActionStore{DB: st.DB,
				ControlOriginAvailable: driverControl.Available, ControlOriginGate: controlState.Available},
			Evidence: driver.SQLStageEvidence{DB: st.DB},
			Owner:    fmt.Sprintf("serve-driver-%d", os.Getpid()), ClaimTTL: time.Minute,
			MaximumTries: 5,
		}
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					var rep driver.RuntimeReport
					err := v2Reconcilers.tick(ctx, "driver_executor", now, func() error {
						var err error
						rep, err = driverRuntime.Tick(ctx, now)
						return err
					})
					if err != nil {
						logger.Error("Driver action runtime failed", "err", err)
					} else if rep.Delivered+rep.Verified+rep.Retried+rep.DeadLettered > 0 {
						logger.Info("Driver action runtime pass", "delivered", rep.Delivered,
							"verified", rep.Verified, "retried", rep.Retried, "dead_lettered", rep.DeadLettered)
					}
				}
			}
		}()
		lifecycleRuntime := driver.LifecycleRuntime{
			Port: driverPort, Store: driver.SQLActionStore{DB: st.DB}, Projector: driverbridge.Projector{Store: st},
			Owner: fmt.Sprintf("serve-lifecycle-%d", os.Getpid()), ClaimTTL: time.Minute,
			MaximumTries: 5,
		}
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					var rep driver.LifecycleRuntimeReport
					err := v2Reconcilers.tick(ctx, "builder_lifecycle", now, func() error {
						var err error
						rep, err = lifecycleRuntime.Tick(ctx, now)
						return err
					})
					if err != nil {
						logger.Error("builder lifecycle runtime failed", "err", err)
					} else if rep.Reclaimed+rep.Verified+rep.Executed+rep.Held+rep.Retried+rep.DeadLettered > 0 {
						logger.Info("builder lifecycle runtime pass", "reclaimed", rep.Reclaimed,
							"verified", rep.Verified, "executed", rep.Executed,
							"held", rep.Held,
							"retried", rep.Retried, "dead_lettered", rep.DeadLettered)
					}
				}
			}
		}()
		launchProvider := envOr("FLOWBEE_BUILD_PROVIDER", "codex")
		var startupLaunchRep store.BuilderLaunchReconcileResult
		startupNow = time.Now()
		if launchErr := v2Reconcilers.tick(ctx, "builder_launch", startupNow, func() error {
			var err error
			startupLaunchRep, err = st.ReconcileBuilderLaunches(ctx, startupNow,
				5*time.Minute, launchProvider, 5)
			return err
		}); launchErr != nil {
			return fmt.Errorf("startup builder-launch reconcile: %w", launchErr)
		}
		logger.Info("startup builder-launch reconcile complete", "scanned", startupLaunchRep.Scanned,
			"actions_created", startupLaunchRep.ActionsCreated, "acknowledged", startupLaunchRep.Acknowledged,
			"capacity_held", startupLaunchRep.CapacityHeld, "stalled", startupLaunchRep.Stalled)
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					var rep store.BuilderLaunchReconcileResult
					err := v2Reconcilers.tick(ctx, "builder_launch", now, func() error {
						var err error
						rep, err = st.ReconcileBuilderLaunches(ctx, now, 5*time.Minute, launchProvider, 5)
						return err
					})
					if err != nil {
						logger.Error("builder-launch reconcile failed", "err", err)
					} else if rep.ActionsCreated+rep.Acknowledged+rep.CapacityHeld+rep.Stalled > 0 {
						logger.Info("builder-launch reconcile pass", "scanned", rep.Scanned,
							"actions_created", rep.ActionsCreated, "acknowledged", rep.Acknowledged,
							"capacity_held", rep.CapacityHeld, "stalled", rep.Stalled)
					}
				}
			}
		}()
		if phase1Enabled {
			conversationRuntime := driver.ConversationRuntime{
				Port: driverPort, Store: driver.ConversationSQLStore{DB: st.DB,
					ControlOriginAvailable: driverControl.Available, ControlOriginGate: controlState.Available},
				Evidence: driver.ConversationStageEvidence{DB: st.DB},
				Owner:    fmt.Sprintf("serve-conversation-%d", os.Getpid()), ClaimTTL: time.Minute,
				AcknowledgementTTL: 10 * time.Minute,
				MaximumTries:       5,
			}
			var startupConversationProjection store.ConversationDeliveryReconcileReport
			var startupConversationRuntime driver.ConversationRuntimeReport
			startupNow = time.Now()
			if cerr := v2Reconcilers.tick(ctx, "conversation_driver", startupNow, func() error {
				var err error
				startupConversationProjection, err = st.ReconcileConversationMessageActions(ctx, startupNow)
				if err != nil {
					return err
				}
				startupConversationRuntime, err = conversationRuntime.Tick(ctx, startupNow)
				return err
			}); cerr != nil {
				return fmt.Errorf("startup conversation Driver reconcile: %w", cerr)
			}
			logger.Info("startup conversation Driver reconcile complete",
				"materialized", startupConversationProjection.ActionsCreated,
				"route_holds", startupConversationProjection.RoutesHeld,
				"fenced", startupConversationRuntime.Fenced,
				"reclaimed", startupConversationRuntime.Reclaimed,
				"held", startupConversationRuntime.Held,
				"delivered", startupConversationRuntime.Delivered,
				"verified", startupConversationRuntime.Verified,
				"retried", startupConversationRuntime.Retried,
				"dead_lettered", startupConversationRuntime.DeadLettered)
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var projection store.ConversationDeliveryReconcileReport
						var transport driver.ConversationRuntimeReport
						err := v2Reconcilers.tick(ctx, "conversation_driver", now, func() error {
							var err error
							projection, err = st.ReconcileConversationMessageActions(ctx, now)
							if err != nil {
								return err
							}
							transport, err = conversationRuntime.Tick(ctx, now)
							return err
						})
						if err != nil {
							logger.Error("conversation Driver reconcile failed", "err", err)
						} else if projection.ActionsCreated+projection.RoutesHeld+transport.Fenced+
							transport.Reclaimed+transport.Held+transport.Delivered+transport.Verified+
							transport.Retried+transport.DeadLettered > 0 {
							logger.Info("conversation Driver reconcile pass",
								"materialized", projection.ActionsCreated, "route_holds", projection.RoutesHeld,
								"fenced", transport.Fenced, "reclaimed", transport.Reclaimed,
								"held", transport.Held, "delivered", transport.Delivered,
								"verified", transport.Verified, "retried", transport.Retried,
								"dead_lettered", transport.DeadLettered)
						}
					}
				}
			}()
			decisionRuntime := driver.DecisionResponseRuntime{
				Port: driverPort, Store: driver.DecisionResponseSQLStore{DB: st.DB,
					ControlOriginAvailable: driverControl.Available, ControlOriginGate: controlState.Available}, Domain: st,
				Evidence: driver.DecisionResponseStageEvidence{DB: st.DB},
				Owner:    fmt.Sprintf("serve-decision-response-%d", os.Getpid()), ClaimTTL: time.Minute,
				AcknowledgementTTL: 10 * time.Minute,
				MaximumTries:       5,
			}
			var startupDecisionRep driver.DecisionResponseRuntimeReport
			startupNow = time.Now()
			if derr := v2Reconcilers.tick(ctx, "decision_response_driver", startupNow, func() error {
				var err error
				startupDecisionRep, err = decisionRuntime.Tick(ctx, startupNow)
				return err
			}); derr != nil {
				return fmt.Errorf("startup decision-response Driver reconcile: %w", derr)
			}
			logger.Info("startup decision-response Driver reconcile complete",
				"materialized", startupDecisionRep.Materialized, "fenced", startupDecisionRep.Fenced,
				"reclaimed", startupDecisionRep.Reclaimed, "held", startupDecisionRep.Held,
				"delivered", startupDecisionRep.Delivered, "verified", startupDecisionRep.Verified,
				"retried", startupDecisionRep.Retried, "dead_lettered", startupDecisionRep.DeadLettered)
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var rep driver.DecisionResponseRuntimeReport
						err := v2Reconcilers.tick(ctx, "decision_response_driver", now, func() error {
							var err error
							rep, err = decisionRuntime.Tick(ctx, now)
							return err
						})
						if err != nil {
							logger.Error("decision-response Driver reconcile failed", "err", err)
						} else if rep.Materialized+rep.Fenced+rep.Reclaimed+rep.Held+rep.Delivered+
							rep.Verified+rep.Retried+rep.DeadLettered > 0 {
							logger.Info("decision-response Driver reconcile pass", "materialized", rep.Materialized,
								"fenced", rep.Fenced, "reclaimed", rep.Reclaimed, "held", rep.Held,
								"delivered", rep.Delivered, "verified", rep.Verified,
								"retried", rep.Retried, "dead_lettered", rep.DeadLettered)
						}
					}
				}
			}()
			var startupIntentRep store.WorkIntentReconcileResult
			startupNow = time.Now()
			if ierr := v2Reconcilers.tick(ctx, "work_intent_promotion", startupNow, func() error {
				var err error
				startupIntentRep, err = st.ReconcileWorkIntents(ctx, startupNow, 10*time.Minute)
				return err
			}); ierr != nil {
				return fmt.Errorf("startup work-intent promotion reconcile: %w", ierr)
			}
			logger.Info("startup work-intent promotion complete", "scanned", startupIntentRep.Scanned,
				"advanced", startupIntentRep.Advanced, "actions_created", startupIntentRep.ActionsCreated,
				"held", startupIntentRep.Held)
			intentRuntime := driver.WorkIntentRuntime{
				Port: driverPort, Store: driver.WorkIntentSQLStore{DB: st.DB,
					ControlOriginAvailable: driverControl.Available, ControlOriginGate: controlState.Available},
				Evidence: driver.SQLStageEvidence{DB: st.DB},
				Owner:    fmt.Sprintf("serve-work-intent-%d", os.Getpid()), ClaimTTL: time.Minute,
				AcknowledgementTTL: 10 * time.Minute,
				MaximumTries:       5,
			}
			var startupDriverRep driver.WorkIntentRuntimeReport
			startupNow = time.Now()
			if ierr := v2Reconcilers.tick(ctx, "work_intent_driver", startupNow, func() error {
				var err error
				startupDriverRep, err = intentRuntime.Tick(ctx, startupNow)
				return err
			}); ierr != nil {
				return fmt.Errorf("startup work-intent Driver reconcile: %w", ierr)
			}
			logger.Info("startup work-intent Driver reconcile complete",
				"fenced", startupDriverRep.Fenced, "reclaimed", startupDriverRep.Reclaimed,
				"held", startupDriverRep.Held,
				"delivered", startupDriverRep.Delivered, "verified", startupDriverRep.Verified,
				"retried", startupDriverRep.Retried, "dead_lettered", startupDriverRep.DeadLettered)
			var startupAdmissionRep store.WorkIntentAdmissionReconcileResult
			startupNow = time.Now()
			if ierr := v2Reconcilers.tick(ctx, "work_intent_admission", startupNow, func() error {
				var err error
				startupAdmissionRep, err = st.ReconcileWorkIntentAdmissions(ctx, startupNow)
				return err
			}); ierr != nil {
				return fmt.Errorf("startup work-intent admission reconcile: %w", ierr)
			}
			logger.Info("startup work-intent admission reconcile complete",
				"scanned", startupAdmissionRep.Scanned, "admitted", startupAdmissionRep.Admitted,
				"held", startupAdmissionRep.Held)
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var transportRep driver.WorkIntentRuntimeReport
						err := v2Reconcilers.tick(ctx, "work_intent_driver", now, func() error {
							var err error
							transportRep, err = intentRuntime.Tick(ctx, now)
							return err
						})
						if err != nil {
							logger.Error("work-intent Driver reconcile failed", "err", err)
						} else if transportRep.Fenced+transportRep.Reclaimed+transportRep.Held+transportRep.Delivered+
							transportRep.Verified+transportRep.Retried+transportRep.DeadLettered > 0 {
							logger.Info("work-intent Driver reconcile pass", "fenced", transportRep.Fenced,
								"reclaimed", transportRep.Reclaimed, "held", transportRep.Held,
								"delivered", transportRep.Delivered,
								"verified", transportRep.Verified, "retried", transportRep.Retried,
								"dead_lettered", transportRep.DeadLettered)
						}
					}
				}
			}()
			go func() {
				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var rep store.WorkIntentReconcileResult
						err := v2Reconcilers.tick(ctx, "work_intent_promotion", now, func() error {
							var err error
							rep, err = st.ReconcileWorkIntents(ctx, now, 10*time.Minute)
							return err
						})
						if err != nil {
							logger.Error("work-intent promotion failed", "err", err)
						} else if rep.Advanced+rep.ActionsCreated+rep.Held > 0 {
							logger.Info("work-intent promotion pass", "scanned", rep.Scanned,
								"advanced", rep.Advanced, "actions_created", rep.ActionsCreated, "held", rep.Held)
						}
					}
				}
			}()
			go func() {
				ticker := time.NewTicker(15 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var rep store.WorkIntentAdmissionReconcileResult
						err := v2Reconcilers.tick(ctx, "work_intent_admission", now, func() error {
							var err error
							rep, err = st.ReconcileWorkIntentAdmissions(ctx, now)
							return err
						})
						if err != nil {
							logger.Error("work-intent admission failed", "err", err)
						} else if rep.Admitted+rep.Held > 0 {
							logger.Info("work-intent admission pass", "scanned", rep.Scanned,
								"admitted", rep.Admitted, "held", rep.Held)
						}
					}
				}
			}()
		}
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					var rep store.ReconcilerWatchdogResult
					err := v2Reconcilers.tick(ctx, "reconciler_watchdog", now, func() error {
						var err error
						rep, err = st.ReconcileStaleReconcilers(ctx, now)
						return err
					})
					if err != nil {
						logger.Error("v2 reconciler watchdog failed", "err", err)
					} else if rep.Alerted > 0 {
						logger.Error("v2 reconcilers declared stale", "scanned", rep.Scanned, "alerted", rep.Alerted)
					}
				}
			}
		}()
	}
	// §6.7 per-job cost circuit-breaker: 0 (default) keeps the shipped posture
	// (metered, never spend-capped); FLOWBEE_COST_CEILING_USD > 0 arms it fleet-wide.
	st.DefaultCostCeilingMicroUSD = cfg.CostCeilingMicroUSD()
	if c := st.DefaultCostCeilingMicroUSD; c > 0 {
		logger.Info("cost ceiling armed", "usd", cfg.CostCeilingUSD, "micro_usd", c)
	}

	// worker-transport mutual auth (§7.6): when a signing secret is configured the
	// private API requires a signed per-worker bearer token bound to an enrolled
	// identity — the trust boundary a non-loopback (Tailscale/LAN) listener needs.
	// Empty secret = loopback-only dev (no mutual auth). mTLS is the documented
	// alternative (auth.MTLSConfig); it needs a CA + per-worker certs (real infra),
	// so it is not the default in-env path.
	var authn auth.Authenticator
	if cfg.WorkerAuthSecret != "" {
		authn = auth.NewBearer([]byte(cfg.WorkerAuthSecret), cfg.EnrolledIdentities, cfg.AuthLoopbackBypass)
		logger.Info("worker mutual-auth enabled", "enrolled", len(cfg.EnrolledIdentities), "loopback_bypass", cfg.AuthLoopbackBypass)
	} else if !isLoopbackAddr(cfg.PrivateAddr) {
		// no auth + a non-loopback bind = the worker API is reachable by any host on
		// the network with NO token. With autonomous merge on, that means a peer could
		// register a worker, claim a lease, and push a diff that auto-merges to main.
		// Refuse to start unless the operator explicitly accepts that (a trusted
		// tailnet), so "open" is always a conscious choice, never a silent default.
		if os.Getenv("FLOWBEE_INSECURE") == "" {
			return fmt.Errorf("REFUSING TO START: worker API binds %s (non-loopback) with NO auth — any host that can reach it could inject code"+
				"%s set FLOWBEE_WORKER_AUTH_SECRET + FLOWBEE_ENROLLED_IDENTITIES (run `flowbee token --identity X` per worker),"+
				"%s or, on a trusted private tailnet, set FLOWBEE_INSECURE=1 to accept an open API", cfg.PrivateAddr, "\n   ", "\n   ")
		}
		logger.Warn("⚠️  worker API is OPEN (no auth) on a non-loopback bind — relying on the network (e.g. Tailscale) as the only trust boundary",
			"addr", cfg.PrivateAddr, "self_merge", cfg.AllowSelfMerge)
	}
	humanAccess, err := configuredHumanAccess(cfg.PrivateAddr, authn, phase1Enabled)
	if err != nil {
		return fmt.Errorf("configure dashboard human access: %w", err)
	}

	// F5 per-repo consensus panel: a repo's required_reviewers overrides the global default,
	// so one repo can run an N-reviewer panel while others stay single-reviewer (keyed by the
	// repo id, which is jobs.repo).
	repoReviewers := map[string]int{}
	for _, rc := range cfg.Repos {
		if rc.RequiredReviewers > 0 {
			repoReviewers[rc.ID] = rc.RequiredReviewers
		}
	}

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL:           cfg.LeaseTTL(),
		HeartbeatInterval:  cfg.HeartbeatInterval(),
		LongPollWait:       cfg.LongPollWait(),
		LeaseTTLS:          cfg.LeaseTTLS,
		HeartbeatIntervalS: cfg.HeartbeatIntervalS,
		// same-box `worktree` provisioning: hand workers the shared bare mirror so
		// they can add a per-lease worktree at base_sha and push to the epoch ref
		// (§7.4). Empty disables local provisioning hints.
		MirrorPath: os.Getenv("FLOWBEE_MIRROR_PATH"),
		// F3: cross-box, credential-less `bundle` provisioning when
		// FLOWBEE_BUNDLE_PROVISIONING is set. Workers then hold NO GitHub credential
		// and NO mirror path — they fetch a read-only bundle, return a diff, and
		// Flowbee performs every git write (apply + push + PR-open, R4/§8).
		BundleProvisioning:         os.Getenv("FLOWBEE_BUNDLE_PROVISIONING") != "",
		Authenticator:              authn,
		HumanAccess:                humanAccess,
		DisableLegacyPaneActuation: st.EnableEpicReviewHandoffV2,
		// THE ONE DECISION (§14, F2): Branch B (autonomous merge) when
		// FLOWBEE_ALLOW_SELF_MERGE is set; default false = Branch A (handoff).
		Policy:        job.Policy{AllowSelfMerge: cfg.AllowSelfMerge, RequiredReviewers: cfg.RequiredReviewers},
		RepoReviewers: repoReviewers,
		// F2: the operator content-integrity posture (ceilings + extra denylist).
		ContentPolicy: cfg.ContentPolicy(),
		// build-list §7.3: the credential-bearing GitHub remote the control plane
		// publishes a build commit to (as a branch) so a PR can open after a build
		// result. Built from the single-repo GitHub creds; empty disables auto PR-open.
		PushRemoteURL: githubPushURL(),
		// FLOWBEE_GIT_REMOTE=ssh makes the lease ship SSH repo URLs to workers
		// (git@github.com:owner/repo.git) — for fleets whose boxes auth with SSH keys
		// (no HTTPS credential helper / no token at rest). Default HTTPS.
		WorkerGitSSH: strings.EqualFold(os.Getenv("FLOWBEE_GIT_REMOTE"), "ssh"),
		// PauseMarkerPath: a file beside the live DB whose presence stops new leases.
		// `flowbee pause` creates it; `flowbee resume` removes it; no server restart needed.
		PauseMarkerPath:      markerPath(cfg.DatabaseURL),
		RunningConfig:        runningConfigSnapshot(cfg),
		DriverControl:        driverControl,
		DriverControlCurrent: controlState.Snapshot,
	}, buildVersion())
	if cfg.AllowSelfMerge {
		logger.Info("autonomous merge enabled (Branch B): self_merge eligible jobs merge without a human gate")
	}
	healthSrv := &http.Server{Addr: cfg.HealthAddr, Handler: srv.HealthHandler()}
	privateSrv := &http.Server{Addr: cfg.PrivateAddr, Handler: srv.PrivateHandler()}

	// a bind failure on any listener is fatal — surfaced here so main exits non-zero
	// rather than running on as a dead process.
	srvErr := make(chan error, 3)
	go serveHTTP(logger, "health", healthSrv, srvErr)
	go serveHTTP(logger, "private", privateSrv, srvErr)

	// the single durable-timer polling goroutine (project override #2): drives the
	// no_eligible_worker alarm + the M8 liveness deadlines (Rung-3), epoch-guarded.
	// hbReap: presume a worker dead after ~4 missed heartbeats. The worker beats every
	// clamp(TTL/3, 20s, 60s); 4× that is comfortably above both the inter-beat gap and
	// the pre-first-beat worktree-setup window (covered by the grant-time floor), so a
	// live worker is never reaped — but a CRASHED one is, in minutes instead of the full
	// ~20-min absolute cap. The cap remains the backstop for tiny TTLs where 4× > cap.
	hbInterval := cfg.LeaseTTL() / 3
	if hbInterval > 60*time.Second {
		hbInterval = 60 * time.Second
	}
	if hbInterval < 20*time.Second {
		hbInterval = 20 * time.Second
	}
	livenessCfg := store.LivenessConfig{
		PhaseBudget:                   cfg.LeaseTTL() / 2, // soft deadline ~ half the TTL window
		AbsoluteCap:                   cfg.LeaseTTL(),     // the un-gameable Rung-3 floor
		Rung2Window:                   cfg.LeaseTTL() / 2,
		GovernorCeiling:               3, // Rung-4 anti-thrash (distinct from max_attempts)
		CircuitBreakerAbstainFraction: 0.8,
		HeartbeatReapAfter:            4 * hbInterval, // crash recovery in minutes, not ~20m
	}
	// the API arms these same deadlines on every claim (the soft phase budget is otherwise
	// never set, so the §10.2 soft-deadline rung stays inert); the poller evaluates them.
	srv.SetLiveness(livenessCfg)
	poller := alarm.New(st, clock.Real{}, time.Second, srv.Broker()).
		WithLiveness(livenessCfg, store.DBFactSource{DB: st.DB}, srv.Broker())
	go poller.Run(ctx)

	// base_sha refresh + rebase-before-review, PER REPO (F9): each managed repo's bare
	// mirror tracks its OWN integration branch, so a build is cut from / rebased onto
	// the correct repo's tip — never another repo's tree. Queries the registry at tick
	// time (repos are registered at startup by wireMultiRepo). Single-repo "default"
	// keeps using FLOWBEE_MIRROR_PATH directly (controlMirrorFor), so this is backward
	// compatible. Cheap public fetch + a rebase pass every 45s.
	if os.Getenv("FLOWBEE_MIRROR_PATH") != "" {
		go func() {
			t := time.NewTicker(45 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					repos, err := st.ListRepos(ctx, true)
					if err != nil {
						continue
					}
					for _, r := range repos {
						mp := controlMirrorFor(r)
						if mp == "" {
							continue
						}
						url := repoTokenURL(r)
						ensureRepoMirror(logger, mp, url)
						branch := r.DefaultBranch
						if branch == "" {
							branch = "main"
						}
						if err := gitops.Open(mp).FetchBranch(branch); err != nil {
							logger.Warn("mirror refresh", "repo", r.ID, "branch", branch, "err", err)
							continue
						}
						rebaseStaleReviews(ctx, logger, st, r.ID, mp, url, branch)
						// align not-yet-built `ready` builds to the live tip BEFORE dispatch:
						// a build that was `blocked` when its sibling merged (skipped by the
						// merge-time refresh) and later armed to `ready` keeps a stale base, and
						// would otherwise waste a build cut from pre-merge code. A ready build has
						// no PR/lease/verdict, so this is a pure pre-dispatch base advance.
						if tip, terr := gitops.Open(mp).HeadSHA("refs/heads/" + branch); terr == nil {
							if n, rerr := st.RefreshStaleReadyBuilds(ctx, r.ID, tip, time.Now()); rerr != nil {
								logger.Warn("refresh stale ready bases", "repo", r.ID, "err", rerr)
							} else if n > 0 {
								logger.Info("advanced stale ready bases to tip", "repo", r.ID, "count", n)
							}
						}
						// routine maintenance: the control mirror accumulates objects from
						// every fetch over months; `git gc --auto` is a no-op below git's
						// loose-object threshold and self-batches the occasional real repack,
						// so keeping the mirror lean costs ~nothing on the steady-state path.
						_ = gitops.Open(mp).GCAuto()
					}
				}
			}
		}()
	}
	// the Rung-2 sweep + two-rung evaluation pass runs on the slower external-oracle
	// cadence (§10.2 — Rung-2 only updates on the reconcile sweep).
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poller.Rung2Tick(ctx)
			}
		}
	}()

	// fleet-health watchdog: a `ready` job with NO live worker sits silently (the
	// scheduler has nothing to assign, no alarm fires for a build stage). Warn loudly
	// + repeatedly so a down/disconnected fleet is impossible to miss — the operator
	// sees it in the log even without looking at the dashboard.
	staleHB := 3 * cfg.HeartbeatInterval()
	if staleHB <= 0 {
		staleHB = 90 * time.Second
	}
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if h, err := st.FleetHealth(ctx, time.Now(), staleHB); err == nil && h.Stranded() {
					logger.Warn("⚠️  jobs WAITING but NO live worker — is the fleet up? start `flowbee fleet` on a box",
						"waiting_jobs", h.WaitingJobs, "stale_workers", h.StaleWorkers, "live_workers", h.LiveWorkers)
				}
			}
		}
	}()

	// forward-progress watchdog: the "never permanently stuck" guarantee. Each tick it
	// (1) re-folds every leasable job's ledger and corrects any projection that drifted
	// out of sync (the #2217 wedge: a `ready` build the table said needed a reviewer cap
	// so no builder could claim it — determinism-restoring self-heal), and (2) escalates
	// a job that re-folds clean but has sat unclaimed past stallAfter WHILE the fleet is
	// live (a no-eligible-worker dead-end) to needs_human, so nothing wedges silently.
	stallAfter := 4 * cfg.LeaseTTL() // generous: well past any real build/review cycle
	if stallAfter < 30*time.Minute {
		stallAfter = 30 * time.Minute
	}
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rep, err := st.ReconcileStuck(ctx, time.Now(), staleHB, stallAfter)
				if err != nil {
					logger.Error("forward-progress watchdog", "err", err)
					continue
				}
				if rep.Resynced > 0 || rep.Escalated > 0 {
					logger.Warn("🩹 forward-progress watchdog acted",
						"resynced_projection", rep.Resynced, "escalated_to_human", rep.Escalated)
				}
				// self-unblock janitor (0023): the sibling that moves MECHANICALLY-recoverable
				// jobs BACK OUT of the needs_human sink — bounded, cooled-down, breaker-gated —
				// so a transient stall no longer needs an operator to run `flowbee requeue`. Only
				// `stall` is auto-recovered today; semantic dead-ends stay parked for a human.
				// Reversible via FLOWBEE_SELF_UNBLOCK=off (the per-rung kill-switch).
				if cfg.SelfUnblockDisabled {
					continue
				}
				jrep, err := st.JanitorUnblock(ctx, time.Now(), staleHB, store.JanitorConfig{})
				if err != nil {
					logger.Error("self-unblock janitor", "err", err)
					continue
				}
				if jrep.Unblocked > 0 {
					logger.Warn("🔓 self-unblock janitor re-armed stuck jobs", "unblocked", jrep.Unblocked)
				}
				if len(jrep.StoodDown) > 0 {
					// correlated-failure breaker tripped: a shared root cause is likelier than N
					// independent stalls. Surface it — this is a fix-once page, not an auto-retry.
					logger.Warn("🛑 self-unblock janitor stood down (correlated failures — fix the shared cause, then requeue)",
						"reasons", jrep.StoodDown)
				}
			}
		}
	}()

	// Rung-E advisor (0024): the LAST resort before a human. Consulted (opt-in) for a job
	// the deterministic janitor could not rescue — a stall past its mechanical unblock cap.
	// A read-only, single-shot model call NOMINATES {PLAN,CORRECTION,REPROMPT,STOP}; the
	// store re-authorizes (PLAN/CORRECTION/REPROMPT re-arm ONCE with the note injected as
	// fresh-context; STOP or any failure leaves it parked). Runs on its OWN slow ticker in
	// its OWN goroutine so a multi-second model call never stalls the fast forward-progress
	// watchdog. Enable with FLOWBEE_ADVISOR=on (+ FLOWBEE_ADVISOR_CMD for the codex form).
	if cfg.AdvisorEnabled {
		adv := advisor.NewCLIAdvisor(cfg.AdvisorCmd, 0)
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					runAdvisorPass(ctx, logger, st, adv)
				}
			}
		}()
		logger.Info("🧭 Rung-E stuck-job advisor enabled", "cmd_is_default", cfg.AdvisorCmd == "")
	}

	// goal-session watchdog (epic-lane Phase 1, 0025_goal_sessions.sql): watches
	// registered tmux "goal" sessions — long-running codex CLI agents, hours-to-days,
	// sometimes on a remote box over ssh. Two real incidents motivated this: a goal on
	// box `buncher` sat silently blocked ~a day on missing `gh` auth (finished work
	// stranded, nobody knew), and sessions routinely max out usage limits and just need
	// `/goal resume` typed once the window resets. Own goroutine on its own 2-minute
	// ticker (mirrors the mirror-refresh/advisor cadence style above) — a wedged
	// tmux/ssh capture must never stall the fast forward-progress watchdog. Kill-switch:
	// FLOWBEE_SESSION_WATCH=off (mirrors FLOWBEE_SELF_UNBLOCK exactly).
	if legacyPaneRuntimeEnabled(st.EnableEpicReviewHandoffV2, cfg.SessionWatchDisabled) {
		watcher := watchdog.New(st, watchdog.NewShellRunner(), logger)
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					watcher.Pass(ctx, time.Now())
				}
			}
		}()
		logger.Info("👁️  goal-session watchdog enabled (2m tick)")
	} else if st.EnableEpicReviewHandoffV2 && !cfg.SessionWatchDisabled {
		logger.Info("goal-session raw pane watcher fenced by v2; Driver observation/actions are authoritative")
	}

	// Keep every epics-topic nudge on the same digest-sequence contract as the master
	// API: constrained consumers can dedupe, while the dashboard simply re-reads truth.
	publishEpicNudge := func(event string) {
		seq, _ := st.EpicDigestSeq(ctx)
		srv.Broker().Publish(api.LifeEvent{State: "epics", Event: event, DigestSeq: seq})
	}

	// THE ONE CONSOLIDATED EPIC-SUPERVISION TICKER (epic-lane Phase 6b, plan §12.2). A
	// SINGLE 2-minute goroutine does the WHOLE epic pass in a serialized batch — NOT six
	// tickers, so the single-writer SQLite budget stays bounded: (a) status ingestion off
	// each active epic's branch (mirror reads), then (b) the supervision pass — pane
	// classify + runtime-state write, attention producers/auto-resolve, launching-reaper,
	// stranded-delivery recovery, expired-lease reap, the send-and-ack loop, dead-master
	// reap, and push-to-wake (all the pure decisions live in internal/attention /
	// internal/epicdigest; internal/epicsupervisor is the impure shell). Kill-switch:
	// FLOWBEE_EPIC_SUPERVISION=off (mirrors FLOWBEE_SESSION_WATCH). Ingestion always runs
	// (it is the Phase 2 status fold); only the supervision half honors the switch.
	{
		var supv *epicsupervisor.Supervisor
		if legacyPaneRuntimeEnabled(st.EnableEpicReviewHandoffV2, cfg.EpicSupervisionDisabled) {
			supv = epicsupervisor.New(st, epicsupervisor.TmuxPane{}, nil, epicsupervisor.Config{}, logger)
			logger.Info("🛰️  epic-supervision ticker enabled (2m tick — one consolidated pass)")
		} else if st.EnableEpicReviewHandoffV2 && !cfg.EpicSupervisionDisabled {
			logger.Info("legacy tmux epic supervisor fenced by v2; Driver observation/actions are authoritative")
		}
		go func() {
			t := time.NewTicker(2 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					// M1: this tick parses UNTRUSTED builder-pushed ## Status markdown
					// (ingestEpicStatuses) and drives keystrokes (supv.Pass), inside the
					// control-plane process. A panic here must NEVER crash `flowbee serve`
					// (Restart=always would crashloop the WHOLE control plane on one bad epic
					// file). epicsupervisor.Pass has its own per-pass + per-epic recover; this
					// backstop additionally covers ingestEpicStatuses. Log loudly, skip the
					// tick; the next 2-minute tick starts clean (mirrors watchdog.Pass).
					func() {
						defer func() {
							if r := recover(); r != nil {
								logger.Error("epic-supervision tick: PANIC recovered — tick skipped", "panic", r)
							}
						}()
						now := time.Now()
						ingestEpicStatuses(ctx, logger, st, now)
						if st.EnableEpicReviewHandoffV2 {
							var handoffRep store.EpicReviewReconcileResult
							if err := v2Reconcilers.tick(ctx, "review_handoff", now, func() error {
								var err error
								handoffRep, err = st.ReconcileEpicReviewHandoffs(ctx, now, 5*time.Minute)
								return err
							}); err != nil {
								logger.Error("epic review handoff reconcile failed", "err", err)
							} else if handoffRep.Dispatched > 0 {
								logger.Info("epic review handoffs repaired", "scanned", handoffRep.Scanned, "dispatched", handoffRep.Dispatched)
							}
							var verdictRep store.EpicReviewVerdictStallResult
							if err := v2Reconcilers.tick(ctx, "review_verdict", now, func() error {
								var err error
								verdictRep, err = st.ReconcileEpicReviewVerdictStalls(ctx, now, 20*time.Minute, 3)
								return err
							}); err != nil {
								logger.Error("epic review verdict reconcile failed", "err", err)
							} else if verdictRep.Requeued > 0 {
								logger.Warn("hung epic reviewers fenced", "scanned", verdictRep.Scanned, "requeued", verdictRep.Requeued, "escalated", verdictRep.Escalated)
							}
							var backstopRep store.EpicDeliveryBackstopResult
							if err := v2Reconcilers.tick(ctx, "delivery_backstop", now, func() error {
								var err error
								backstopRep, err = st.ReconcileEpicDeliveryBackstops(ctx, now)
								return err
							}); err != nil {
								logger.Error("epic delivery backstop failed", "err", err)
							} else if backstopRep.Alerted > 0 {
								logger.Warn("overdue epic delivery states surfaced", "scanned", backstopRep.Scanned, "alerted", backstopRep.Alerted)
							}
						}
						if supv != nil {
							supv.Pass(ctx, now)
						}
						// The dashboard polls as a backstop, but the completed serialized pass is
						// also the authoritative low-latency nudge for epic/runtime/attention data.
						publishEpicNudge("epic_supervision_pass")
					}()
				}
			}
		}()
	}

	// The STAGGERED capacity/seat fold (plan §12.2 — the acctprobe fold runs on a separate
	// 5-minute offset so its ssh-heavy probes never share a batch with the fast supervision
	// pass; 2 tickers total, not 6). It probes each registered seat (acctprobe over ssh),
	// folds real 5h/7d% into account_windows via UpsertAccountLimits, and refreshes seat
	// health — the truth the launch gate + usage_critical producer read. Gated on the same
	// kill-switch (the capacity data feeds the supervision decisions).
	if !cfg.EpicSupervisionDisabled {
		go func() {
			foldAndPublish := func() {
				foldSeatCapacity(ctx, logger, st, time.Now())
				// Account windows and seat health are dashboard truth too; publish even
				// when no threshold crossing produced the legacy capacity event.
				publishEpicNudge("capacity_fold")
			}
			// stagger: wait ~1 minute before the first probe so it never collides with the
			// supervision tick's first fire on startup.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
			foldAndPublish()
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					foldAndPublish()
				}
			}
		}()
	}

	// epic fan-out drain (§F4): once an epic's barrier review passes, its child issues
	// are released from backlog into their own spec flows. Review and fan-out are kept
	// distinct steps (barrier-before-fan-out); this drain is the trigger that releases
	// a reviewed epic's children — without it they sit in backlog forever.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n, err := st.FanOutReviewedEpics(ctx, time.Now()); err != nil {
					logger.Error("epic fan-out drain", "err", err)
				} else if n > 0 {
					logger.Info("📤 epic fan-out: released children to build", "issues", n)
				}
				// self-heal: a build job re-armed to `ready` with stale review caps is
				// unleaseable by any builder — repair it so it can never strand.
				if n, err := st.NormalizeStrandedReadyBuilds(ctx, time.Now()); err != nil {
					logger.Error("normalize stranded ready builds", "err", err)
				} else if n > 0 {
					logger.Warn("🩹 repaired stranded ready build jobs (stale review caps)", "jobs", n)
				}
			}
		}
	}()

	// built-in auto-backup (durability floor): the control plane snapshots its own DB on
	// a timer so an operator gets the on-disk restore floor with ZERO extra services — no
	// cron, no litestream — matching Flowbee's one-binary promise. VACUUM INTO is safe
	// against the live writer under WAL, and the jobs table folds from the append-only
	// ledger, so every snapshot restores self-consistently. A NEGATIVE backup_interval_s
	// opts out (operator runs their own). Litestream to object storage is still the
	// off-disk production answer (operating.md §6); this is the floor that was missing.
	if d, on := cfg.BackupInterval(); on {
		backupDir := envOr("FLOWBEE_BACKUP_DIR", defaultBackupDir())
		keep := cfg.BackupKeepN()
		logger.Info("auto-backup enabled", "interval", d.String(), "keep", keep, "dir", backupDir)
		go func() {
			snap := func(why string) {
				s, size, pruned, err := takeSnapshot(ctx, st.DB, backupDir, keep)
				if err != nil {
					logger.Error("auto-backup FAILED", "why", why, "err", err) // durability gap — alertable
					return
				}
				logger.Info("💾 auto-backup", "why", why, "snapshot", s, "bytes", size, "pruned", pruned)
			}
			// POLL-and-check-DUE rather than a fixed-interval ticker from startup. A snapshot
			// is taken whenever the newest one is older than the interval (or none exists), so:
			// (a) a CP that restarts more often than the interval still snapshots (a plain
			// ticker resets every restart → never fires); AND (b) staleness is bounded to
			// ~interval REGARDLESS of restart timing — a fixed ticker would let a restart that
			// lands mid-interval (snapshot e.g. 5h old, < interval so no catch-up) push the next
			// backup to interval-after-startup, leaving the floor ~2×interval stale. A recent
			// snapshot within the window is left alone, so frequent restarts don't spam backups.
			poll := d / 6
			if poll < time.Minute {
				poll = time.Minute
			}
			// Fire when within HALF A POLL of the interval boundary, not strictly at/after it.
			// The snapshot is WRITTEN by the poll, so its mtime lands slightly AFTER the poll
			// tick; exactly `interval` later (with poll evenly dividing interval) the boundary
			// poll computes age as a few ms UNDER `interval` and skips, deferring the backup a
			// full poll — making the effective cadence interval+poll, not interval. A half-poll
			// tolerance lets the boundary poll fire (cadence ~interval ± poll/2) while the prior
			// poll, a half-interval-fraction earlier, still doesn't (no early spam).
			threshold := d - poll/2
			due := func(why string) {
				if age, ok := newestSnapshotAge(backupDir); !ok || age >= threshold {
					snap(why)
				}
			}
			due("startup")
			t := time.NewTicker(poll)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					due("interval")
				}
			}
		}()
	} else {
		logger.Warn("auto-backup DISABLED (backup_interval_s<0) — ensure an external backup (cron `flowbee backup` / litestream) covers durability")
	}

	// reconcile-IN + project-OUT (M6/M7, §8.1/§8.2): Flowbee is the SINGLE GitHub
	// caller (R4). F9 multi-repo: ONE control plane runs a per-repo reconcile-IN +
	// project-OUT loop over the repos registry, sharing a GLOBAL scheduler + fleet.
	// The registry is seeded from cfg.Repos (a structured list) or, for backward
	// compatibility, the single-repo FLOWBEE_GITHUB_OWNER/REPO env path. There are no
	// creds in dev/CI, so this stays dormant when nothing is configured.
	if mgr := wireMultiRepo(ctx, logger, cfg, st, srv); mgr != nil {
		if st.EnableEpicReviewHandoffV2 {
			breakerRunner := newProductionProjectBreakerRunner(st, mgr,
				fmt.Sprintf("serve-project-breaker-%d", os.Getpid()))
			startupNow := time.Now()
			var startupBreakerRep projectbreaker.Report
			if err := v2Reconcilers.tick(ctx, "project_breaker_probe", startupNow, func() error {
				var err error
				startupBreakerRep, err = breakerRunner.RunOnce(ctx)
				return err
			}); err != nil {
				// The periodic supervised loop remains live even if the startup claim
				// encounters a transient store error. Existing open breakers remain a
				// visible, fail-closed hold and the reconciler watchdog tracks health.
				logger.Error("startup project breaker probe failed", "err", err)
			} else {
				logProjectBreakerReport(logger, "startup project breaker probe complete", startupBreakerRep)
			}
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						var rep projectbreaker.Report
						err := v2Reconcilers.tick(ctx, "project_breaker_probe", now, func() error {
							var err error
							rep, err = breakerRunner.RunOnce(ctx)
							return err
						})
						if err != nil {
							logger.Error("project breaker probe failed", "err", err)
						} else if rep.Claimed > 0 {
							logProjectBreakerReport(logger, "project breaker probe pass", rep)
						}
					}
				}
			}()

			startupNow = time.Now()
			var startupEffectRep store.EpicEffectReconcileResult
			if err := v2Reconcilers.tick(ctx, "epic_effects", startupNow, func() error {
				var err error
				startupEffectRep, err = st.ReconcileEpicEffectActions(ctx, startupNow, 2)
				return err
			}); err != nil {
				return fmt.Errorf("startup epic merge/cleanup reconcile: %w", err)
			}
			logger.Info("startup epic merge/cleanup reconcile complete", "scanned", startupEffectRep.Scanned,
				"ensured", startupEffectRep.Ensured, "rearmed", startupEffectRep.Rearmed)
			effectRunner := epicexec.Runner{Store: st, Resolver: mgr, Authorizer: mgr,
				Owner: fmt.Sprintf("serve-effects-%d", os.Getpid()), ClaimTTL: time.Minute}
			go func() {
				ticker := time.NewTicker(2 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case now := <-ticker.C:
						err := v2Reconcilers.tick(ctx, "epic_effects", now, func() error {
							if _, err := st.ReconcileEpicEffectActions(ctx, now, 2); err != nil {
								return err
							}
							if _, err := st.ReclaimExpiredEpicDomainActions(ctx, now); err != nil {
								return err
							}
							if _, err := effectRunner.VerifyNext(ctx); err != nil {
								return err
							}
							_, err := effectRunner.ExecuteNext(ctx)
							return err
						})
						if err != nil {
							logger.Error("epic merge/cleanup runtime failed", "err", err)
						}
					}
				}
			}()
		}
		// let POST /v1/adopt (flowbee adopt <pr>) reach the per-repo GitHub loops.
		srv.SetAdopter(mgr)
		// boot sweep + periodic floor sweep PER repo (every 2-5 min; default 3 min),
		// plus a periodic project-OUT drain PER repo.
		// reconcile cadence drives how fast Flowbee REACTS to GitHub facts — chiefly
		// CI going green (the reviewer can't mint until it's reconciled) and a PR
		// merging. A slow sweep means a green build sits unreviewed until the next tick,
		// so this is the dominant pipeline latency. Default 45s (public repos have ample
		// rate limit — one GraphQL board read per repo per tick); tune via
		// FLOWBEE_RECONCILE_INTERVAL_S. Webhooks make it event-driven when wired.
		reconcileEvery := 45 * time.Second
		if v := os.Getenv("FLOWBEE_RECONCILE_INTERVAL_S"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
				reconcileEvery = time.Duration(n) * time.Second
			}
		}
		logger.Info("reconcile cadence", "interval", reconcileEvery.String())
		go func() {
			_, err := mgr.SweepAll(ctx)
			srv.RecordGitHubSweep(err) // surface a sustained GitHub failure (expired token, …)
			if err != nil {
				logger.Error("boot reconcile sweep", "err", err)
			}
			t := time.NewTicker(reconcileEvery)
			defer t.Stop()
			drain := time.NewTicker(5 * time.Second)
			defer drain.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					_, err := mgr.SweepAll(ctx)
					srv.RecordGitHubSweep(err)
					if err != nil {
						logger.Error("reconcile sweep", "err", err)
					}
				case <-drain.C:
					if _, err := mgr.DrainAll(ctx); err != nil {
						logger.Error("project-out drain", "err", err)
					}
				}
			}
		}()
		// #214 merge_handoff un-stick: periodically fast-forward any reviewed, green PR that is
		// BEHIND its base, so it stops rotting (15-19h observed) AND stops pushing the other
		// waiting PRs further behind — the cascade that never converges on its own. It NEVER
		// merges (only update-branch, a server-side FF that re-triggers CI), acts ONLY on a
		// definitive GitHub "behind" (reported only when the repo requires up-to-date branches,
		// so it self-scopes), and a real conflict is left to a human. Slower cadence than
		// reconcile (the rot is hours; one REST read per merge_handoff PR per pass). Tune via
		// FLOWBEE_UNSTICK_INTERVAL_S; a negative value disables it.
		unstickEvery := 5 * time.Minute
		if v := os.Getenv("FLOWBEE_UNSTICK_INTERVAL_S"); v != "" {
			if n, perr := strconv.Atoi(v); perr == nil {
				if n < 0 {
					unstickEvery = 0
				} else if n > 0 {
					unstickEvery = time.Duration(n) * time.Second
				}
			}
		}
		if unstickEvery > 0 {
			logger.Info("merge_handoff un-stick enabled", "interval", unstickEvery.String())
			go func() {
				t := time.NewTicker(unstickEvery)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						counts, err := mgr.UnstickAll(ctx)
						if err != nil {
							logger.Error("merge_handoff un-stick", "err", err)
							continue
						}
						total := 0
						for repo, n := range counts {
							if n > 0 {
								logger.Info("🔀 un-stuck behind PRs (update-branch)", "repo", repo, "count", n)
								total += n
							}
						}
						srv.AddUnstick(total) // feeds flowbee_unstick_total
					}
				}
			}()
		}
		refetcher := repoRefetcher{mgr: mgr, defaultRepo: firstRepo(mgr)}
		// crash-replay: re-drive any webhook deliveries recorded 'pending' but interrupted
		// before their refetch (a CP crash between RecordDelivery and MarkDeliveryProcessed).
		// The periodic sweep is the correctness floor; this recovers the targeted refetch
		// promptly and drains inbox rows that would otherwise strand forever.
		if pend, err := st.PendingDeliveries(ctx); err == nil && len(pend) > 0 {
			wp := make([]webhook.PendingDelivery, len(pend))
			for i, d := range pend {
				wp[i] = webhook.PendingDelivery{DeliveryID: d.DeliveryID, Event: d.Event, PRNumber: d.PRNumber}
			}
			done := webhook.ReplayPending(ctx, wp, refetcher, func(id string) error {
				return st.MarkDeliveryProcessed(ctx, id)
			})
			logger.Info("replayed pending webhook deliveries", "replayed", done, "found", len(pend))
		}
		// the PUBLIC webhook listener (I-2): only started when a secret is set. Hints
		// are routed to the right repo's reconciler via the X-Flowbee-Repo header
		// (default: the sole/first managed repo when unset).
		if secret := os.Getenv("FLOWBEE_WEBHOOK_SECRET"); secret != "" {
			wh := webhook.New(secret, st, refetcher)
			webhookMux := http.NewServeMux()
			webhookMux.Handle("POST /webhooks", wh)
			webhookSrv := &http.Server{Addr: cfg.WebhookAddr, Handler: webhookMux}
			go serveHTTP(logger, "webhook", webhookSrv, srvErr)
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = webhookSrv.Shutdown(shutdownCtx)
			}()
		}
		logger.Info("multi-repo control plane wired", "repos", mgr.Repos())
	}

	logger.Info("flowbee serve started",
		"version", buildVersion(), "health", cfg.HealthAddr, "private", cfg.PrivateAddr)

	// pidfile so `flowbee repo add --reload` can find the CP to signal. Survives a re-exec
	// (same pid rewrites it); removed on a clean SIGTERM exit, not on re-exec (Exec replaces
	// the image before the defer runs).
	if pf := pidFilePath(); pf != "" {
		_ = os.WriteFile(pf, []byte(strconv.Itoa(os.Getpid())), 0o644)
		defer os.Remove(pf)
	}

	var bindErr error
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case bindErr = <-srvErr:
		// a listener failed to bind — shut down and exit non-zero so the operator (and
		// `flowbee up`'s health wait) sees a loud failure, not a silent dead process.
		logger.Error("fatal: a listener failed; shutting down", "err", bindErr)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = healthSrv.Shutdown(shutdownCtx)
	_ = privateSrv.Shutdown(shutdownCtx)
	select {
	case <-reexec:
		return reexecSelf(logger)
	default:
	}
	return bindErr
}

// legacyPaneRuntimeEnabled is the single activation predicate for background
// loops that observe or actuate sessions through raw tmux. V2 owns those
// operations through DriverPort, so an operator cannot accidentally re-enable a
// legacy loop merely by leaving its old kill-switch unset.
func legacyPaneRuntimeEnabled(v2Enabled, explicitlyDisabled bool) bool {
	return !v2Enabled && !explicitlyDisabled
}

func runningConfigSnapshot(cfg config.Config) api.RunningConfig {
	prov := currentProvenance(context.Background(), true)
	rc := api.RunningConfig{
		SourceCommit:          prov.SourceCommit,
		TreeDirty:             prov.TreeDirty,
		TreeDirtyKnown:        prov.TreeDirtyKnown,
		OriginMainSHA:         prov.OriginMainSHA,
		BehindOriginMainBy:    prov.BehindOriginMainBy,
		BehindOriginMainKnown: prov.BehindOriginMainKnown,
		SourceWarning:         prov.Warning,
		ConfigPath:            runningConfigPath(),
		DatabaseURL:           cfg.DatabaseURL,
		PrivateAddr:           cfg.PrivateAddr,
		HealthAddr:            cfg.HealthAddr,
		WebhookAddr:           cfg.WebhookAddr,
		AllowSelfMerge:        cfg.AllowSelfMerge,
		RequiredReviewers:     cfg.RequiredReviewers,
		MirrorPath:            os.Getenv("FLOWBEE_MIRROR_PATH"),
		GitRemote:             os.Getenv("FLOWBEE_GIT_REMOTE"),
		WorkerGitSSH:          strings.EqualFold(os.Getenv("FLOWBEE_GIT_REMOTE"), "ssh"),
		BundleProvisioning:    os.Getenv("FLOWBEE_BUNDLE_PROVISIONING") != "",
		GitHubTokenPresent:    os.Getenv("FLOWBEE_GITHUB_TOKEN") != "",
		WebhookSecretPresent:  os.Getenv("FLOWBEE_WEBHOOK_SECRET") != "",
		WorkerAuthConfigured:  cfg.WorkerAuthSecret != "",
		InsecureWorkerAPI:     os.Getenv("FLOWBEE_INSECURE") != "",
		AuthLoopbackBypass:    cfg.AuthLoopbackBypass,
		LogPath:               os.Getenv("FLOWBEE_LOG_PATH"),
		BackupDir:             envOr("FLOWBEE_BACKUP_DIR", defaultBackupDir()),
		ReconcileIntervalEnv:  os.Getenv("FLOWBEE_RECONCILE_INTERVAL_S"),
		UnstickIntervalEnv:    os.Getenv("FLOWBEE_UNSTICK_INTERVAL_S"),
		FlowbeeURL:            os.Getenv("FLOWBEE_URL"),
	}
	for _, r := range cfg.Repos {
		tokenEnv := r.TokenEnv
		if tokenEnv == "" {
			tokenEnv = "FLOWBEE_GITHUB_TOKEN"
		}
		rc.Repos = append(rc.Repos, api.RunningConfigRepo{
			ID: r.ID, Owner: r.Owner, Repo: r.Repo, DefaultBranch: r.DefaultBranch,
			Active: r.IsActive(), TokenEnv: tokenEnv, TokenPresent: os.Getenv(tokenEnv) != "",
			ArchiveHistory: r.ArchiveHistory, RequiredReviewers: r.RequiredReviewers,
		})
	}
	if len(rc.Repos) == 0 && cfg.GithubOwner != "" && cfg.GithubRepo != "" {
		rc.Repos = append(rc.Repos, api.RunningConfigRepo{
			ID: "default", Owner: cfg.GithubOwner, Repo: cfg.GithubRepo,
			DefaultBranch: cfg.GithubDefaultBranch, Active: true,
			TokenEnv: "FLOWBEE_GITHUB_TOKEN", TokenPresent: os.Getenv("FLOWBEE_GITHUB_TOKEN") != "",
		})
	}
	return rc
}

func runningConfigPath() string {
	if p := os.Getenv("FLOWBEE_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("flowbee.yaml"); err == nil {
		return "flowbee.yaml"
	}
	return ""
}

// reexecSelf replaces the running process image with a fresh `flowbee serve` (same binary,
// args, env, pid) — an in-place restart that re-reads the config. Behaviourally identical to a
// graceful kill -TERM + relaunch, so it inherits that safety (SQLite WAL is durable across the
// abrupt image swap; the loops + listeners were already shut down via the cancelled ctx).
func reexecSelf(logger *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("re-exec: resolve self: %w", err)
	}
	logger.Info("re-exec for config reload", "binary", exe)
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("re-exec %s: %w", exe, err)
	}
	return nil // unreachable on success: the image is replaced
}

// pidFilePath is the standard control-plane pidfile (~/.flowbee/flowbee.pid), written by serve
// and read by `flowbee repo add --reload` to find the CP to signal. Empty if no home dir.
func pidFilePath() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".flowbee", "flowbee.pid")
	}
	return ""
}

// wireMultiRepo seeds the F9 repos registry from config (or the legacy single-repo
// env) and builds the per-repo reconcile/project Manager. Returns nil when no repo
// is configured (dev/CI with no creds), so the control plane runs without GitHub.
// isLoopbackAddr reports whether a listen address is loopback-only (so an open,
// no-auth worker API is not exposed to the network). The default ":7070" binds ALL
// interfaces and is therefore NOT loopback-only.
func isLoopbackAddr(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// ensureControlMirror clones the control-plane bare mirror from the GitHub token
// when FLOWBEE_MIRROR_PATH is set but the directory is absent. The token is baked
// into the mirror's origin so the periodic fetch and branch pushes work against a
// private repo; the mirror lives on the control plane, which holds the token anyway.
func ensureControlMirror(logger *slog.Logger, cfg config.Config) {
	mp := os.Getenv("FLOWBEE_MIRROR_PATH")
	if mp == "" {
		return
	}
	if _, err := os.Stat(mp); err == nil {
		return // already provisioned
	}
	// Prefer the legacy single-repo env; else fall back to the F9 repos REGISTRY (the
	// production layout) — the legacy-env-only path left the shared mirror unprovisioned
	// on every registry deployment and emitted a misleading "set FLOWBEE_GITHUB_OWNER/
	// REPO" warning, even though the same-box-worker + bundle paths that consume the
	// shared mirror were then silently broken. The shared mirror is single-repo by
	// nature, so it tracks the PRIMARY registered repo.
	url := githubPushURL()
	if url == "" {
		url = registryControlMirrorURL(cfg)
	}
	if url == "" {
		logger.Warn("FLOWBEE_MIRROR_PATH set but absent and no GitHub coords/token to clone it "+
			"(set FLOWBEE_GITHUB_TOKEN + a repos: registry, or the legacy FLOWBEE_GITHUB_OWNER/REPO)", "path", mp)
		return
	}
	logger.Info("provisioning control-plane mirror from GitHub", "path", mp)
	if err := gitops.CloneBareMirror(mp, url); err != nil {
		logger.Error("clone control-plane mirror", "err", err)
		return
	}
	// the token is NOT stored in the mirror config (CloneBareMirror persists an
	// env-reading credential helper instead) and never appeared in argv; still lock the
	// mirror down as defense-in-depth.
	_ = os.Chmod(mp, 0o700)
	_ = os.Chmod(filepath.Join(mp, "config"), 0o600)
}

// registryControlMirrorURL builds the shared-control-mirror clone URL from the F9
// repos registry when the legacy single-repo env is unset. The shared mirror is
// single-repo by nature, so it tracks the PRIMARY active repo (first by id — the same
// repo a repo-less ingest defaults to). The token is the repo's token_env, else the
// shared FLOWBEE_GITHUB_TOKEN. Empty when no active repo has both coords and a token.
func registryControlMirrorURL(cfg config.Config) string {
	var active []config.RepoConfig
	for _, r := range cfg.Repos {
		if r.IsActive() && r.Owner != "" && r.Repo != "" {
			active = append(active, r)
		}
	}
	if len(active) == 0 {
		return ""
	}
	sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
	r := active[0]
	tok := os.Getenv("FLOWBEE_GITHUB_TOKEN")
	if r.TokenEnv != "" {
		if v := os.Getenv(r.TokenEnv); v != "" {
			tok = v
		}
	}
	if tok == "" {
		return ""
	}
	return "https://x-access-token:" + tok + "@github.com/" + r.Owner + "/" + r.Repo + ".git"
}

// githubPushURL builds the credential-bearing https remote the control plane
// pushes build branches to (so a PR can open), from the single-repo GitHub env.
// Empty when any of owner/repo/token is unset — auto PR-open is then disabled.
func githubPushURL() string {
	owner, repo, tok := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO"), os.Getenv("FLOWBEE_GITHUB_TOKEN")
	if owner == "" || repo == "" || tok == "" {
		return ""
	}
	return "https://x-access-token:" + tok + "@github.com/" + owner + "/" + repo + ".git"
}

// rebaseStaleReviews replays every review_pending build that is behind the current
// integration tip onto it, BEFORE a reviewer leases it (build-list rebase-before-
// review). It reuses the F8 machinery (store.RebaseOnto): a clean rebase re-arms
// review + CI at the integrated head and the rebased commit is force-pushed to the
// issue branch so the PR + CI track it; a real conflict diverts the job to a
// conflict_resolver. Best-effort: a single job's failure never blocks the others.
// runAdvisorPass consults the Rung-E advisor for up to advisorMaxPerPass jobs the
// deterministic janitor could not rescue, and applies each verdict (fail-safe: an advisor
// error is treated as STOP by CLIAdvisor, and the consult is still recorded so the model is
// not re-hammered at the same signature). Kept small + bounded — the advisor is the rare,
// expensive tail, not a steady-state cost.
func runAdvisorPass(ctx context.Context, logger *slog.Logger, st *store.Store, adv advisor.Advisor) {
	const (
		minUnblock        = 2 // == JanitorConfig.MaxUnblockAttempts default: only jobs the janitor gave up on
		advisorCap        = 3 // per-job consult ceiling -> converges to a permanent human park
		advisorMaxPerPass = 2
		cooldown          = 10 * time.Minute
	)
	cands, err := st.AdvisorCandidates(ctx, minUnblock, advisorCap)
	if err != nil {
		logger.Error("advisor candidates", "err", err)
		return
	}
	n := 0
	for _, c := range cands {
		if n >= advisorMaxPerPass {
			break
		}
		v, cerr := adv.Consult(ctx, advisor.StuckJob{
			JobID: c.JobID, Reason: c.Reason, Kind: c.Kind, HeadSHA: c.HeadSHA,
			Task: c.TaskText, Acceptance: c.Acceptance,
			LastReviewNotes: c.LastReviewNotes, LastCIFailures: c.LastCIFailures,
			Attempts: c.Attempts, MaxAttempts: c.MaxAttempts, UnblockAttempts: c.UnblockAttempts,
		})
		if cerr != nil {
			// fail-safe: v.Action is STOP here; record it so we don't re-consult, then log.
			logger.Warn("advisor unavailable — leaving job parked", "job", c.JobID, "err", cerr)
		}
		rearmed, aerr := st.ApplyAdvisorVerdict(ctx, c.JobID, string(v.Action), v.Note, c.TriggerHash, time.Now(), cooldown)
		if aerr != nil {
			logger.Error("apply advisor verdict", "job", c.JobID, "err", aerr)
			continue
		}
		if rearmed {
			logger.Warn("🧭 advisor re-armed a stuck job", "job", c.JobID, "action", v.Action, "note", v.Note)
		} else {
			logger.Info("advisor left job parked", "job", c.JobID, "action", v.Action)
		}
		n++
	}
}

func rebaseStaleReviews(ctx context.Context, logger *slog.Logger, st *store.Store, repoID, mirrorPath, pushURL, branch string) {
	mirror := gitops.Open(mirrorPath)
	// the local mirror lags after a sibling's API merge, so fetch main FIRST — else the
	// rebase target (and any conflict_resolver base derived from it) is a STALE pre-merge
	// commit, the rebase misses the real conflict or replays onto the wrong base, and the
	// resolution re-conflicts (the same stale-base bug fixed in project-out's merge path).
	_ = mirror.FetchBranch(branch)
	mainTip, err := mirror.HeadSHA("refs/heads/" + branch)
	if err != nil {
		return
	}
	stale, err := st.StaleReviewBuilds(ctx, repoID, mainTip)
	if err != nil || len(stale) == 0 {
		return
	}
	for _, jobID := range stale {
		res, rerr := st.RebaseOnto(ctx, mirror, store.RebaseOntoParams{
			JobID: jobID, NewBaseSHA: mainTip, Now: time.Now(),
		})
		if rerr != nil {
			logger.Warn("rebase-before-review", "job", jobID, "err", rerr)
			continue
		}
		switch {
		case res.Clean && res.NewSHA != "" && pushURL != "":
			// force-push the rebased head to the issue branch so the PR + CI track it.
			brName := store.IssueBranch(st.ResolveIssueNum(ctx, jobID), jobID)
			if perr := mirror.PushCommit(pushURL, res.NewSHA, brName); perr != nil {
				logger.Warn("rebase-before-review push", "job", jobID, "branch", brName, "err", perr)
				if _, rerr := st.RequeueJob(ctx, jobID, true, time.Now()); rerr != nil {
					logger.Warn("rebase-before-review push compensation", "job", jobID, "err", rerr)
				}
			} else {
				logger.Info("rebased review onto tip", "job", jobID, "branch", brName, "head", res.NewSHA[:min(7, len(res.NewSHA))])
			}
		case res.ResolverNeeded:
			logger.Info("review diverted to conflict_resolver before review", "job", jobID)
		}
	}
}

// controlMirrorFor resolves the control-plane bare mirror for a repo (F9 per-repo
// mirror). Backward-compatible: the legacy single-repo "default" keeps using
// FLOWBEE_MIRROR_PATH directly; additional repos get sibling mirrors <dir>/<id>.git,
// so a non-default repo's build NEVER resolves base_sha from another repo's tree.
// Empty when no mirror is configured.
func controlMirrorFor(r store.Repo) string {
	return gitops.RepoMirrorPath(os.Getenv("FLOWBEE_MIRROR_PATH"), r.ID)
}

// repoTokenURL builds the credential-bearing clone/push URL for a repo from the
// shared FLOWBEE_GITHUB_TOKEN (the local mirror sweep's auth; per-repo token_env is
// honored for the GitHub-API writer in wireMultiRepo). Empty if coords/token missing.
func repoTokenURL(r store.Repo) string {
	tok := os.Getenv("FLOWBEE_GITHUB_TOKEN")
	if r.Owner == "" || r.Repo == "" || tok == "" {
		return ""
	}
	return "https://x-access-token:" + tok + "@github.com/" + r.Owner + "/" + r.Repo + ".git"
}

// repoTokenWarning returns a startup warning (or "") for a registered repo whose
// GitHub token is missing or silently falling back — the multi-repo footgun where a
// repo you added to the registry never moves because its credential isn't wired. Two
// cases: (1) no token at all (per-repo env empty AND no shared FLOWBEE_GITHUB_TOKEN) →
// every GitHub call 401s and the repo's reconcile/project loops no-op; (2) a declared
// token_env that is UNSET while a shared token exists → the repo quietly uses the
// shared token, which may lack access to this repo (a later 403, not an obvious cause).
// doctor flags these too; this is the runtime backstop for an operator who skipped it.
func repoTokenWarning(id, tokenEnvName, sharedTok, perRepoTok string) string {
	if perRepoTok == "" && sharedTok == "" {
		env := "FLOWBEE_GITHUB_TOKEN"
		if tokenEnvName != "" {
			env = tokenEnvName + " (or FLOWBEE_GITHUB_TOKEN)"
		}
		return fmt.Sprintf("repo %q has NO GitHub token (set %s) — its reconcile/merge loops will no-op until it does", id, env)
	}
	if tokenEnvName != "" && perRepoTok == "" && sharedTok != "" {
		return fmt.Sprintf("repo %q token_env %s is unset — falling back to the shared FLOWBEE_GITHUB_TOKEN, which may lack access to this repo", id, tokenEnvName)
	}
	return ""
}

// ensureRepoMirror clones a repo's bare mirror if absent (F9 per-repo provisioning),
// locking it down so the baked-in token isn't world/group-readable. A no-op when the
// mirror is already present or coords are missing.
func ensureRepoMirror(logger *slog.Logger, mp, url string) {
	if mp == "" || url == "" {
		return
	}
	if _, err := os.Stat(mp); err == nil {
		return // already provisioned
	}
	if err := gitops.CloneBareMirror(mp, url); err != nil {
		logger.Error("clone repo mirror", "path", mp, "err", err)
		return
	}
	_ = os.Chmod(mp, 0o700)
	_ = os.Chmod(filepath.Join(mp, "config"), 0o600)
}

func wireMultiRepo(ctx context.Context, logger *slog.Logger, cfg config.Config, st *store.Store, srv *api.Server) *multirepo.Manager {
	// (1) seed the registry. cfg.Repos (structured list) wins; else fall back to the
	// single-repo FLOWBEE_GITHUB_OWNER/REPO env path (registered under id "default").
	repos := cfg.Repos
	tokenEnv := map[string]string{}  // repo id -> token env-var name ("" = shared default)
	allowOwn := map[string]bool{}    // repo id -> relax flowbee_source (non-control-plane repo)
	archiveHist := map[string]bool{} // repo id -> opt into the durable §F history archive
	if len(repos) == 0 {
		// env wins (legacy path); else fall back to the flowbee.yaml coords
		// `flowbee init` prefills (F13).
		owner, repo := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO")
		if owner == "" {
			owner = cfg.GithubOwner
		}
		if repo == "" {
			repo = cfg.GithubRepo
		}
		if owner == "" || repo == "" {
			return nil // nothing configured: no GitHub loops
		}
		branch := cfg.GithubDefaultBranch
		if branch == "" {
			branch = "main"
		}
		repos = []config.RepoConfig{{ID: "default", Owner: owner, Repo: repo, DefaultBranch: branch}}
	}
	for _, rc := range repos {
		id := rc.ID
		if id == "" {
			id = rc.Repo
		}
		if err := st.RegisterRepo(ctx, store.Repo{
			ID: id, Owner: rc.Owner, Repo: rc.Repo,
			DefaultBranch: rc.DefaultBranch, Active: rc.IsActive(),
		}); err != nil {
			logger.Error("register repo", "id", id, "err", err)
			continue
		}
		logger.Info("repo policy",
			"id", id,
			"repo", rc.Owner+"/"+rc.Repo,
			"allow_own_source_merge", rc.AllowOwnSourceMerge,
			"archive_history", rc.ArchiveHistory,
		)
		tokenEnv[id] = rc.TokenEnv
		if rc.AllowOwnSourceMerge {
			allowOwn[id] = true
		}
		if rc.ArchiveHistory {
			archiveHist[id] = true
		}
		// name a repo that will silently no-op for lack of (the right) token NOW, at
		// startup, instead of leaving the operator to wonder why one repo never moves.
		if rc.IsActive() {
			perRepo := ""
			if rc.TokenEnv != "" {
				perRepo = os.Getenv(rc.TokenEnv)
			}
			if msg := repoTokenWarning(id, rc.TokenEnv, os.Getenv("FLOWBEE_GITHUB_TOKEN"), perRepo); msg != "" {
				logger.Warn("⚠️  " + msg)
			}
		}
	}

	// (2) per-repo GitHub factory: each repo gets its OWN RealClient bearing its
	// per-repo PAT (or the shared FLOWBEE_GITHUB_TOKEN). Workers hold NO creds (F3).
	sharedTok := os.Getenv("FLOWBEE_GITHUB_TOKEN")
	factory := func(r store.Repo) (github.Client, github.Writer, error) {
		tok := sharedTok
		if env := tokenEnv[r.ID]; env != "" {
			if v := os.Getenv(env); v != "" {
				tok = v
			}
		}
		client := github.NewRealClient(r.Owner, r.Repo, func(context.Context) (string, error) { return tok, nil })
		return client, client, nil
	}
	// (3) F11 (build-list §F): wire the LOCAL-git history writer so each repo's
	// project-OUT loop lands the dedicated post-merge issue-archive commit
	// (docs/history/<id>.md + the TOC) on the integration branch. The shared bare
	// mirror is the same one workers' worktrees/bundles come off; an unset mirror
	// path leaves history.write rows as audited no-ops (the ledger stays canonical).
	// F2: relax flowbee_source for non-control-plane repos at BOTH gate sites — the
	// store's per-job content check AND the project-OUT merge cross-check (they must
	// agree). Empty (no repo opted in) = the shipped fully-protected posture.
	st.AllowOwnSourceRepos = allowOwn
	var historyOpt []multirepo.Option
	historyOpt = append(historyOpt, multirepo.WithAllowOwnSource(allowOwn))
	historyOpt = append(historyOpt, multirepo.WithArchiveHistory(archiveHist))
	historyOpt = append(historyOpt, multirepo.WithAutoMergeHandoff(cfg.AllowSelfMerge))
	historyOpt = append(historyOpt, multirepo.WithLogger(logger)) // durable dead-letter records
	if os.Getenv("FLOWBEE_MIRROR_PATH") != "" {
		// F9: each repo's history archive + base_sha resolution come off ITS OWN bare
		// mirror (provisioned lazily), not one shared mirror — so a non-default repo
		// never reads another repo's tree.
		historyOpt = append(historyOpt, multirepo.WithHistory(func(r store.Repo) project.HistoryWriter {
			mp := controlMirrorFor(r)
			if mp == "" {
				return nil
			}
			ensureRepoMirror(logger, mp, repoTokenURL(r))
			return gitops.Open(mp)
		}))
	}
	mgr, err := multirepo.New(ctx, st, clock.Real{}, srv.Broker(), factory, historyOpt...)
	if err != nil {
		logger.Error("build multi-repo manager", "err", err)
		return nil
	}
	if len(mgr.Repos()) == 0 {
		return nil
	}
	return mgr
}

// repoRefetcher adapts the multi-repo Manager to the webhook.Refetcher interface:
// a hint is routed to the default repo's reconciler (a richer X-Flowbee-Repo router
// is a later enhancement; the floor sweep reconciles every repo regardless).
type repoRefetcher struct {
	mgr         *multirepo.Manager
	defaultRepo string
}

func (r repoRefetcher) RefetchHint(ctx context.Context, prNumber int) bool {
	return r.mgr.RefetchHint(ctx, r.defaultRepo, prNumber)
}

// IntakeSweep runs a reconcile sweep across all repos so a freshly labeled/opened issue
// is adopted immediately on the webhook, not on the next floor poll.
func (r repoRefetcher) IntakeSweep(ctx context.Context) bool {
	if _, err := r.mgr.SweepAll(ctx); err != nil {
		return false
	}
	return true
}

func firstRepo(mgr *multirepo.Manager) string {
	if rs := mgr.Repos(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

// logProjectBreakerReport keeps per-scope failures visible without turning one
// poison repository into a pass-level error that could suppress healthy scopes.
func logProjectBreakerReport(logger *slog.Logger, message string, report projectbreaker.Report) {
	logger.Info(message, "claimed", report.Claimed, "recovered", report.Recovered,
		"reopened", report.Reopened, "poisoned", report.Poisoned)
	for _, outcome := range report.Outcomes {
		if outcome.Err != nil {
			logger.Warn("project breaker scope remained open", "project_id", outcome.ProjectID,
				"repo_id", outcome.RepoID, "probe_epoch", outcome.Epoch,
				"poisoned", outcome.Poisoned, "err", outcome.Err)
		}
	}
}

func newProductionProjectBreakerRunner(st *store.Store, mgr *multirepo.Manager, owner string) projectbreaker.Runner {
	return projectbreaker.Runner{
		Store: st,
		Probe: projectbreaker.MechanicalDependencyProbe{
			Projects: st, Repositories: mgr, RetryAfter: time.Minute,
		},
		Config: projectbreaker.Config{
			Owner: owner, ClaimTTL: time.Minute, FailureRetryAfter: time.Minute, Budget: 25,
		},
	}
}

func serveHTTP(logger *slog.Logger, name string, srv *http.Server, errc chan<- error) {
	logger.Info("listening", "server", name, "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// a bind failure (e.g. the port is already in use — a duplicate serve/up) is FATAL:
		// the control plane cannot do its job without this listener. Surface it so main exits
		// LOUDLY (non-zero) instead of lingering as a healthy-looking process that never
		// serves — which would let `flowbee up` proceed against the wrong control plane.
		logger.Error("http server", "server", name, "err", err)
		select {
		case errc <- fmt.Errorf("%s listener (%s): %w", name, srv.Addr, err):
		default:
		}
	}
}
