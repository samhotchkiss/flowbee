/**
 * SVG key renderers (144×144, returned as data: URIs for setImage).
 * One dark visual system across all Flowbee keys; red flashes alternate the
 * `flash` phase supplied by the shared FlashCoordinator.
 */

export const COLORS = {
	bg: "#17181c",
	panel: "#23252b",
	text: "#f4f4f5",
	dim: "#9ca3af",
	faint: "#565b66",
	green: "#34d399",
	blue: "#60a5fa",
	amber: "#fbbf24",
	red: "#ef4444",
	redDark: "#7f1d1d",
	honey: "#f5b93c",
	claude: "#d97757",
	codex: "#10a37f",
	otherFamily: "#8b8ff0",
};

function esc(s: string): string {
	return s.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
}

function truncate(s: string, max: number): string {
	// code-point-wise: a .slice() through a surrogate pair yields a lone
	// surrogate, which makes encodeURIComponent throw (URIError) mid-render.
	const cps = Array.from(s);
	return cps.length <= max ? s : `${cps.slice(0, max - 1).join("")}…`;
}

function svg(body: string, bg = COLORS.bg): string {
	const doc = `<svg xmlns="http://www.w3.org/2000/svg" width="144" height="144" viewBox="0 0 144 144"><rect width="144" height="144" rx="16" fill="${bg}"/><g font-family="-apple-system, 'Helvetica Neue', Helvetica, Arial, sans-serif">${body}</g></svg>`;
	return `data:image/svg+xml,${encodeURIComponent(doc)}`;
}

export function familyColor(family: string): string {
	if (family.includes("claude") || family.includes("opus") || family.includes("sonnet")) return COLORS.claude;
	if (family.includes("codex") || family.includes("gpt")) return COLORS.codex;
	return COLORS.otherFamily;
}

/** A key with a short caption — used for "no server", "no account", etc. */
export function noteKey(title: string, note: string, tone: string = COLORS.faint): string {
	return svg(
		`<text x="72" y="66" font-size="22" font-weight="700" fill="${tone}" text-anchor="middle">${esc(truncate(title, 11))}</text>
		 <text x="72" y="92" font-size="15" fill="${COLORS.dim}" text-anchor="middle">${esc(truncate(note, 15))}</text>`,
	);
}

export type AccountStatus = "ok" | "warn" | "gated" | "stale" | "never";

export function accountStatusColor(status: AccountStatus): string {
	switch (status) {
		case "ok":
			return COLORS.green;
		case "warn":
			return COLORS.amber;
		case "gated":
			return COLORS.red;
		default:
			return COLORS.faint;
	}
}

/** Ring gauge for one account's usage level. */
export function accountKey(opts: {
	label: string;
	family: string;
	pct: number;
	ceiling: number;
	status: AccountStatus;
}): string {
	const { label, family, pct, ceiling, status } = opts;
	const color = accountStatusColor(status);
	const shown = Math.max(0, Math.min(pct, 100));
	const r = 44;
	const c = 2 * Math.PI * r;
	const frac = shown / 100;
	// ceiling tick position on the ring (same -90° rotation as the arc).
	const ceilAngle = ((Math.min(ceiling, 100) / 100) * 360 - 90) * (Math.PI / 180);
	const tick =
		ceiling > 0 && ceiling <= 100
			? `<line x1="${72 + (r - 8) * Math.cos(ceilAngle)}" y1="${62 + (r - 8) * Math.sin(ceilAngle)}" x2="${72 + (r + 8) * Math.cos(ceilAngle)}" y2="${62 + (r + 8) * Math.sin(ceilAngle)}" stroke="${COLORS.dim}" stroke-width="3"/>`
			: "";
	const center =
		status === "never"
			? `<text x="72" y="70" font-size="26" font-weight="700" fill="${COLORS.faint}" text-anchor="middle">–</text>`
			: `<text x="72" y="72" font-size="30" font-weight="800" fill="${status === "stale" ? COLORS.faint : COLORS.text}" text-anchor="middle">${Math.round(pct)}<tspan font-size="16" font-weight="600">%</tspan></text>`;
	const sub =
		status === "gated"
			? `<text x="72" y="90" font-size="12" font-weight="700" fill="${COLORS.red}" text-anchor="middle">AT LIMIT</text>`
			: status === "stale"
				? `<text x="72" y="90" font-size="12" fill="${COLORS.faint}" text-anchor="middle">stale</text>`
				: "";
	return svg(
		`<circle cx="72" cy="62" r="${r}" fill="none" stroke="${COLORS.panel}" stroke-width="10"/>
		 <circle cx="72" cy="62" r="${r}" fill="none" stroke="${color}" stroke-width="10" stroke-linecap="round"
		   stroke-dasharray="${(frac * c).toFixed(1)} ${c.toFixed(1)}" transform="rotate(-90 72 62)" opacity="${status === "stale" ? 0.35 : 1}"/>
		 ${tick}${center}${sub}
		 <circle cx="26" cy="126" r="5" fill="${familyColor(family)}"/>
		 <text x="38" y="131" font-size="15" font-weight="600" fill="${COLORS.dim}">${esc(truncate(label, 11))}</text>`,
	);
}

const SESSION_TONE: Record<string, { color: string; label: string }> = {
	pursuing: { color: COLORS.blue, label: "PURSUING" },
	working: { color: COLORS.green, label: "WORKING" },
	blocked: { color: COLORS.red, label: "BLOCKED" },
	achieved: { color: COLORS.green, label: "DONE ✓" },
	unknown: { color: COLORS.faint, label: "UNKNOWN" },
	unreachable: { color: COLORS.red, label: "UNREACHABLE" },
	unwatched: { color: COLORS.faint, label: "UNWATCHED" },
	// healthy-but-parked (epic-lane taxonomy): resume pending, never alarm-red.
	goal_paused: { color: COLORS.amber, label: "PARKED" },
};

/** Goal-session / epic key. `flash` alternates the blocked pulse. */
export function sessionKey(opts: {
	name: string;
	state: string;
	detail?: string;
	footer?: string;
	remote?: boolean;
	flash?: boolean;
}): string {
	const tone = SESSION_TONE[opts.state] ?? { color: COLORS.otherFamily, label: opts.state.toUpperCase() };
	const alarmed = (opts.state === "blocked" || opts.state === "unreachable") && opts.flash;
	const bg = alarmed ? COLORS.redDark : COLORS.bg;
	const name = truncate(opts.name, 12);
	const nameSize = name.length > 9 ? 19 : 23;
	const footer = opts.footer ? truncate(opts.footer, 16) : "";
	return svg(
		`<rect x="10" y="12" width="124" height="26" rx="8" fill="${tone.color}" opacity="${alarmed ? 1 : 0.22}"/>
		 <text x="72" y="30" font-size="14" font-weight="800" letter-spacing="1" fill="${alarmed ? "#fff" : tone.color}" text-anchor="middle">${esc(truncate(tone.label, 12))}</text>
		 <text x="72" y="82" font-size="${nameSize}" font-weight="700" fill="${COLORS.text}" text-anchor="middle">${esc(name)}</text>
		 ${opts.remote ? `<text x="72" y="102" font-size="12" fill="${COLORS.dim}" text-anchor="middle">⇅ remote</text>` : ""}
		 <text x="72" y="126" font-size="14" fill="${COLORS.dim}" text-anchor="middle">${esc(footer)}</text>`,
		bg,
	);
}

/** Pause/resume toggle key. */
export function pauseKey(opts: { paused: boolean; scope?: string; unknown?: boolean }): string {
	if (opts.unknown) return noteKey("PAUSE?", "state unknown");
	const scope = opts.scope ? truncate(opts.scope, 14) : "all repos";
	if (opts.paused) {
		return svg(
			`<rect x="46" y="34" width="16" height="48" rx="4" fill="${COLORS.amber}"/>
			 <rect x="80" y="34" width="16" height="48" rx="4" fill="${COLORS.amber}"/>
			 <text x="72" y="110" font-size="18" font-weight="800" fill="${COLORS.amber}" text-anchor="middle">PAUSED</text>
			 <text x="72" y="130" font-size="13" fill="${COLORS.dim}" text-anchor="middle">${esc(scope)}</text>`,
		);
	}
	return svg(
		`<path d="M52 32 L100 58 L52 84 Z" fill="${COLORS.green}"/>
		 <text x="72" y="110" font-size="18" font-weight="800" fill="${COLORS.green}" text-anchor="middle">RUNNING</text>
		 <text x="72" y="130" font-size="13" fill="${COLORS.dim}" text-anchor="middle">${esc(scope)}</text>`,
	);
}

/** Attention key: how many jobs wait on a human. */
export function attentionKey(opts: { total: number; breakdown: string; flash?: boolean }): string {
	if (opts.total === 0) {
		return svg(
			`<path d="M46 74 L64 92 L100 50" fill="none" stroke="${COLORS.green}" stroke-width="10" stroke-linecap="round" stroke-linejoin="round"/>
			 <text x="72" y="126" font-size="14" fill="${COLORS.dim}" text-anchor="middle">all clear</text>`,
		);
	}
	const alarmed = opts.flash;
	return svg(
		`<text x="72" y="82" font-size="56" font-weight="800" fill="${alarmed ? "#fff" : COLORS.red}" text-anchor="middle">${opts.total}</text>
		 <text x="72" y="108" font-size="14" font-weight="700" fill="${alarmed ? "#fff" : COLORS.text}" text-anchor="middle">NEEDS YOU</text>
		 <text x="72" y="128" font-size="12" fill="${alarmed ? "#fecaca" : COLORS.dim}" text-anchor="middle">${esc(truncate(opts.breakdown, 18))}</text>`,
		alarmed ? COLORS.redDark : COLORS.bg,
	);
}

/** Fleet-health key. */
export function fleetKey(opts: {
	live: number;
	stale: number;
	waiting: number;
	stranded: boolean;
	flash?: boolean;
}): string {
	if (opts.stranded) {
		return svg(
			`<text x="72" y="66" font-size="21" font-weight="800" fill="#fff" text-anchor="middle">STRANDED</text>
			 <text x="72" y="94" font-size="14" fill="#fecaca" text-anchor="middle">${opts.waiting} waiting · 0 live</text>`,
			opts.flash ? COLORS.red : COLORS.redDark,
		);
	}
	const staleRow = opts.stale > 0 ? `<circle cx="34" cy="72" r="6" fill="${COLORS.amber}"/><text x="48" y="78" font-size="17" fill="${COLORS.text}">${opts.stale} stale</text>` : "";
	return svg(
		`<circle cx="34" cy="44" r="6" fill="${opts.live > 0 ? COLORS.green : COLORS.faint}"/>
		 <text x="48" y="50" font-size="17" fill="${COLORS.text}">${opts.live} live</text>
		 ${staleRow}
		 <text x="48" y="${opts.stale > 0 ? 106 : 78}" font-size="17" fill="${opts.waiting > 0 ? COLORS.amber : COLORS.dim}">${opts.waiting} waiting</text>
		 <text x="72" y="132" font-size="12" fill="${COLORS.faint}" text-anchor="middle">FLEET</text>`,
	);
}

/** "Go to master" key. */
export function masterKey(opts: { name?: string; state?: string }): string {
	const tone = opts.state ? (SESSION_TONE[opts.state]?.color ?? COLORS.otherFamily) : COLORS.faint;
	return svg(
		`<rect x="30" y="30" width="84" height="52" rx="8" fill="${COLORS.panel}" stroke="${COLORS.honey}" stroke-width="3"/>
		 <text x="72" y="63" font-size="20" font-weight="800" fill="${COLORS.honey}" text-anchor="middle">&gt;_</text>
		 <circle cx="108" cy="36" r="7" fill="${tone}"/>
		 <text x="72" y="106" font-size="16" font-weight="700" fill="${COLORS.text}" text-anchor="middle">${esc(truncate(opts.name ?? "master", 12))}</text>
		 <text x="72" y="128" font-size="12" fill="${COLORS.dim}" text-anchor="middle">GO TO MASTER</text>`,
	);
}

/** "Prompt master" key. */
export function promptKey(opts: { label: string }): string {
	return svg(
		`<path d="M34 34 h76 a8 8 0 0 1 8 8 v34 a8 8 0 0 1 -8 8 h-44 l-18 16 v-16 h-14 a8 8 0 0 1 -8 -8 v-34 a8 8 0 0 1 8 -8 Z" fill="${COLORS.panel}" stroke="${COLORS.blue}" stroke-width="3"/>
		 <text x="72" y="63" font-size="20" font-weight="800" fill="${COLORS.blue}" text-anchor="middle">?</text>
		 <text x="72" y="122" font-size="15" font-weight="600" fill="${COLORS.text}" text-anchor="middle">${esc(truncate(opts.label, 14))}</text>`,
	);
}
