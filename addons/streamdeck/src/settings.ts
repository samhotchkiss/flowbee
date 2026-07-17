/**
 * Global settings shared by every Flowbee action (stored via Stream Deck's
 * secure global-settings store; edited from any action's property inspector).
 */
export type GlobalSettings = {
	/** Flowbee control-plane base URL. */
	baseUrl?: string;
	/** Optional bearer token (`flowbee token --identity …`) for the authed surface off-loopback. */
	token?: string;
	/** tmux session name of the "master" session (the operator's planning agent). */
	masterSession?: string;
	/** Terminal app that hosts the tmux clients. */
	terminalApp?: "iTerm" | "Terminal";
	/** Poll interval for the read endpoints, seconds. */
	pollSeconds?: number;
};

export const DEFAULTS = {
	baseUrl: "http://127.0.0.1:7070",
	terminalApp: "iTerm" as const,
	pollSeconds: 5,
};

export function baseUrl(gs: GlobalSettings): string {
	const raw = (gs.baseUrl ?? "").trim() || DEFAULTS.baseUrl;
	return raw.replace(/\/+$/, "");
}

export function pollMs(gs: GlobalSettings): number {
	const s = Number(gs.pollSeconds);
	return (Number.isFinite(s) && s >= 2 ? s : DEFAULTS.pollSeconds) * 1000;
}
