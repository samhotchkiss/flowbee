import {
	action,
	KeyAction,
	SingletonAction,
	type KeyDownEvent,
	type WillAppearEvent,
	type WillDisappearEvent,
} from "@elgato/streamdeck";

import { flasher } from "../flash";
import { flowbee } from "../flowbee/service";
import type { Attention } from "../flowbee/types";
import { openUrl } from "../macos";
import { attentionKey, noteKey } from "../render";

@action({ UUID: "com.samhotchkiss.flowbee.attention" })
export class AttentionAction extends SingletonAction {
	private views = new Map<string, KeyAction>();
	private unsubscribe?: () => void;

	override onWillAppear(ev: WillAppearEvent): void {
		if (!ev.action.isKey()) return;
		this.views.set(ev.action.id, ev.action);
		if (!this.unsubscribe) {
			this.unsubscribe = flowbee.subscribe("attention", () => this.renderAll());
		} else {
			this.render(ev.action);
		}
	}

	override onWillDisappear(ev: WillDisappearEvent): void {
		this.views.delete(ev.action.id);
		flasher.unregister(ev.action.id);
		if (this.views.size === 0) {
			this.unsubscribe?.();
			this.unsubscribe = undefined;
		}
	}

	override async onKeyDown(ev: KeyDownEvent): Promise<void> {
		try {
			await openUrl(`${flowbee.api.baseUrl}/dashboard`);
		} catch {
			await ev.action.showAlert().catch(() => {});
		}
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: KeyAction): void {
		const { data, error } = flowbee.state<Attention>("attention");
		if (!data) {
			void view.setImage(noteKey("ALERTS", error ? "no server" : "loading…")).catch(() => {});
			flasher.unregister(view.id);
			return;
		}
		if (data.total > 0) {
			flasher.register(view.id, () => this.render(view));
		} else {
			flasher.unregister(view.id);
		}
		const parts: string[] = [];
		if (data.needsHuman.length) parts.push(`${data.needsHuman.length} human`);
		if (data.mergeHandoff.length) parts.push(`${data.mergeHandoff.length} merge`);
		if (data.needsInput.length) parts.push(`${data.needsInput.length} input`);
		void view
			.setImage(attentionKey({ total: data.total, breakdown: parts.join(" · "), flash: flasher.phase }))
			.catch(() => {});
	}
}
