/**
 * One shared 500ms flasher for every key that needs to pulse (blocked
 * sessions, attention, stranded fleet). Runs only while someone is alarmed.
 */
class FlashCoordinator {
	private subs = new Map<string, () => void>();
	private timer?: NodeJS.Timeout;
	phase = false;

	register(id: string, render: () => void): void {
		this.subs.set(id, render);
		if (!this.timer) {
			this.timer = setInterval(() => {
				this.phase = !this.phase;
				for (const render of this.subs.values()) render();
			}, 500);
		}
	}

	unregister(id: string): void {
		this.subs.delete(id);
		if (this.subs.size === 0 && this.timer) {
			clearInterval(this.timer);
			this.timer = undefined;
			this.phase = false;
		}
	}
}

export const flasher = new FlashCoordinator();
