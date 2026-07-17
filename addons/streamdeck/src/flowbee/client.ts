import type {
	AccountUsage,
	Attention,
	ControlStatus,
	FleetHealth,
	GoalSession,
	MergeHandoffRow,
	NeedsHumanRow,
	NeedsInputRow,
} from "./types";

/** Error carrying the HTTP status so callers can branch (404 = older server, 401 = needs token). */
export class HttpError extends Error {
	constructor(
		public readonly status: number,
		public readonly body: string,
		path: string,
	) {
		super(`${path}: HTTP ${status}${body ? ` — ${body.slice(0, 200)}` : ""}`);
	}
}

const TIMEOUT_MS = 4_000;

/**
 * Thin fetch client for the Flowbee control plane. Read endpoints are open on
 * loopback; the token (when set) rides along for the authed control/ops surface.
 * Flowbee returns JSON `null` for several empty lists — normalized to [] here.
 */
export class FlowbeeClient {
	constructor(
		private readonly base: string,
		private readonly token?: string,
	) {}

	private headers(json = false): Record<string, string> {
		const h: Record<string, string> = {};
		if (json) h["Content-Type"] = "application/json";
		if (this.token) h["Authorization"] = `Bearer ${this.token}`;
		return h;
	}

	private async request<T>(method: "GET" | "POST", path: string, body?: unknown): Promise<T> {
		const res = await fetch(`${this.base}${path}`, {
			method,
			headers: this.headers(body !== undefined),
			body: body === undefined ? undefined : JSON.stringify(body),
			signal: AbortSignal.timeout(TIMEOUT_MS),
		});
		if (!res.ok) {
			// Flowbee error bodies are plain text, never JSON.
			throw new HttpError(res.status, (await res.text()).trim(), path);
		}
		return (await res.json()) as T;
	}

	private async list<T>(path: string): Promise<T[]> {
		return (await this.request<T[] | null>("GET", path)) ?? [];
	}

	fleet(): Promise<AccountUsage[]> {
		return this.list<AccountUsage>("/v1/fleet");
	}

	sessions(): Promise<GoalSession[]> {
		return this.list<GoalSession>("/v1/sessions");
	}

	control(): Promise<ControlStatus> {
		return this.request<ControlStatus>("GET", "/v1/control");
	}

	fleetHealth(): Promise<FleetHealth> {
		return this.request<FleetHealth>("GET", "/v1/fleet-health");
	}

	async attention(): Promise<Attention> {
		const [needsHuman, mergeHandoff, needsInput] = await Promise.all([
			this.list<NeedsHumanRow>("/v1/needs-human"),
			this.list<MergeHandoffRow>("/v1/merge-handoff"),
			this.list<NeedsInputRow>("/v1/needs-input"),
		]);
		return {
			needsHuman,
			mergeHandoff,
			needsInput,
			total: needsHuman.length + mergeHandoff.length + needsInput.length,
		};
	}

	/** Pause dispatch. Empty/omitted repo = global; a repo id parks just that repo. */
	pause(repo = ""): Promise<{ paused: boolean; scope: string }> {
		return this.request("POST", "/v1/control/pause", { repo });
	}

	resume(repo = ""): Promise<{ paused: boolean; scope: string }> {
		return this.request("POST", "/v1/control/resume", { repo });
	}

	get baseUrl(): string {
		return this.base;
	}
}
