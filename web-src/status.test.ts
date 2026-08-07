import { isPlatformFault, toneExplanation, toneIcon, toneLabel, toneOf, toneOfClass, type Tone } from "./status.js";
import { assert, assertEqual, test } from "./testing.js";

test("an incomplete unit's tone comes from its status, not its conclusion", () => {
	assertEqual(toneOf("queued"), "queued");
	assertEqual(toneOf("in_progress"), "running");
	assertEqual(toneOf("waiting"), "waiting");
	assertEqual(toneOf("in_progress", "success"), "running");
});

test("infra and config failures are NOT the red a broken build gets", () => {
	assertEqual(toneOf("completed", "failure"), "failure");
	assertEqual(toneOf("completed", "timed_out"), "failure");
	assertEqual(toneOf("completed", "infra_failure"), "infra");
	assertEqual(toneOf("completed", "config_error"), "config");
	assert(toneOf("completed", "infra_failure") !== toneOf("completed", "failure"), "infra must not share the failure tone");
	assert(toneOf("completed", "config_error") !== toneOf("completed", "failure"), "config must not share the failure tone");
});

test("the remaining conclusions", () => {
	assertEqual(toneOf("completed", "success"), "success");
	assertEqual(toneOf("completed", "cancelled"), "cancelled");
	assertEqual(toneOf("completed", "skipped"), "skipped");
	assertEqual(toneOf("completed", "stale"), "neutral");
	assertEqual(toneOf("completed", undefined), "neutral");
});

test("toneOfClass maps a failure class on its own", () => {
	assertEqual(toneOfClass("infra"), "infra");
	assertEqual(toneOfClass("config"), "config");
	assertEqual(toneOfClass("user"), "failure");
	assertEqual(toneOfClass(""), null);
});

test("isPlatformFault is exactly infra and config", () => {
	assert(isPlatformFault("infra"), "infra");
	assert(isPlatformFault("config"), "config");
	for (const t of ["success", "failure", "cancelled", "skipped", "neutral", "running", "queued", "waiting"] as Tone[]) {
		assert(!isPlatformFault(t), `${t} is not a platform fault`);
	}
});

test("every tone has a label and a distinct icon, so colour is never the only signal", () => {
	const tones: Tone[] = ["success", "failure", "infra", "config", "cancelled", "skipped", "neutral", "running", "queued", "waiting"];
	const icons = new Set<string>();
	for (const t of tones) {
		assert(toneLabel(t).length > 0, `${t} has no label`);
		assert(toneIcon(t).length > 0, `${t} has no icon`);
		icons.add(toneIcon(t));
	}
	assertEqual(icons.size, tones.length, "icons must be distinct");
});

test("the platform-fault tones carry the sentence that keeps them off the user", () => {
	assert(toneExplanation("infra").includes("not your code"), "infra explanation");
	assert(toneExplanation("config").length > 0, "config explanation");
	assertEqual(toneExplanation("failure"), "");
});
