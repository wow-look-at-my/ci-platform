// Which palette the page is painted in. The DOM half of this lives in
// widgets.ts; what is here is the decision, which is worth testing on its own.

export type Preference = "dark" | "light" | "system";
export type Palette = "dark" | "light";

export const THEME_KEY = "ci-theme";
export const PREFERENCES: Preference[] = ["dark", "light", "system"];
export const LABELS: Record<Preference, string> = { dark: "☾ Dark", light: "☀ Light", system: "◐ System" };

/**
 * Dark unless the viewer chose otherwise. Following the operating system is a
 * setting they can pick, never what they get for picking nothing -- a
 * dashboard that is dark on one machine and light on the next, with no choice
 * made on either, reads as a bug rather than as a preference.
 */
export function preferenceOf(stored: string | null): Preference {
	return stored === "light" || stored === "system" ? stored : "dark";
}

export function nextPreference(current: Preference): Preference {
	return PREFERENCES[(PREFERENCES.indexOf(current) + 1) % PREFERENCES.length];
}

/** data-theme always names a concrete palette, so app.css needs no media query. */
export function paletteFor(pref: Preference, systemPrefersLight: boolean): Palette {
	if (pref !== "system") return pref;
	return systemPrefersLight ? "light" : "dark";
}
