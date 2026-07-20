import {
	action,
	KeyAction,
	SingletonAction,
	type DidReceiveSettingsEvent,
	type KeyDownEvent,
	type WillAppearEvent,
	type WillDisappearEvent,
} from "@elgato/streamdeck";
import streamDeck from "@elgato/streamdeck";

import { HttpError } from "../flowbee/client";
import { flowbee } from "../flowbee/service";
import type { ControlStatus } from "../flowbee/types";
import { noteKey, pauseConfirmKey, pauseKey } from "../render";

type Settings = {
	/** repo id to park/unpark; "" = pause everything. */
	repo?: string;
};

const logger = streamDeck.logger.createScope("PauseToggle");
const CONFIRM_WINDOW_MS = 3_000;

@action({ UUID: "com.samhotchkiss.flowbee.pause-toggle" })
export class PauseToggleAction extends SingletonAction<Settings> {
	private views = new Map<string, { action: KeyAction<Settings>; settings: Settings }>();
	private unsubscribe?: () => void;
	/** keys armed for the global-pause second press (a bump must not halt the fleet). */
	private confirmTimers = new Map<string, NodeJS.Timeout>();

	override onWillAppear(ev: WillAppearEvent<Settings>): void {
		if (!ev.action.isKey()) return;
		this.views.set(ev.action.id, { action: ev.action, settings: ev.payload.settings });
		if (!this.unsubscribe) {
			this.unsubscribe = flowbee.subscribe("control", () => this.renderAll());
		} else {
			this.render(this.views.get(ev.action.id)!);
		}
	}

	override onWillDisappear(ev: WillDisappearEvent<Settings>): void {
		this.views.delete(ev.action.id);
		this.disarm(ev.action.id);
		if (this.views.size === 0) {
			this.unsubscribe?.();
			this.unsubscribe = undefined;
		}
	}

	override onDidReceiveSettings(ev: DidReceiveSettingsEvent<Settings>): void {
		const view = this.views.get(ev.action.id);
		if (!view) return;
		view.settings = ev.payload.settings;
		// a scope change invalidates any armed confirm — the arming belonged to
		// the OLD scope, and render() would otherwise stay suppressed.
		this.disarm(ev.action.id);
		this.render(view);
	}

	override async onKeyDown(ev: KeyDownEvent<Settings>): Promise<void> {
		const repo = ev.payload.settings.repo?.trim() ?? "";
		const view = this.views.get(ev.action.id);
		const wasArmed = this.confirmTimers.has(ev.action.id);
		this.disarm(ev.action.id);
		const control = flowbee.state<ControlStatus>("control").data;
		if (!control) {
			await ev.action.showAlert();
			if (view) this.render(view);
			return;
		}
		const paused = repo ? control.parked_repos.includes(repo) : control.dispatch_paused;
		// pausing EVERYTHING takes two presses within 3s — one physical bump must
		// not halt the fleet. Resume and per-repo parking stay single-press.
		if (!paused && !repo && !wasArmed) {
			this.confirmTimers.set(
				ev.action.id,
				setTimeout(() => {
					this.confirmTimers.delete(ev.action.id);
					if (view) this.render(view);
				}, CONFIRM_WINDOW_MS),
			);
			void ev.action.setImage(pauseConfirmKey()).catch(() => {});
			return;
		}
		try {
			if (wasArmed && !repo) {
				// the armed intent is PAUSE and nothing else: if dispatch got paused
				// during the window (another key, the CLI), confirming is a no-op —
				// it must never invert into a resume.
				if (!paused) await flowbee.api.pause("");
			} else if (paused) {
				await flowbee.api.resume(repo);
			} else {
				await flowbee.api.pause(repo);
			}
			await flowbee.refresh("control");
		} catch (e) {
			logger.warn(`toggle (repo=${repo || "all"}): ${e instanceof Error ? e.message : e}`);
			await ev.action.showAlert();
		} finally {
			// never leave a stale confirm frame: repaint from real state even when
			// the API call failed (the disarmed timer can no longer do it).
			if (view) this.render(view);
		}
	}

	private disarm(actionId: string): void {
		const t = this.confirmTimers.get(actionId);
		if (t) {
			clearTimeout(t);
			this.confirmTimers.delete(actionId);
		}
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: { action: KeyAction<Settings>; settings: Settings }): void {
		// an armed confirm frame owns the key until it fires or times out.
		if (this.confirmTimers.has(view.action.id)) return;
		const { data, error } = flowbee.state<ControlStatus>("control");
		const set = (image: string) => void view.action.setImage(image).catch(() => {});
		if (!data) {
			if (error instanceof HttpError && error.status === 401) {
				// GET /v1/control sits on the worker-auth surface — off-loopback it needs a token.
				set(noteKey("PAUSE", "needs token"));
			} else {
				set(noteKey("PAUSE", error ? "no server" : "loading…"));
			}
			return;
		}
		const repo = view.settings.repo?.trim() ?? "";
		set(
			pauseKey({
				paused: repo ? data.parked_repos.includes(repo) : data.dispatch_paused,
				scope: repo || undefined,
			}),
		);
	}
}
