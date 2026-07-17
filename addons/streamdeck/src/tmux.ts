import streamDeck from "@elgato/streamdeck";
import { execFile } from "node:child_process";
import { existsSync } from "node:fs";
import { promisify } from "node:util";

const exec = promisify(execFile);
const logger = streamDeck.logger.createScope("tmux");

/**
 * tmux + terminal integration: jump to a session's window in the terminal app,
 * and type prompts into a session. Mirrors the semantics of Flowbee's own
 * watchdog runner (internal/watchdog/runner.go): text and Enter are sent as two
 * separate send-keys calls so agent TUIs register the submission.
 */

// Stream Deck launches plugins with a minimal PATH, so resolve tmux explicitly.
const TMUX_CANDIDATES = ["/opt/homebrew/bin/tmux", "/usr/local/bin/tmux", "/usr/bin/tmux"];
let tmuxPath: string | undefined;

function tmux(): string {
	if (!tmuxPath) {
		tmuxPath = TMUX_CANDIDATES.find((p) => existsSync(p));
		if (!tmuxPath) throw new Error("tmux not found (looked in /opt/homebrew/bin, /usr/local/bin, /usr/bin)");
	}
	return tmuxPath;
}

/** Session names travel into tmux -t targets, ssh commands, and AppleScript — keep them boring. */
export function validSessionName(name: string): boolean {
	return /^[A-Za-z0-9._@-]+$/.test(name);
}

function assertName(name: string): void {
	if (!validSessionName(name)) throw new Error(`refusing unsafe tmux session name: ${JSON.stringify(name)}`);
}

/** Single-quote for a POSIX shell (remote ssh side). Matches runner.go shQuote. */
function shQuote(s: string): string {
	return `'${s.replaceAll("'", `'\\''`)}'`;
}

/** Escape a string literal for embedding in AppleScript source. */
function osaQuote(s: string): string {
	return `"${s.replaceAll("\\", "\\\\").replaceAll(`"`, `\\"`)}"`;
}

async function osascript(script: string): Promise<string> {
	const { stdout } = await exec("/usr/bin/osascript", ["-e", script], { timeout: 10_000 });
	return stdout.trim();
}

export type LocalTmuxSession = { name: string; attached: boolean };

/** All local tmux sessions ([] when the tmux server isn't running). */
export async function listLocalTmuxSessions(): Promise<LocalTmuxSession[]> {
	try {
		const { stdout } = await exec(
			tmux(),
			["list-sessions", "-F", "#{session_name}\t#{session_attached}"],
			{ timeout: 5_000 },
		);
		return stdout
			.split("\n")
			.filter(Boolean)
			.map((line) => {
				const [name, attached] = line.split("\t");
				return { name, attached: Number(attached) > 0 };
			});
	} catch {
		return []; // no tmux server (or no tmux) — not an error for our callers
	}
}

/** ttys of terminal panes attached to `session`, most recently active first. */
async function clientTtys(session: string): Promise<string[]> {
	try {
		const { stdout } = await exec(
			tmux(),
			["list-clients", "-F", "#{client_tty}\t#{session_name}\t#{client_activity}"],
			{ timeout: 5_000 },
		);
		return stdout
			.split("\n")
			.filter(Boolean)
			.map((l) => l.split("\t"))
			.filter(([, name]) => name === session)
			.sort((a, b) => Number(b[2]) - Number(a[2]))
			.map(([tty]) => tty);
	} catch {
		return [];
	}
}

type TerminalApp = "iTerm" | "Terminal";

/** AppleScript: bring the window/tab whose tty matches to the front. Returns "ok" or "notfound". */
function selectByTtyScript(app: TerminalApp, tty: string): string {
	if (app === "iTerm") {
		return `
tell application "iTerm"
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is ${osaQuote(tty)} then
					select w
					tell w to select t
					activate
					return "ok"
				end if
			end repeat
		end repeat
	end repeat
end tell
return "notfound"`;
	}
	return `
tell application "Terminal"
	repeat with w in windows
		repeat with t in tabs of w
			if tty of t is ${osaQuote(tty)} then
				set selected of t to true
				set index of w to 1
				activate
				return "ok"
			end if
		end repeat
	end repeat
end tell
return "notfound"`;
}

/** AppleScript: open a new terminal window running `command`. */
function openWindowScript(app: TerminalApp, command: string): string {
	if (app === "iTerm") {
		return `
tell application "iTerm"
	create window with default profile command ${osaQuote(command)}
	activate
end tell`;
	}
	return `
tell application "Terminal"
	do script ${osaQuote(command)}
	activate
end tell`;
}

export type FocusTarget = {
	tmuxName: string;
	/** ssh host when the session lives on another box ('' / undefined = local). */
	box?: string;
	terminalApp: TerminalApp;
};

/**
 * Jump to a tmux session's window: focus the terminal tab already attached to
 * it, or open a fresh window attaching to it (via ssh for a remote box).
 */
export async function focusSession({ tmuxName, box, terminalApp }: FocusTarget): Promise<void> {
	assertName(tmuxName);
	if (box) {
		if (!validSessionName(box)) throw new Error(`refusing unsafe ssh host: ${JSON.stringify(box)}`);
		await osascript(openWindowScript(terminalApp, `ssh -t ${box} tmux attach -t ${shQuote(tmuxName)}`));
		return;
	}
	const sessions = await listLocalTmuxSessions();
	if (!sessions.some((s) => s.name === tmuxName)) {
		throw new Error(`no local tmux session named ${JSON.stringify(tmuxName)}`);
	}
	for (const tty of await clientTtys(tmuxName)) {
		if ((await osascript(selectByTtyScript(terminalApp, tty))) === "ok") {
			logger.debug(`focused ${tmuxName} via ${tty}`);
			return;
		}
	}
	// session exists but nothing is attached (or its tab is gone) — attach fresh.
	await osascript(openWindowScript(terminalApp, `tmux attach -t ${shQuote(tmuxName)}`));
}

export type PromptTarget = {
	tmuxName: string;
	box?: string;
	text: string;
	/** press Enter after the text (default true). */
	submit?: boolean;
};

/**
 * Type `text` into the session. Text first (`send-keys -l --`), a beat, then a
 * separate Enter — agent composers (claude/codex TUIs) drop same-burst Enters.
 */
export async function sendPrompt({ tmuxName, box, text, submit = true }: PromptTarget): Promise<void> {
	assertName(tmuxName);
	const line = text.replaceAll("\r", " ").replaceAll("\n", " ").trim();
	if (!line) throw new Error("empty prompt");
	if (box) {
		if (!validSessionName(box)) throw new Error(`refusing unsafe ssh host: ${JSON.stringify(box)}`);
		const target = shQuote(tmuxName);
		await exec("/usr/bin/ssh", [box, `tmux send-keys -t ${target} -l -- ${shQuote(line)}`], { timeout: 15_000 });
		if (submit) {
			await new Promise((r) => setTimeout(r, 300));
			await exec("/usr/bin/ssh", [box, `tmux send-keys -t ${target} Enter`], { timeout: 15_000 });
		}
		return;
	}
	await exec(tmux(), ["send-keys", "-t", tmuxName, "-l", "--", line], { timeout: 5_000 });
	if (submit) {
		await new Promise((r) => setTimeout(r, 300));
		await exec(tmux(), ["send-keys", "-t", tmuxName, "Enter"], { timeout: 5_000 });
	}
}
