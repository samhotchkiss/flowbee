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
import type { AccountUsage, LimitWindow } from "../flowbee/types";
import { openUrl } from "../macos";
import {
	BG_HOT_PCT,
	BG_WARN_PCT,
	RING_HUES,
	accountKey,
	accountStatusColor,
	noteKey,
	type AccountStatus,
	type RingSpec,
} from "../render";

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

const KIND_ORDER: LimitWindow["kind"][] = ["session", "weekly_all", "weekly_scoped"];

function windowTag(w: LimitWindow): string {
	if (w.kind === "session") return "5h";
	if (w.kind === "weekly_all") return "wk";
	return w.scope || "model";
}

export type AccountRings = {
	rings: RingSpec[];
	bindingPct: number;
	bindingTag: string;
	/** what drives the background tint: max of session/weekly (user spec), not the scoped ring. */
	bgDriverPct: number;
};

/**
 * Rings outer → inner: session (5h) / weekly / model-scoped ("Fable"), one per
 * window the provider actually reports — Codex draws a single weekly ring
 * until its session limit returns. Pre-digest wire (no windows[]) falls back
 * to one generic ring off usage_pct.
 */
export function accountRings(a: AccountUsage): AccountRings {
	const windows = a.windows ?? [];
	if (windows.length === 0) {
		const gatedNow = a.rate_limited || a.at_ceiling;
		return {
			rings: [{ pct: a.usage_pct, hue: RING_HUES.used, tag: "used", alarm: gatedNow }],
			bindingPct: a.usage_pct,
			bindingTag: "used",
			bgDriverPct: a.usage_pct,
		};
	}
	const rings: RingSpec[] = [];
	let binding: { pct: number; tag: string } | undefined;
	let bgDriverPct = 0;
	for (const kind of KIND_ORDER) {
		const ofKind = windows.filter((w) => w.kind === kind);
		if (ofKind.length === 0) continue;
		const worst = ofKind.reduce((a, b) => (b.percent > a.percent ? b : a));
		const tag = windowTag(worst);
		rings.push({
			pct: worst.percent,
			hue: RING_HUES[kind],
			tag,
			alarm: worst.percent >= 100 || worst.severity === "critical",
		});
		if (!binding || worst.percent > binding.pct) binding = { pct: worst.percent, tag };
		if (kind !== "weekly_scoped") bgDriverPct = Math.max(bgDriverPct, worst.percent);
	}
	return {
		rings,
		bindingPct: binding?.pct ?? 0,
		bindingTag: binding?.tag ?? "used",
		bgDriverPct,
	};
}

export function bgTier(bgDriverPct: number): "none" | "warn" | "hot" {
	if (bgDriverPct >= BG_HOT_PCT) return "hot";
	if (bgDriverPct >= BG_WARN_PCT) return "warn";
	return "none";
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
		const { rings, bindingPct, bindingTag, bgDriverPct } = accountRings(account);
		if (view.action.isDial()) {
			void view.action
				.setFeedback({
					title: shortLabel(account.account_id),
					value: status === "never" ? "–" : `${bindingTag} ${Math.round(bindingPct)}%`,
					indicator: { value: Math.max(0, Math.min(bindingPct, 100)), bar_fill_c: accountStatusColor(status) },
				})
				.catch(() => {});
			return;
		}
		set(
			accountKey({
				label: shortLabel(account.account_id),
				family: account.model_family,
				rings,
				bindingPct,
				bindingTag,
				bgTier: status === "stale" || status === "never" ? "none" : bgTier(bgDriverPct),
				stale: status === "stale",
				gated: status === "gated",
				never: status === "never",
			}),
		);
	}
}
