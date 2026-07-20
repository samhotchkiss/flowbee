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
	/**
	 * ssh hosts a key press may connect to (comma/space-separated). The goal-
	 * session registry supplies each remote session's `box` — without this
	 * allowlist, a hostile registration could open ssh to an attacker host on a
	 * keypress. Empty = remote sessions are blocked (local tmux unaffected).
	 */
	sshAllowedHosts?: string;
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

/** Throws unless `box` is empty (local) or on the operator's ssh allowlist. */
export function assertBoxAllowed(gs: GlobalSettings, box: string | undefined): void {
	if (!box) return;
	// hostnames are case-insensitive — don't false-block on a case mismatch.
	const allowed = (gs.sshAllowedHosts ?? "")
		.split(/[\s,]+/)
		.map((h) => h.trim().toLowerCase())
		.filter(Boolean);
	if (!allowed.includes(box.toLowerCase())) {
		throw new Error(
			`ssh to ${JSON.stringify(box)} blocked: not on the "SSH hosts allowed" list (set it in any key's settings)`,
		);
	}
}
