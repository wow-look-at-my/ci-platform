// Status/conclusion to presentation tone. No DOM.
//
// The whole point: an infra failure and a config error are NOT the red a broken
// build gets. They are their own tones with their own icons, so an operator
// reading the run list never mistakes "the registry timed out" for "your tests
// failed".

export type Tone =
	| "success"
	| "failure"
	| "infra"
	| "config"
	| "cancelled"
	| "skipped"
	| "neutral"
	| "running"
	| "queued"
	| "waiting";

export function toneOf(status: string, conclusion?: string): Tone {
	if (status !== "completed") {
		if (status === "in_progress") return "running";
		if (status === "waiting") return "waiting";
		return "queued";
	}
	switch (conclusion) {
		case "success":
			return "success";
		case "failure":
		case "timed_out":
			return "failure";
		case "infra_failure":
			return "infra";
		case "config_error":
			return "config";
		case "cancelled":
			return "cancelled";
		case "skipped":
			return "skipped";
		default:
			return "neutral";
	}
}

/** The class the failure class alone implies, for a step or job row. */
export function toneOfClass(failureClass: string): Tone | null {
	if (failureClass === "infra") return "infra";
	if (failureClass === "config") return "config";
	if (failureClass === "user") return "failure";
	return null;
}

const LABELS: Record<Tone, string> = {
	success: "Success",
	failure: "Failed",
	infra: "Infrastructure failure",
	config: "Workflow config error",
	cancelled: "Cancelled",
	skipped: "Skipped",
	neutral: "Neutral",
	running: "Running",
	queued: "Queued",
	waiting: "Waiting",
};

// Distinct glyphs, not just colour: colour alone fails for a colour-blind
// operator and for a screenshot pasted into a ticket.
const ICONS: Record<Tone, string> = {
	success: "✔",
	failure: "✘",
	infra: "⚠",
	config: "⚙",
	cancelled: "⊘",
	skipped: "→",
	neutral: "○",
	running: "◔",
	queued: "◌",
	waiting: "⏸",
};

export function toneLabel(tone: Tone): string {
	return LABELS[tone];
}

export function toneIcon(tone: Tone): string {
	return ICONS[tone];
}

/** True for the two tones that mean "not your code". */
export function isPlatformFault(tone: Tone): boolean {
	return tone === "infra" || tone === "config";
}

/** The one-line explanation shown next to a non-user failure. */
export function toneExplanation(tone: Tone): string {
	if (tone === "infra") return "The platform or a dependency of it failed. This is not your code.";
	if (tone === "config") return "The workflow file is wrong. Retrying cannot help.";
	return "";
}
