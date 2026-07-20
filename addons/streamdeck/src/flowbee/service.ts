import streamDeck from "@elgato/streamdeck";

import { baseUrl, pollMs, type GlobalSettings } from "../settings";
import { listLocalTmuxSessions } from "../tmux";
import { FlowbeeClient, HttpError } from "./client";
import type { LifeEvent, SessionEntry } from "./types";

const logger = streamDeck.logger.createScope("FlowbeeService");

export type ResourceKey = "fleet" | "sessions" | "control" | "fleetHealth" | "attention";

export type ResourceState<T = unknown> = {
	data?: T;
	error?: Error;
	fetchedAt?: number;
};

type Listener = () => void;

type Resource = {
	fetch: () => Promise<unknown>;
	state: ResourceState;
	listeners: Set<Listener>;
	timer?: NodeJS.Timeout;
	inFlight?: Promise<void>;
};

/**
 * One shared data layer for every key on the deck: per-resource polling that
 * only runs while a key is subscribed, plus an SSE nudge (`/v1/events`) that
 * refreshes the affected resource immediately. The SSE stream is lossy by
 * design (64-slot buffer, no replay), so polling stays the source of truth
 * and SSE only tightens latency.
 */
class FlowbeeService {
	private gs: GlobalSettings = {};
	private client = new FlowbeeClient("http://127.0.0.1:7070");
	private resources = new Map<ResourceKey, Resource>();
	private sseAbort?: AbortController;
	private sseBackoffMs = 1_000;
	private sseDebounce?: NodeJS.Timeout;
	/** false once /v1/sessions 404s (older control plane without the endpoint). */
	private serverHasSessions = true;

	constructor() {
		const define = (key: ResourceKey, fetch: () => Promise<unknown>) =>
			this.resources.set(key, { fetch, state: {}, listeners: new Set() });

		define("fleet", () => this.client.fleet());
		define("control", () => this.client.control());
		define("fleetHealth", () => this.client.fleetHealth());
		define("attention", () => this.client.attention());
		define("sessions", () => this.fetchSessions());
	}

	/** Apply (possibly changed) global settings: new client, fresh data, fresh SSE. */
	configure(gs: GlobalSettings): void {
		this.gs = gs;
		this.client = new FlowbeeClient(baseUrl(gs), gs.token?.trim() || undefined);
		this.serverHasSessions = true;
		logger.info(`configured for ${this.client.baseUrl}`);
		if (this.client.tokenWithheld) {
			logger.warn("API token configured but WITHHELD: base URL is non-loopback http — use https (or loopback) to send credentials");
		}
		for (const [key, r] of this.resources) {
			if (r.listeners.size > 0) {
				this.restartTimer(key, r);
				void this.refresh(key);
			}
		}
		// force the SSE stream onto the (possibly new) base URL — restartSSE()
		// alone is a no-op while a connection to the OLD server is still open.
		this.sseAbort?.abort();
		this.sseAbort = undefined;
		this.restartSSE();
	}

	get settings(): GlobalSettings {
		return this.gs;
	}

	get api(): FlowbeeClient {
		return this.client;
	}

	/** Subscribe to a resource; polling starts with the first subscriber. Returns unsubscribe. */
	subscribe(key: ResourceKey, listener: Listener): () => void {
		const r = this.resources.get(key)!;
		r.listeners.add(listener);
		if (r.listeners.size === 1) {
			this.restartTimer(key, r);
			void this.refresh(key);
		} else if (r.state.fetchedAt) {
			queueMicrotask(listener);
		}
		this.restartSSE();
		return () => {
			r.listeners.delete(listener);
			if (r.listeners.size === 0 && r.timer) {
				clearInterval(r.timer);
				r.timer = undefined;
			}
			this.restartSSE();
		};
	}

	state<T>(key: ResourceKey): ResourceState<T> {
		return this.resources.get(key)!.state as ResourceState<T>;
	}

	/** Fetch a resource now (deduped against an in-flight fetch) and notify listeners. */
	refresh(key: ResourceKey): Promise<void> {
		const r = this.resources.get(key)!;
		if (r.inFlight) return r.inFlight;
		r.inFlight = (async () => {
			try {
				r.state = { data: await r.fetch(), fetchedAt: Date.now() };
			} catch (e) {
				const error = e instanceof Error ? e : new Error(String(e));
				// keep last-known data across a brief blip, but once the server has
				// been gone for ~3 poll intervals, drop it — a deck showing hours-old
				// "all clear" is worse than one showing "no server".
				const gone = !r.state.fetchedAt || Date.now() - r.state.fetchedAt > 3 * pollMs(this.gs);
				r.state = gone ? { error } : { ...r.state, error };
				logger.debug(`refresh ${key}: ${error.message}`);
			} finally {
				r.inFlight = undefined;
			}
			for (const l of r.listeners) l();
		})();
		return r.inFlight;
	}

	/**
	 * Goal sessions = the registry (`/v1/sessions`, watchdog-scored) first, then
	 * local tmux sessions the registry doesn't cover as "unwatched" entries — so
	 * row 2 is useful even before `flowbee session add`, and even with the
	 * control plane down (pure-tmux degraded mode).
	 */
	private async fetchSessions(): Promise<SessionEntry[]> {
		let registry: SessionEntry[] = [];
		let registryErr: Error | undefined;
		if (this.serverHasSessions) {
			try {
				registry = await this.client.sessions();
			} catch (e) {
				if (e instanceof HttpError && e.status === 404) {
					this.serverHasSessions = false;
					logger.warn("control plane has no GET /v1/sessions (older build) — showing local tmux only");
				} else {
					registryErr = e instanceof Error ? e : new Error(String(e));
				}
			}
		}
		const localSessions = await listLocalTmuxSessions();
		const localNames = new Set(localSessions.map((t) => t.name));
		// liveness: a local registry entry is running iff its tmux session exists
		// right now; a remote one can't be checked from here, so trust the
		// watchdog's verdict (achieved / unreachable / paused-watch = not running).
		for (const s of registry) {
			s.running = s.box
				? s.enabled && s.state !== "achieved" && s.state !== "unreachable"
				: localNames.has(s.tmux_name);
		}
		const covered = new Set(registry.filter((s) => !s.box).map((s) => s.tmux_name));
		const local = localSessions
			.filter((t) => !covered.has(t.name))
			.map((t) => ({
				id: t.name,
				tmux_name: t.name,
				state: "unwatched" as const,
				enabled: true as const,
				attached: t.attached,
				running: true as const,
			}));
		if (registryErr && local.length === 0) throw registryErr;
		return [...registry, ...local];
	}

	private restartTimer(key: ResourceKey, r: Resource): void {
		if (r.timer) clearInterval(r.timer);
		r.timer = setInterval(() => void this.refresh(key), pollMs(this.gs));
	}

	private get anySubscribers(): boolean {
		for (const r of this.resources.values()) if (r.listeners.size > 0) return true;
		return false;
	}

	// ── SSE nudge ──────────────────────────────────────────────────────────

	private restartSSE(): void {
		const want = this.anySubscribers;
		const have = !!this.sseAbort;
		if (want && !have) {
			void this.sseLoop();
		} else if (!want && have) {
			this.sseAbort?.abort();
			this.sseAbort = undefined;
		}
	}

	private async sseLoop(): Promise<void> {
		const abort = new AbortController();
		this.sseAbort = abort;
		while (!abort.signal.aborted && this.anySubscribers) {
			const connectedAt = Date.now();
			try {
				// /v1/events is on Flowbee's open read tier today; the auth header
				// (same transport policy as the client) is for the authed SSE topics
				// coming in the epic-lane API.
				const res = await fetch(`${this.client.baseUrl}/v1/events`, {
					headers: this.client.authHeaders(),
					signal: abort.signal,
				});
				if (!res.ok || !res.body) throw new Error(`SSE HTTP ${res.status}`);
				logger.info("SSE connected");
				// connected after an outage — re-sync everything we watch.
				this.refreshActive();
				const reader = res.body.getReader();
				const decoder = new TextDecoder();
				let buf = "";
				for (;;) {
					const { done, value } = await reader.read();
					if (done) break;
					// only a real frame proves the stream works — resetting backoff on
					// a bare 200 turns a connect-then-close endpoint into a 1s storm.
					this.sseBackoffMs = 1_000;
					buf += decoder.decode(value, { stream: true });
					let i: number;
					while ((i = buf.indexOf("\n\n")) >= 0) {
						const frame = buf.slice(0, i);
						buf = buf.slice(i + 2);
						for (const line of frame.split("\n")) {
							if (line.startsWith("data:")) this.onLifeEvent(line.slice(5).trim());
						}
					}
				}
			} catch (e) {
				if (abort.signal.aborted) break;
				logger.debug(`SSE: ${e instanceof Error ? e.message : e}`);
			}
			if (abort.signal.aborted) break;
			// a connection that survived a while was healthy even if the fleet was
			// quiet (Flowbee sends no keepalives) — start the next one promptly.
			if (Date.now() - connectedAt > 60_000) this.sseBackoffMs = 1_000;
			await new Promise((r) => setTimeout(r, this.sseBackoffMs));
			this.sseBackoffMs = Math.min(this.sseBackoffMs * 2, 30_000);
		}
		if (this.sseAbort === abort) this.sseAbort = undefined;
	}

	private onLifeEvent(json: string): void {
		let ev: LifeEvent;
		try {
			ev = JSON.parse(json) as LifeEvent;
		} catch {
			return;
		}
		if (ev.state === "control") {
			void this.refresh("control");
		} else if (ev.state === "capacity") {
			void this.refresh("fleet");
		} else {
			// job-lifecycle event: attention/fleet-health counters may have moved.
			if (this.sseDebounce) clearTimeout(this.sseDebounce);
			this.sseDebounce = setTimeout(() => {
				for (const key of ["attention", "fleetHealth"] as const) {
					if (this.resources.get(key)!.listeners.size > 0) void this.refresh(key);
				}
			}, 400);
		}
	}

	private refreshActive(): void {
		for (const [key, r] of this.resources) {
			if (r.listeners.size > 0) void this.refresh(key);
		}
	}
}

export const flowbee = new FlowbeeService();
