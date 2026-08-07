// The demo build's API client: it answers from a captured snapshot instead of
// a server.
//
// The snapshot is not written by hand. cmd/demofixtures seeds a real store,
// serves it with the real internal/api handlers, and records what they return,
// so the demo cannot show a shape the product does not produce. Anything the
// snapshot does not contain fails loudly here rather than rendering as empty.

import fixtures from "./fixtures.json";

const responses = fixtures as Record<string, unknown>;

/** An API failure carrying the server's own status and message. */
export class ApiError extends Error {
	readonly status: number;
	constructor(status: number, message: string) {
		super(message);
		this.status = status;
		this.name = "ApiError";
	}
}

// Kept so the demo build swaps cleanly for the real client. Nothing here can
// return 401: there is no session to expire.
export function onUnauthorized(_fn: () => void): void {}

function lookup<T>(path: string): Promise<T> {
	const body = responses[path];
	if (body === undefined) {
		return Promise.reject(
			new ApiError(404, `This static demo has no captured response for ${path}. It is a snapshot of a few runs, not a live instance.`),
		);
	}
	return Promise.resolve(structuredClone(body) as T);
}

/** Actions need a control plane, and a demo does not have one. */
function refuse(what: string): Promise<never> {
	return Promise.reject(
		new ApiError(
			403,
			`This is a static demo, so ${what} would have nothing to act on. Run the real thing to use it: see the README.`,
		),
	);
}

interface RunLike {
	status: string;
	conclusion?: string;
	head_branch: string;
	actor: string;
	event: string;
	workflow_name: string;
	head_sha: string;
}

interface RunListLike {
	total_count: number;
	page: number;
	per_page: number;
	workflow_runs: RunLike[];
}

// The runs page's filters are a real feature, so the demo applies them to the
// captured list rather than ignoring them and looking broken.
function filterRuns(list: RunListLike, filters: Record<string, string | number | undefined>): RunListLike {
	const want = (k: string) => String(filters[k] ?? "").trim();
	const q = want("q").toLowerCase();
	const runs = list.workflow_runs.filter((r) => {
		if (want("branch") && r.head_branch !== want("branch")) return false;
		if (want("actor") && r.actor !== want("actor")) return false;
		if (want("event") && r.event !== want("event")) return false;
		if (want("status") && r.status !== want("status")) return false;
		if (want("conclusion") && (r.conclusion ?? "") !== want("conclusion")) return false;
		if (want("workflow") && r.workflow_name !== want("workflow")) return false;
		if (q) {
			const haystack = `${r.workflow_name} ${r.head_branch} ${r.head_sha} ${r.actor}`.toLowerCase();
			if (!haystack.includes(q)) return false;
		}
		return true;
	});
	return { ...list, total_count: runs.length, workflow_runs: runs };
}

interface LogPageLike {
	lines: { text: string; ts: string; stream: string }[];
}

// The raw-log link is a real download in the demo: the bytes come from the
// captured log rather than from a URL that would 404.
function rawLogDataURL(id: number): string {
	const page = responses[`/api/v1/jobs/${id}/logs`] as LogPageLike | undefined;
	const text = (page?.lines ?? []).map((l) => `${l.ts} ${l.stream} ${l.text}`).join("\n");
	return `data:text/plain;charset=utf-8,${encodeURIComponent(text || "no log captured for this job")}`;
}

function query(params: Record<string, string | number | undefined>): string {
	const q = new URLSearchParams();
	for (const [k, v] of Object.entries(params)) {
		if (v !== undefined && v !== "") q.set(k, String(v));
	}
	const s = q.toString();
	return s ? `?${s}` : "";
}

export const api = {
	runs: async (filters: Record<string, string | number | undefined>) =>
		filterRuns(await lookup<RunListLike>("/api/v1/runs"), filters) as never,
	run: (id: number) => lookup<never>(`/api/v1/runs/${id}`),
	runJobs: (id: number) => lookup<never>(`/api/v1/runs/${id}/jobs`),
	job: (id: number, _attempt?: number) => lookup<never>(`/api/v1/jobs/${id}`),
	logs: (id: number, _opts: { from_seq?: number; limit?: number; attempt?: number } = {}) =>
		lookup<never>(`/api/v1/jobs/${id}/logs`),
	rawLogUrl: (id: number, _attempt?: number) => rawLogDataURL(id),
	// No stream: every job in the snapshot is finished, so the log view never
	// opens one. Returning a dead URL would be a page that says "reconnecting"
	// forever.
	streamUrl: (id: number, opts: { from_seq?: number; attempt?: number } = {}) =>
		`/api/v1/jobs/${id}/logs/stream${query(opts)}`,
	runners: () => lookup<never>("/api/v1/runners"),
	queue: () => lookup<never>("/api/v1/queue"),
	// The captured history is one window; the page's since= is ignored rather
	// than answered with an empty graph.
	queueHistory: (_since: string) =>
		lookup<never>(Object.keys(responses).find((k) => k.startsWith("/api/v1/queue/history")) ?? "/api/v1/queue/history"),
	artifacts: (runID: number) => lookup<never>(`/api/v1/runs/${runID}/artifacts`),
	cache: (owner: string, repo: string) => lookup<never>(`/api/v1/repos/${owner}/${repo}/cache`),
	health: () => lookup<never>("/healthz"),

	cancelRun: (_id: number, _reason: string) => refuse("cancelling a run"),
	cancelJob: (_id: number, _reason: string) => refuse("cancelling a job"),
	rerunRun: (_id: number) => refuse("re-running"),
	rerunFailed: (_id: number) => refuse("re-running the failed jobs"),
	rerunJob: (_id: number) => refuse("re-running a job"),
};
