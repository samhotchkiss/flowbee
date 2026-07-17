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

import { flowbee } from "../flowbee/service";
import type { SessionEntry } from "../flowbee/types";
import { masterKey, noteKey, promptKey } from "../render";
import { DEFAULTS } from "../settings";
import { focusSession, sendPrompt } from "../tmux";

const logger = streamDeck.logger.createScope("Master");

/** The master session: per-key override first, then the shared global setting. */
function masterName(override?: string): string {
	return (override ?? "").trim() || (flowbee.settings.masterSession ?? "").trim();
}

/** Registry entry for the master (if registered) — supplies box + watchdog state. */
function masterEntry(name: string): SessionEntry | undefined {
	const entries = flowbee.state<SessionEntry[]>("sessions").data ?? [];
	return entries.find((e) => e.tmux_name === name) ?? entries.find((e) => e.id === name);
}

function sessionItems(): { value: string; label: string }[] {
	const entries = flowbee.state<SessionEntry[]>("sessions").data ?? [];
	return [
		{ value: "", label: "Use the global master session" },
		...entries.map((e) => ({ value: e.tmux_name, label: e.tmux_name })),
	];
}

async function focusMaster(name: string): Promise<void> {
	// resolve against a fetched registry so a remote master's box is honored.
	if (!flowbee.state<SessionEntry[]>("sessions").data) await flowbee.refresh("sessions");
	const entry = masterEntry(name);
	await focusSession({
		tmuxName: entry?.tmux_name ?? name,
		box: entry?.box,
		terminalApp: flowbee.settings.terminalApp ?? DEFAULTS.terminalApp,
	});
}

type FocusSettings = { session?: string };

@action({ UUID: "com.samhotchkiss.flowbee.master-focus" })
export class MasterFocusAction extends SingletonAction<FocusSettings> {
	private views = new Map<string, { action: KeyAction<FocusSettings>; settings: FocusSettings }>();
	private unsubscribe?: () => void;

	override onWillAppear(ev: WillAppearEvent<FocusSettings>): void {
		if (!ev.action.isKey()) return;
		this.views.set(ev.action.id, { action: ev.action, settings: ev.payload.settings });
		if (!this.unsubscribe) {
			this.unsubscribe = flowbee.subscribe("sessions", () => this.renderAll());
		} else {
			this.render(this.views.get(ev.action.id)!);
		}
	}

	override onWillDisappear(ev: WillDisappearEvent<FocusSettings>): void {
		this.views.delete(ev.action.id);
		if (this.views.size === 0) {
			this.unsubscribe?.();
			this.unsubscribe = undefined;
		}
	}

	override onDidReceiveSettings(ev: DidReceiveSettingsEvent<FocusSettings>): void {
		const view = this.views.get(ev.action.id);
		if (!view) return;
		view.settings = ev.payload.settings;
		this.render(view);
	}

	override async onKeyDown(ev: KeyDownEvent<FocusSettings>): Promise<void> {
		const name = masterName(ev.payload.settings.session);
		if (!name) {
			await ev.action.showAlert();
			return;
		}
		try {
			await focusMaster(name);
		} catch (e) {
			logger.warn(`focus master: ${e instanceof Error ? e.message : e}`);
			await ev.action.showAlert();
		}
	}

	override onSendToPlugin(ev: SendToPluginEvent<{ event?: string }, FocusSettings>): void {
		if (ev.payload?.event !== "getSessions") return;
		void streamDeck.ui.sendToPropertyInspector({ event: "getSessions", items: sessionItems() }).catch(() => {});
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: { action: KeyAction<FocusSettings>; settings: FocusSettings }): void {
		const name = masterName(view.settings.session);
		if (!name) {
			void view.action.setImage(noteKey("MASTER", "set session")).catch(() => {});
			return;
		}
		const entry = masterEntry(name);
		void view.action
			.setImage(masterKey({ name, state: entry && entry.state !== "unwatched" ? entry.state : undefined }))
			.catch(() => {});
	}
}

type PromptSettings = {
	session?: string;
	prompt?: string;
	label?: string;
	/** stay on the current window instead of jumping to the master after sending. */
	stayPut?: boolean;
};

const DEFAULT_PROMPT = "Give me the current status of all of our goals";

@action({ UUID: "com.samhotchkiss.flowbee.master-prompt" })
export class MasterPromptAction extends SingletonAction<PromptSettings> {
	// subscribes to "sessions" like MasterFocusAction: masterEntry() needs the
	// registry to resolve a remote master's box, and the PI's session picker
	// reads the same data — without a subscription both silently see nothing
	// whenever no goal-session/master-focus key is on the visible page.
	private visible = new Set<string>();
	private unsubscribe?: () => void;

	override onWillAppear(ev: WillAppearEvent<PromptSettings>): void {
		if (!ev.action.isKey()) return;
		this.visible.add(ev.action.id);
		if (!this.unsubscribe) this.unsubscribe = flowbee.subscribe("sessions", () => {});
		void ev.action.setImage(promptKey({ label: ev.payload.settings.label?.trim() || "goal status" })).catch(() => {});
	}

	override onWillDisappear(ev: WillDisappearEvent<PromptSettings>): void {
		this.visible.delete(ev.action.id);
		if (this.visible.size === 0) {
			this.unsubscribe?.();
			this.unsubscribe = undefined;
		}
	}

	override onDidReceiveSettings(ev: DidReceiveSettingsEvent<PromptSettings>): void {
		void ev.action.setImage(promptKey({ label: ev.payload.settings.label?.trim() || "goal status" })).catch(() => {});
	}

	override async onKeyDown(ev: KeyDownEvent<PromptSettings>): Promise<void> {
		const { settings } = ev.payload;
		const name = masterName(settings.session);
		if (!name) {
			await ev.action.showAlert();
			return;
		}
		// belt-and-suspenders: make sure the registry was actually fetched before
		// resolving box/tmux, so a remote master never falls back to local tmux.
		if (!flowbee.state<SessionEntry[]>("sessions").data) await flowbee.refresh("sessions");
		const entry = masterEntry(name);
		try {
			await sendPrompt({
				tmuxName: entry?.tmux_name ?? name,
				box: entry?.box,
				text: settings.prompt?.trim() || DEFAULT_PROMPT,
			});
			await ev.action.showOk();
			if (!settings.stayPut) await focusMaster(name);
		} catch (e) {
			logger.warn(`prompt master: ${e instanceof Error ? e.message : e}`);
			await ev.action.showAlert();
		}
	}

	override onSendToPlugin(ev: SendToPluginEvent<{ event?: string }, PromptSettings>): void {
		if (ev.payload?.event !== "getSessions") return;
		void streamDeck.ui.sendToPropertyInspector({ event: "getSessions", items: sessionItems() }).catch(() => {});
	}
}
