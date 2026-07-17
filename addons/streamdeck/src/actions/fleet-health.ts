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
import type { FleetHealth } from "../flowbee/types";
import { openUrl } from "../macos";
import { fleetKey, noteKey } from "../render";

@action({ UUID: "com.samhotchkiss.flowbee.fleet-health" })
export class FleetHealthAction extends SingletonAction {
	private views = new Map<string, KeyAction>();
	private unsubscribe?: () => void;

	override onWillAppear(ev: WillAppearEvent): void {
		if (!ev.action.isKey()) return;
		this.views.set(ev.action.id, ev.action);
		if (!this.unsubscribe) {
			this.unsubscribe = flowbee.subscribe("fleetHealth", () => this.renderAll());
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
			await openUrl(`${flowbee.api.baseUrl}/fleet`);
		} catch {
			await ev.action.showAlert().catch(() => {});
		}
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: KeyAction): void {
		const { data, error } = flowbee.state<FleetHealth>("fleetHealth");
		if (!data) {
			void view.setImage(noteKey("FLEET", error ? "no server" : "loading…")).catch(() => {});
			flasher.unregister(view.id);
			return;
		}
		if (data.stranded) {
			flasher.register(view.id, () => this.render(view));
		} else {
			flasher.unregister(view.id);
		}
		void view
			.setImage(
				fleetKey({
					live: data.live_workers,
					stale: data.stale_workers,
					waiting: data.waiting_jobs,
					stranded: data.stranded,
					flash: flasher.phase,
				}),
			)
			.catch(() => {});
	}
}
