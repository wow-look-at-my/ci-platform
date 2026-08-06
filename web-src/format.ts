// Duration, size and staleness formatting. No DOM: these are the functions the
// tests pin down.

/** Human duration from seconds. Zero and negatives read as "0s". */
export function formatDuration(seconds: number): string {
	if (!isFinite(seconds) || seconds <= 0) return "0s";
	if (seconds < 1) return `${Math.round(seconds * 1000)}ms`;
	if (seconds < 10) return `${seconds.toFixed(1)}s`;
	if (seconds < 60) return `${Math.round(seconds)}s`;
	if (seconds < 3600) {
		const m = Math.floor(seconds / 60);
		const s = Math.round(seconds - m * 60);
		return s === 0 ? `${m}m` : `${m}m ${s}s`;
	}
	const h = Math.floor(seconds / 3600);
	const m = Math.round((seconds - h * 3600) / 60);
	return `${h}h ${m}m`;
}

/** Compact duration for dense tables: always one unit. */
export function formatDurationShort(seconds: number): string {
	if (!isFinite(seconds) || seconds <= 0) return "0s";
	if (seconds < 60) return `${Math.round(seconds)}s`;
	if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
	if (seconds < 86400) return `${Math.round(seconds / 3600)}h`;
	return `${Math.round(seconds / 86400)}d`;
}

/** Binary size. */
export function formatBytes(bytes: number): string {
	if (!isFinite(bytes) || bytes <= 0) return "0 B";
	const units = ["B", "KiB", "MiB", "GiB", "TiB"];
	let n = bytes;
	let i = 0;
	while (n >= 1024 && i < units.length - 1) {
		n /= 1024;
		i++;
	}
	return i === 0 ? `${Math.round(n)} B` : `${n.toFixed(1)} ${units[i]}`;
}

/** "3m ago" / "in 20s". `nowMs` is injected so this stays testable. */
export function formatRelative(iso: string | null | undefined, nowMs: number): string {
	if (!iso) return "never";
	const then = Date.parse(iso);
	if (isNaN(then)) return "unknown";
	const deltaSec = (nowMs - then) / 1000;
	const abs = formatDurationShort(Math.abs(deltaSec));
	if (Math.abs(deltaSec) < 1) return "just now";
	return deltaSec >= 0 ? `${abs} ago` : `in ${abs}`;
}

/** Absolute timestamp for a title attribute. */
export function formatTimestamp(iso: string | null | undefined): string {
	if (!iso) return "";
	const d = new Date(iso);
	return isNaN(d.getTime()) ? "" : d.toISOString().replace("T", " ").replace(/\.\d+Z$/, "Z");
}

export type Staleness = "fresh" | "stale" | "dead";

/**
 * Heartbeat staleness bands the fleet page colours by. A runner that has not
 * checked in for two minutes is presented as dead, not as "probably fine".
 */
export function heartbeatStaleness(ageSeconds: number): Staleness {
	if (ageSeconds < 30) return "fresh";
	if (ageSeconds < 120) return "stale";
	return "dead";
}

/** Short commit SHA, unchanged when it is already short. */
export function shortSha(sha: string): string {
	return sha.length > 7 ? sha.slice(0, 7) : sha;
}

/** Percentage with one decimal, for cache hit rates. */
export function formatPercent(fraction: number): string {
	if (!isFinite(fraction)) return "n/a";
	return `${(fraction * 100).toFixed(1)}%`;
}
