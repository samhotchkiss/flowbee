import { execFile } from "node:child_process";
import { promisify } from "node:util";

const exec = promisify(execFile);

/** Open a URL in the default browser. */
export async function openUrl(url: string): Promise<void> {
	if (!/^https?:\/\//.test(url)) throw new Error(`refusing to open non-http URL: ${url}`);
	await exec("/usr/bin/open", [url], { timeout: 5_000 });
}
