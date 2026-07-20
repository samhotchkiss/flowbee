import {
	action,
	KeyAction,
	SingletonAction,
	type DidReceiveSettingsEvent,
	type KeyDownEvent,
	type SendToPluginEvent,
	type WillAppearEvent,
	type WillDisappearEvent,
} from "@elgato/streamdeck";
import streamDeck from "@elgato/streamdeck";

import { flasher } from "../flash";
import { flowbee } from "../flowbee/service";
import type { SessionEntry } from "../flowbee/types";
import { noteKey, sessionKey } from "../render";
import { DEFAULTS, assertBoxAllowed } from "../settings";
import { focusSession } from "../tmux";

type Settings = {
	/** registry id, "tmux:<name>" for an unwatched session, or "" = auto by key column. */
	session?: string;
};

type View = {
	action: KeyAction<Settings>;
	column: number;
	settings: Settings;
};

const logger = streamDeck.logger.createScope("GoalSession");

/**
 * The auto-slot pool: sessions that are live RIGHT NOW (local = tmux session
 * exists; remote = watchdog says so). Set keys up once and they populate and
 * vacate on their own as sessions start and finish — no Stream Deck app trips.
 */
export function runningEntries(entries: SessionEntry[]): SessionEntry[] {
	return entries.filter((e) => e.running !== false);
}

export function resolveEntry(entries: SessionEntry[], selector: string | undefined, column: number): SessionEntry | undefined {
	if (!selector) return runningEntries(entries)[column];
	if (selector.startsWith("tmux:")) {
		const name = selector.slice(5);
		return entries.find((e) => e.tmux_name === name) ?? {
			id: name,
			tmux_name: name,
			state: "unwatched",
			enabled: true,
			attached: false,
		};
	}
	return entries.find((e) => e.id === selector) ?? entries.find((e) => e.tmux_name === selector);
}

/** "2026-07-16T22:47:00-06:00" → "→ 22:47" (the watchdog's auto-resume gate). */
function untilLabel(rfc3339?: string): string | undefined {
	if (!rfc3339) return undefined;
	const t = Date.parse(rfc3339);
	if (Number.isNaN(t)) return undefined;
	const d = new Date(t);
	return `→ ${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

@action({ UUID: "com.samhotchkiss.flowbee.goal-session" })
export class GoalSessionAction extends SingletonAction<Settings> {
	private views = new Map<string, View>();
	private unsubscribe?: () => void;

	override onWillAppear(ev: WillAppearEvent<Settings>): void {
		if (!ev.action.isKey()) return;
		const coords = "coordinates" in ev.payload ? ev.payload.coordinates : undefined;
		this.views.set(ev.action.id, {
			action: ev.action,
			column: coords?.column ?? 0,
			settings: ev.payload.settings,
		});
		if (!this.unsubscribe) {
			this.unsubscribe = flowbee.subscribe("sessions", () => this.renderAll());
		} else {
			this.render(this.views.get(ev.action.id)!);
		}
	}

	override onWillDisappear(ev: WillDisappearEvent<Settings>): void {
		this.views.delete(ev.action.id);
		flasher.unregister(ev.action.id);
		if (this.views.size === 0) {
			this.unsubscribe?.();
			this.unsubscribe = undefined;
		}
	}

	override onDidReceiveSettings(ev: DidReceiveSettingsEvent<Settings>): void {
		const view = this.views.get(ev.action.id);
		if (!view) return;
		view.settings = ev.payload.settings;
		this.render(view);
	}

	override async onKeyDown(ev: KeyDownEvent<Settings>): Promise<void> {
		const view = this.views.get(ev.action.id);
		const entries = flowbee.state<SessionEntry[]>("sessions").data ?? [];
		const entry = resolveEntry(entries, view?.settings.session, view?.column ?? 0);
		if (!entry) {
			await ev.action.showAlert();
			return;
		}
		try {
			assertBoxAllowed(flowbee.settings, entry.box);
			await focusSession({
				tmuxName: entry.tmux_name,
				box: entry.box,
				terminalApp: flowbee.settings.terminalApp ?? DEFAULTS.terminalApp,
			});
		} catch (e) {
			logger.warn(`focus ${entry.tmux_name}: ${e instanceof Error ? e.message : e}`);
			await ev.action.showAlert();
		}
	}

	/** Datasource for the property inspector's session picker (grouped). */
	override onSendToPlugin(ev: SendToPluginEvent<{ event?: string }, Settings>): void {
		if (ev.payload?.event !== "getSessions") return;
		const entries = flowbee.state<SessionEntry[]>("sessions").data ?? [];
		const watched = entries.filter((e) => e.state !== "unwatched");
		const unwatched = entries.filter((e) => e.state === "unwatched");
		void streamDeck.ui
			.sendToPropertyInspector({
				event: "getSessions",
				items: [
					{ value: "", label: "Auto — running sessions fill keys by column" },
					...(watched.length
						? [{ label: "Goal sessions (watched)", children: watched.map((e) => ({ value: e.id, label: `${e.id} — ${e.state}` })) }]
						: []),
					...(unwatched.length
						? [{ label: "Local tmux (unwatched)", children: unwatched.map((e) => ({ value: `tmux:${e.tmux_name}`, label: e.tmux_name })) }]
						: []),
				],
			})
			.catch(() => {});
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: View): void {
		const { data, error } = flowbee.state<SessionEntry[]>("sessions");
		const set = (image: string) => void view.action.setImage(image).catch(() => {});

		if (!data) {
			set(noteKey("EPIC", error ? "no server" : "loading…"));
			return;
		}
		const entry = resolveEntry(data, view.settings.session, view.column);
		if (!entry) {
			set(noteKey("—", view.settings.session ? "gone?" : `slot ${view.column + 1} idle`));
			flasher.unregister(view.action.id);
			return;
		}
		const alarmed = entry.state === "blocked" || entry.state === "unreachable";
		if (alarmed) {
			flasher.register(view.action.id, () => this.render(view));
		} else {
			flasher.unregister(view.action.id);
		}
		const g = entry.state === "unwatched" ? undefined : entry;
		set(
			sessionKey({
				name: entry.id,
				state: entry.enabled ? entry.state : "unknown",
				footer:
					untilLabel(g && "blocked_until" in g ? g.blocked_until : undefined) ??
					(g && "goal_elapsed" in g ? g.goal_elapsed : undefined) ??
					(g && "state_detail" in g ? g.state_detail : undefined) ??
					(entry.state === "unwatched" ? "not registered" : undefined),
				remote: !!entry.box,
				flash: flasher.phase,
			}),
		);
	}
}
