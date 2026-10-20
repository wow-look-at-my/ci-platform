// Which palette the page is painted in.

export type Preference = "dark" | "light" | "system";
export type Palette = "dark" | "light";

export const THEME_KEY = "ci-theme";
export const PREFERENCES: Preference[] = ["dark", "light", "system"];
export const LABELS: Record<Preference, string> = { dark: "☾ Dark", light: "☀ Light", system: "◐ System" };

/** Dark unless the viewer chose otherwise. */
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
