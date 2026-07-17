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
import { noteKey, pauseKey } from "../render";

type Settings = {
	/** repo id to park/unpark; "" = pause everything. */
	repo?: string;
};

const logger = streamDeck.logger.createScope("PauseToggle");

@action({ UUID: "com.samhotchkiss.flowbee.pause-toggle" })
export class PauseToggleAction extends SingletonAction<Settings> {
	private views = new Map<string, { action: KeyAction<Settings>; settings: Settings }>();
	private unsubscribe?: () => void;

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
		const repo = ev.payload.settings.repo?.trim() ?? "";
		const control = flowbee.state<ControlStatus>("control").data;
		if (!control) {
			await ev.action.showAlert();
			return;
		}
		const paused = repo ? control.parked_repos.includes(repo) : control.dispatch_paused;
		try {
			if (paused) {
				await flowbee.api.resume(repo);
			} else {
				await flowbee.api.pause(repo);
			}
			await flowbee.refresh("control");
		} catch (e) {
			logger.warn(`toggle (repo=${repo || "all"}): ${e instanceof Error ? e.message : e}`);
			await ev.action.showAlert();
		}
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: { action: KeyAction<Settings>; settings: Settings }): void {
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
