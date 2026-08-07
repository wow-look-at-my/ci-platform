import {
	chooseFailingJob,
	chooseFailingStep,
	chooseFocus,
	firstErrorSeq,
	formatLineAnchor,
	isFailing,
	looksLikeError,
	parseLineAnchor,
	type StepLike,
} from "./failfirst.js";
import { assert, assertDeepEqual, assertEqual, test } from "./testing.js";

function step(number: number, name: string, conclusion: string, log_start: number, log_end: number): StepLike {
	return { number, name, status: "completed", conclusion, log_start, log_end };
}

const steps = [
	step(1, "Set up job", "success", 1, 10),
	step(2, "Checkout", "success", 11, 20),
	step(3, "Run tests", "failure", 21, 40),
	step(4, "Upload", "skipped", 41, 41),
];

test("isFailing covers every conclusion that stops dependents", () => {
	for (const c of ["failure", "timed_out", "infra_failure", "config_error", "action_required"]) {
		assert(isFailing(c), `${c} should be failing`);
	}
	for (const c of ["success", "skipped", "cancelled", "neutral", undefined]) {
		assert(!isFailing(c), `${c} should not be failing`);
	}
});

test("chooseFailingStep picks the first failure, not the last step", () => {
	assertEqual(chooseFailingStep(steps)!.number, 3);
	assertEqual(chooseFailingStep([steps[0], steps[1]]), null);
});

test("chooseFailingJob picks the first failing job of a run", () => {
	const jobs = [
		{ id: 1, name: "build", status: "completed", conclusion: "success" },
		{ id: 2, name: "test", status: "completed", conclusion: "infra_failure" },
		{ id: 3, name: "deploy", status: "completed", conclusion: "failure" },
	];
	assertEqual(chooseFailingJob(jobs)!.id, 2);
	assertEqual(chooseFailingJob([jobs[0]]), null);
});

test("looksLikeError recognises the shapes an operator scans for", () => {
	for (const s of [
		"Error: cannot find module",
		"npm ERR! code E404",
		"--- FAIL: TestThing (0.01s)",
		"panic: runtime error: index out of range",
		"fatal: could not read from remote repository",
		"Traceback (most recent call last)",
		"##[error]Process completed with exit code 1",
		"process exited with status 2",
	]) {
		assert(looksLikeError(s), `should match: ${s}`);
	}
	for (const s in { "compiling package": 1, "ok  github.com/x/y  0.2s": 1, "": 1 }) {
		assert(!looksLikeError(s), `should not match: ${s}`);
	}
});

test("firstErrorSeq stays inside the step's log range", () => {
	const lines = [
		{ seq: 5, text: "Error: this one is in an earlier step" },
		{ seq: 22, text: "running tests" },
		{ seq: 27, text: "--- FAIL: TestThing" },
		{ seq: 30, text: "Error: later" },
	];
	assertEqual(firstErrorSeq(lines, 21, 40), 27);
});

test("firstErrorSeq falls back to the first non-empty stderr line", () => {
	const lines = [
		{ seq: 21, text: "quiet", stream: "stdout" },
		{ seq: 22, text: "   ", stream: "stderr" },
		{ seq: 23, text: "something on stderr", stream: "stderr" },
	];
	assertEqual(firstErrorSeq(lines, 21, 40), 23);
});

test("firstErrorSeq returns null when nothing in range looks wrong", () => {
	assertEqual(firstErrorSeq([{ seq: 22, text: "all fine" }], 21, 40), null);
});

test("a successful job gets no focus at all", () => {
	const f = chooseFocus({ id: 1, name: "build", status: "completed", conclusion: "success" }, steps);
	assertDeepEqual(f, { stepNumber: null, seq: null, reason: "" });
});

test("a running job gets no focus", () => {
	assertEqual(chooseFocus({ id: 1, name: "build", status: "in_progress" }, steps).stepNumber, null);
});

test("a failed job opens on the failing step, at its first error line", () => {
	const lines = [
		{ seq: 22, text: "running tests" },
		{ seq: 27, text: "--- FAIL: TestThing" },
	];
	const f = chooseFocus({ id: 1, name: "test", status: "completed", conclusion: "failure" }, steps, lines);
	assertEqual(f.stepNumber, 3);
	assertEqual(f.seq, 27);
	assert(f.reason.includes("Run tests"), "the reason names the step");
	assert(f.reason.includes("27"), "the reason names the line");
});

test("with no lines loaded yet, the focus is the failing step's first line", () => {
	const f = chooseFocus({ id: 1, name: "test", status: "completed", conclusion: "failure" }, steps);
	assertEqual(f.stepNumber, 3);
	assertEqual(f.seq, 21);
});

test("an infra failure focuses just like a user failure", () => {
	const s = [step(1, "Set up job", "infra_failure", 1, 4)];
	const f = chooseFocus({ id: 1, name: "test", status: "completed", conclusion: "infra_failure" }, s);
	assertEqual(f.stepNumber, 1);
	assertEqual(f.seq, 1);
});

test("a job that failed before any step says so instead of opening at the top", () => {
	const f = chooseFocus({ id: 1, name: "test", status: "completed", conclusion: "config_error" }, []);
	assertEqual(f.stepNumber, null);
	assertEqual(f.seq, null);
	assert(f.reason.includes("before any step ran"), `reason was: ${f.reason}`);
});

test("line anchors round-trip, single and range", () => {
	assertDeepEqual(parseLineAnchor("#L1234"), { from: 1234, to: 1234 });
	assertDeepEqual(parseLineAnchor("L12-L20"), { from: 12, to: 20 });
	assertDeepEqual(parseLineAnchor("#L20-L12"), { from: 12, to: 20 });
	assertEqual(parseLineAnchor("#nonsense"), null);
	assertEqual(parseLineAnchor(""), null);
	assertEqual(formatLineAnchor(5, 5), "L5");
	assertEqual(formatLineAnchor(5, 9), "L5-L9");
});
