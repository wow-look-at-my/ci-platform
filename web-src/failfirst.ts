// Failure-first navigation: which step to open and which line to scroll to.
// No DOM.
//
// Opening a failed run at the top of a 40,000-line log is the behaviour this
// platform exists to replace, so this chooser is a first-class, tested unit.

export interface StepLike {
	number: number;
	name: string;
	status: string;
	conclusion?: string;
	failure_class?: string;
	log_start: number;
	log_end: number;
}

export interface JobLike {
	id: number;
	name: string;
	status: string;
	conclusion?: string;
	failure_class?: string;
}

export interface LineLike {
	seq: number;
	text: string;
	stream?: string;
}

/** Conclusions that stop dependents and therefore deserve the focus. */
const FAILING = new Set(["failure", "timed_out", "infra_failure", "config_error", "action_required"]);

export function isFailing(conclusion?: string): boolean {
	return !!conclusion && FAILING.has(conclusion);
}

/** The first failing step, or null when nothing failed. */
export function chooseFailingStep(steps: StepLike[]): StepLike | null {
	for (const s of steps) {
		if (isFailing(s.conclusion)) return s;
	}
	return null;
}

/** The first failing job of a run, or null. */
export function chooseFailingJob<T extends JobLike>(jobs: T[]): T | null {
	for (const j of jobs) {
		if (isFailing(j.conclusion)) return j;
	}
	return null;
}

// Patterns an operator would scan for by eye. Deliberately broad: landing one
// line early is harmless, landing at the top of the log is the failure.
const ERROR_PATTERNS: RegExp[] = [
	/(^|[^a-z])(error|errors)\b/i,
	/\bERR!/,
	/\bfailed\b|\bfailure\b|\bFAIL\b/i,
	/^\s*panic:/,
	/\bfatal\b/i,
	/^Traceback \(most recent call last\)/,
	/\bAssertionError\b|\bexpected\b.*\bgot\b/i,
	/\bexit(ed)? (with )?(status |code )?[1-9]/i,
	/^\s*##\[error\]/,
	/^\s*::error/,
];

export function looksLikeError(text: string): boolean {
	return ERROR_PATTERNS.some((re) => re.test(text));
}

/**
 * The first error-looking line inside [fromSeq, toSeq]. stderr wins over stdout
 * at equal position only when nothing on stdout matched earlier.
 */
export function firstErrorSeq(lines: LineLike[], fromSeq: number, toSeq: number): number | null {
	let stderrFallback: number | null = null;
	for (const l of lines) {
		if (l.seq < fromSeq || (toSeq > 0 && l.seq > toSeq)) continue;
		if (looksLikeError(l.text)) return l.seq;
		if (stderrFallback === null && l.stream === "stderr" && l.text.trim() !== "") stderrFallback = l.seq;
	}
	return stderrFallback;
}

export interface Focus {
	/** The step to expand, or null when there is nothing to focus. */
	stepNumber: number | null;
	/** The line to scroll to and highlight, or null. */
	seq: number | null;
	/** A sentence naming why the page opened here. Shown to the operator. */
	reason: string;
}

const NO_FOCUS: Focus = { stepNumber: null, seq: null, reason: "" };

/**
 * Chooses where a job page opens. Lines may be empty (they usually are on the
 * first paint); the step's own log range is then the answer.
 */
export function chooseFocus(job: JobLike, steps: StepLike[], lines: LineLike[] = []): Focus {
	if (!isFailing(job.conclusion)) return NO_FOCUS;

	const step = chooseFailingStep(steps);
	if (!step) {
		// A job can fail before any step ran: an infra failure during setup, a
		// config error while loading the workflow. Say that instead of silently
		// opening at the top.
		return {
			stepNumber: null,
			seq: null,
			reason: `${job.name} failed before any step ran, so there is no failing step to open.`,
		};
	}

	const seq = firstErrorSeq(lines, step.log_start, step.log_end) ?? (step.log_start > 0 ? step.log_start : null);
	const where = seq === null ? "the start of its output" : `line ${seq}`;
	return {
		stepNumber: step.number,
		seq,
		reason: `Opened at step ${step.number} "${step.name}", ${where}.`,
	};
}

/** Parses "#L1234" / "#L1234-L1250" into a seq range. */
export function parseLineAnchor(hash: string): { from: number; to: number } | null {
	const raw = hash.startsWith("#") ? hash.slice(1) : hash;
	const m = /^L(\d+)(?:-L?(\d+))?$/.exec(raw);
	if (!m) return null;
	const from = parseInt(m[1], 10);
	const to = m[2] ? parseInt(m[2], 10) : from;
	return from <= to ? { from, to } : { from: to, to: from };
}

/** Renders a seq range back to an anchor. */
export function formatLineAnchor(from: number, to: number): string {
	return from === to ? `L${from}` : `L${from}-L${to}`;
}
