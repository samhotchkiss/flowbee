/** Wire types for the Flowbee read/control API (field names match the Go json tags). */

/** GET /v1/fleet — one per-account usage gauge. */
export type AccountUsage = {
	account_id: string;
	model_family: string;
	ceiling_pct: number;
	preference_rank: number;
	usage_pct: number;
	rate_limited: boolean;
	at_ceiling: boolean;
	/** RFC3339; absent when the account has never reported. Only staleness signal (>24h = stale). */
	reported_at?: string;
};

/** GET /v1/sessions — one goal-session registry entry (watchdog-watched tmux session). */
export type GoalSession = {
	id: string;
	box?: string;
	tmux_name: string;
	tz?: string;
	repo?: string;
	note?: string;
	state: "pursuing" | "working" | "blocked" | "achieved" | "unknown" | "unreachable" | string;
	state_detail?: string;
	goal_elapsed?: string;
	blocked_until?: string;
	enabled: boolean;
	last_change_at?: string;
	last_checked_at?: string;
};

/** A local tmux session that is not in the goal-session registry (fallback listing). */
export type UnwatchedSession = {
	id: string;
	tmux_name: string;
	state: "unwatched";
	enabled: true;
	box?: undefined;
	attached: boolean;
};

export type SessionEntry = GoalSession | UnwatchedSession;

/** GET /v1/control */
export type ControlStatus = {
	dispatch_paused: boolean;
	parked_repos: string[];
};

/** GET /v1/fleet-health */
export type FleetHealth = {
	live_workers: number;
	stale_workers: number;
	waiting_jobs: number;
	stranded: boolean;
};

/** GET /v1/needs-human */
export type NeedsHumanRow = { job_id: string; flow: string; role: string; reason: string };
/** GET /v1/merge-handoff */
export type MergeHandoffRow = {
	job_id: string;
	repo: string;
	issue_number: number;
	pr_number: number;
	reason?: string;
	self_merge: boolean;
};
/** GET /v1/needs-input */
export type NeedsInputRow = { job_id: string; state: string; reason: string };

export type Attention = {
	needsHuman: NeedsHumanRow[];
	mergeHandoff: MergeHandoffRow[];
	needsInput: NeedsInputRow[];
	total: number;
};

/** SSE /v1/events frame. */
export type LifeEvent = { job_id: string; state: string; event: string; lease_epoch: number };
