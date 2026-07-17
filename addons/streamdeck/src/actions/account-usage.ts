import {
	action,
	DialAction,
	KeyAction,
	SingletonAction,
	type DialDownEvent,
	type DidReceiveSettingsEvent,
	type KeyDownEvent,
	type SendToPluginEvent,
	type TouchTapEvent,
	type WillAppearEvent,
	type WillDisappearEvent,
} from "@elgato/streamdeck";
import streamDeck from "@elgato/streamdeck";

import { flowbee } from "../flowbee/service";
import type { AccountUsage } from "../flowbee/types";
import { openUrl } from "../macos";
import { accountKey, accountStatusColor, noteKey, type AccountStatus } from "../render";

type Settings = {
	/** account_id to pin; "" = auto-pick by key column. */
	account?: string;
};

type View = {
	action: KeyAction<Settings> | DialAction<Settings>;
	column: number;
	settings: Settings;
};

const STALE_MS = 24 * 60 * 60 * 1000;

/** "claude:pearl@swh.me" → "pearl" — the shortest label that stays unambiguous on a key. */
function shortLabel(accountId: string): string {
	const afterFamily = accountId.includes(":") ? accountId.slice(accountId.indexOf(":") + 1) : accountId;
	return afterFamily.includes("@") ? afterFamily.slice(0, afterFamily.indexOf("@")) : afterFamily;
}

export function usageStatus(a: AccountUsage): AccountStatus {
	if (!a.reported_at) return "never";
	// staleness FIRST: a quiet box pins a frozen high-water gauge (429 included)
	// forever — Flowbee's house rule is never to alarm on a >24h-old report.
	if (Date.now() - Date.parse(a.reported_at) > STALE_MS) return "stale";
	if (a.rate_limited || a.at_ceiling) return "gated";
	if (a.usage_pct >= 75) return "warn";
	return "ok";
}

@action({ UUID: "com.samhotchkiss.flowbee.account-usage" })
export class AccountUsageAction extends SingletonAction<Settings> {
	private views = new Map<string, View>();
	private unsubscribe?: () => void;

	override onWillAppear(ev: WillAppearEvent<Settings>): void {
		const coords = "coordinates" in ev.payload ? ev.payload.coordinates : undefined;
		this.views.set(ev.action.id, {
			action: ev.action,
			column: coords?.column ?? 0,
			settings: ev.payload.settings,
		});
		if (!this.unsubscribe) {
			this.unsubscribe = flowbee.subscribe("fleet", () => this.renderAll());
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

	override onKeyDown(ev: KeyDownEvent<Settings>): Promise<void> {
		return this.openFleet(() => ev.action.showAlert());
	}

	override onDialDown(ev: DialDownEvent<Settings>): Promise<void> {
		return this.openFleet(() => ev.action.showAlert());
	}

	override onTouchTap(ev: TouchTapEvent<Settings>): Promise<void> {
		return this.openFleet(() => ev.action.showAlert());
	}

	/** Datasource for the property inspector's account picker. */
	override onSendToPlugin(ev: SendToPluginEvent<{ event?: string }, Settings>): void {
		if (ev.payload?.event !== "getAccounts") return;
		const accounts = flowbee.state<AccountUsage[]>("fleet").data ?? [];
		void streamDeck.ui
			.sendToPropertyInspector({
				event: "getAccounts",
				items: [
					{ value: "", label: "Auto (by key column)" },
					...accounts.map((a) => ({ value: a.account_id, label: `${a.account_id} (${a.usage_pct}%)` })),
				],
			})
			.catch(() => {});
	}

	private async openFleet(alert: () => Promise<void>): Promise<void> {
		try {
			await openUrl(`${flowbee.api.baseUrl}/fleet`);
		} catch {
			await alert().catch(() => {});
		}
	}

	private renderAll(): void {
		for (const view of this.views.values()) this.render(view);
	}

	private render(view: View): void {
		const { data, error } = flowbee.state<AccountUsage[]>("fleet");
		const set = (image: string) => void view.action.setImage(image).catch(() => {});

		if (!data) {
			if (view.action.isDial()) {
				void view.action
					.setFeedback({ title: "Flowbee", value: error ? "offline" : "…", indicator: { value: 0 } })
					.catch(() => {});
			} else {
				set(noteKey("FLOWBEE", error ? "no server" : "loading…"));
			}
			return;
		}
		const account = view.settings.account
			? data.find((a) => a.account_id === view.settings.account)
			: data[view.column];
		if (!account) {
			if (view.action.isDial()) {
				void view.action.setFeedback({ title: "—", value: "no account", indicator: { value: 0 } }).catch(() => {});
			} else {
				set(noteKey("—", view.settings.account ? "gone?" : `no account #${view.column + 1}`));
			}
			return;
		}
		const status = usageStatus(account);
		if (view.action.isDial()) {
			void view.action
				.setFeedback({
					title: shortLabel(account.account_id),
					value: status === "never" ? "–" : `${Math.round(account.usage_pct)}%`,
					indicator: { value: Math.max(0, Math.min(account.usage_pct, 100)), bar_fill_c: accountStatusColor(status) },
				})
				.catch(() => {});
			return;
		}
		set(
			accountKey({
				label: shortLabel(account.account_id),
				family: account.model_family,
				pct: account.usage_pct,
				ceiling: account.ceiling_pct,
				status,
			}),
		);
	}
}
