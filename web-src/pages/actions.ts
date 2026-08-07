// Cancel and re-run controls shared by the run and job pages.

import { el } from "../dom.js";

export function toast(message: string, isError = false): void {
	const node = el("div", { class: `toast${isError ? " tone-failure" : ""}`, role: "status" }, message);
	document.body.appendChild(node);
	setTimeout(() => node.remove(), 6000);
}

/**
 * Cancelling always asks for a reason. The API rejects a cancel without one,
 * and the reason is what the next person reads when they ask why this stopped.
 */
export async function confirmCancel(what: string, send: (reason: string) => Promise<unknown>): Promise<void> {
	const reason = prompt(`Why are you cancelling this ${what}? The reason is recorded and shown to whoever asks.`);
	if (reason === null) return;
	if (reason.trim() === "") {
		toast("Cancel needs a reason. Nothing was cancelled.", true);
		return;
	}
	try {
		await send(reason.trim());
		toast(`Cancel requested: ${reason.trim()}`);
	} catch (err) {
		toast(err instanceof Error ? err.message : String(err), true);
	}
}

/** Runs an action, reporting success or the server's own error text. */
export async function runAction(label: string, fn: () => Promise<unknown>): Promise<void> {
	try {
		await fn();
		toast(`${label} requested.`);
	} catch (err) {
		toast(err instanceof Error ? err.message : String(err), true);
	}
}
