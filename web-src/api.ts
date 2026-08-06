// Typed fetch wrappers over /api/v1. Every failure throws with the server's own
// message; nothing here turns an error into an empty list.

export interface Timing {
	queued_for: number;
	setup_for: number;
	execute_for: number;
	total_for: number;
}

export interface Cancel {
	actor: string;
	sentence: string;
	triggered_by?: string;
}

export interface Run {
	id: number;
	name: string;
	workflow_name: string;
	workflow_path: string;
	repository: string;
	run_number: number;
	run_attempt: number;
	attempt: number;
	event: string;
	status: string;
	conclusion?: string;
	head_sha: string;
	head_branch: string;
	base_branch?: string;
	actor: string;
	is_fork_pr: boolean;
	failure_class: string;
	classification_reason?: string;
	cancel?: Cancel;
	timing: Timing;
	created_at: string;
	started_at?: string;
	completed_at?: string;
}

export interface Job {
	id: number;
	run_id: number;
	key: string;
	name: string;
	matrix_key?: string;
	needs: string[];
	labels: string[];
	attempt: number;
	max_attempts: number;
	requeue_count: number;
	infra_retry_count: number;
	status: string;
	conclusion?: string;
	failure_class: string;
	classification_reason?: string;
	cancel?: Cancel;
	timing: Timing;
	runner_id?: string;
	concurrency_group?: string;
	environment?: string;
	awaiting_approval: boolean;
	created_at: string;
	queued_at?: string;
	started_at?: string;
	completed_at?: string;
}

export interface Step {
	id: number;
	number: number;
	name: string;
	status: string;
	conclusion?: string;
	failure_class: string;
	exit_code: number;
	attempt: number;
	started_at?: string;
	completed_at?: string;
	duration: number;
	log_start: number;
	log_end: number;
}

export interface Annotation {
	path: string;
	start_line: number;
	end_line: number;
	annotation_level: string;
	message: string;
	title?: string;
}

export interface AuditEvent {
	id: number;
	kind: string;
	message: string;
	at: string;
}

export interface JobDetail extends Job {
	repository?: string;
	workflow_name?: string;
	run_number?: number;
	head_branch?: string;
	head_sha?: string;
	steps: Step[];
	annotations: Annotation[];
	events: AuditEvent[];
	classification_log?: string[];
}

export interface GraphNode {
	id: number;
	key: string;
	name: string;
	status: string;
	conclusion?: string;
	failure_class: string;
	needs: string[];
	depth: number;
}

export interface RunDetail extends Run {
	jobs: Job[];
	graph: { nodes: GraphNode[]; edges: { from: string; to: string }[] };
	attempts: { attempt: number; current: boolean }[];
	events: AuditEvent[];
}

export interface RunList {
	total_count: number;
	page: number;
	per_page: number;
	workflow_runs: Run[];
}

export interface LogLineWire {
	seq: number;
	ts: string;
	step: number;
	stream: string;
	text: string;
	group?: string;
}

export interface LogPage {
	job_id: number;
	attempt: number;
	from_seq: number;
	next_seq: number;
	count: number;
	lines: LogLineWire[];
}

export interface Runner {
	id: string;
	name: string;
	labels: string[];
	group?: string;
	state: string;
	current_job_id?: number;
	capacity: number;
	version: string;
	os: string;
	arch: string;
	last_heartbeat: string;
	heartbeat_age: number;
}

export interface RunnerList {
	total_count: number;
	online: number;
	busy: number;
	idle: number;
	offline: number;
	capacity: number;
	runners: Runner[];
	at: string;
}

export interface QueueStats {
	depth: number;
	depth_by_label: Record<string, number>;
	oldest_waiting: number;
	oldest_job_id?: number;
	runners_by_label: Record<string, number>;
	idle_by_label: Record<string, number>;
	starved_labels: string[];
	at: string;
}

export interface QueueSample {
	at: string;
	depth: number;
	depth_by_label?: Record<string, number>;
	busy: number;
	idle: number;
}

export interface QueueHistory {
	since: string;
	count: number;
	samples: QueueSample[];
}

export interface Artifact {
	id: number;
	run_id: number;
	job_id?: number;
	name: string;
	size_bytes: number;
	digest: string;
	finalized: boolean;
	created_at: string;
	expires_at: string;
	expired: boolean;
	download_url: string;
}

export interface ArtifactList {
	total_count: number;
	size_bytes: number;
	artifacts: Artifact[];
}

export interface CacheEntry {
	id: number;
	key: string;
	version?: string;
	ref?: string;
	size_bytes: number;
	created_at: string;
	last_accessed: string;
	finalized: boolean;
}

export interface CacheKeyStats {
	key: string;
	hits: number;
	misses: number;
	stores: number;
	evictions: number;
	hit_rate: number;
}

export interface CacheView {
	repository: string;
	usage_bytes: number;
	total_count: number;
	entries: CacheEntry[];
	entries_source: string;
	entries_complete: boolean;
	warning?: string;
	stats: { hits: number; misses: number; stores: number; evictions: number; hit_rate: number; events_considered: number };
	by_key: CacheKeyStats[];
	events: { key: string; kind: string; reason?: string; matched_on?: string; at: string }[];
}

export interface Health {
	status: string;
	store_durable: boolean;
	subsystems: { name: string; state: string; detail?: string }[];
	at: string;
}

/** An API failure carrying the server's own status and message. */
export class ApiError extends Error {
	readonly status: number;
	constructor(status: number, message: string) {
		super(message);
		this.status = status;
		this.name = "ApiError";
	}
}

/**
 * Called when the server rejects the session, so a cookie that expired
 * mid-session puts the sign-in form up instead of painting every panel with an
 * unauthorized error the operator cannot act on.
 */
let unauthorizedHandler: (() => void) | null = null;

export function onUnauthorized(fn: () => void): void {
	unauthorizedHandler = fn;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	let resp: Response;
	try {
		resp = await fetch(path, { headers: { Accept: "application/json" }, ...init });
	} catch (err) {
		throw new ApiError(0, `could not reach ${path}: ${err instanceof Error ? err.message : String(err)}`);
	}
	const text = await resp.text();
	if (!resp.ok) {
		if (resp.status === 401) unauthorizedHandler?.();
		let message = text.trim() || resp.statusText;
		try {
			const body = JSON.parse(text) as { message?: string; error?: string };
			if (body.message) message = body.message;
		} catch {
			// A non-JSON error body is still the truth about what went wrong.
		}
		throw new ApiError(resp.status, message);
	}
	return JSON.parse(text) as T;
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
	runs: (filters: Record<string, string | number | undefined>) => request<RunList>(`/api/v1/runs${query(filters)}`),
	run: (id: number) => request<RunDetail>(`/api/v1/runs/${id}`),
	runJobs: (id: number) => request<{ total_count: number; jobs: Job[] }>(`/api/v1/runs/${id}/jobs`),
	job: (id: number, attempt?: number) => request<JobDetail>(`/api/v1/jobs/${id}${query({ attempt })}`),
	logs: (id: number, opts: { from_seq?: number; limit?: number; attempt?: number } = {}) =>
		request<LogPage>(`/api/v1/jobs/${id}/logs${query(opts)}`),
	rawLogUrl: (id: number, attempt?: number) => `/api/v1/jobs/${id}/logs/raw${query({ attempt })}`,
	streamUrl: (id: number, opts: { from_seq?: number; attempt?: number } = {}) =>
		`/api/v1/jobs/${id}/logs/stream${query(opts)}`,
	runners: () => request<RunnerList>("/api/v1/runners"),
	queue: () => request<QueueStats>("/api/v1/queue"),
	queueHistory: (since: string) => request<QueueHistory>(`/api/v1/queue/history${query({ since })}`),
	artifacts: (runID: number) => request<ArtifactList>(`/api/v1/runs/${runID}/artifacts`),
	cache: (owner: string, repo: string) => request<CacheView>(`/api/v1/repos/${owner}/${repo}/cache`),
	health: () => request<Health>("/healthz"),

	cancelRun: (id: number, reason: string) =>
		request<unknown>(`/api/v1/runs/${id}/cancel`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ reason }),
		}),
	cancelJob: (id: number, reason: string) =>
		request<unknown>(`/api/v1/jobs/${id}/cancel`, {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ reason }),
		}),
	rerunRun: (id: number) => request<unknown>(`/api/v1/runs/${id}/rerun`, { method: "POST" }),
	rerunFailed: (id: number) => request<unknown>(`/api/v1/runs/${id}/rerun-failed`, { method: "POST" }),
	rerunJob: (id: number) => request<unknown>(`/api/v1/jobs/${id}/rerun`, { method: "POST" }),
};
