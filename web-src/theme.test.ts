import { assertEqual, test } from "./testing.js";
import { nextPreference, paletteFor, type Preference, preferenceOf } from "./theme.js";

test("a viewer who has chosen nothing gets dark, whatever their machine reports", () => {
	assertEqual(preferenceOf(null), "dark");
	assertEqual(paletteFor(preferenceOf(null), true), "dark");
	assertEqual(paletteFor(preferenceOf(null), false), "dark");
});

test("a stored value from an older build is not a choice", () => {
	// "" was what this key held when the toggle wrote the empty string for
	// "system"; it must not read as one now.
	assertEqual(preferenceOf(""), "dark");
	assertEqual(preferenceOf("nonsense"), "dark");
});

test("an explicit choice is honoured and does not consult the machine", () => {
	assertEqual(paletteFor("light", false), "light");
	assertEqual(paletteFor("dark", true), "dark");
});

test("system is the only setting that follows the machine", () => {
	assertEqual(paletteFor("system", true), "light");
	assertEqual(paletteFor("system", false), "dark");
});

test("the toggle cycles through every setting and returns to the start", () => {
	const seen: Preference[] = [];
	let at: Preference = preferenceOf(null);
	for (let i = 0; i < 3; i++) {
		seen.push(at);
		at = nextPreference(at);
	}
	assertEqual(seen.join(" "), "dark light system");
	assertEqual(at, "dark");
});
