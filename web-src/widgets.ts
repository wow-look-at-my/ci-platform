// Shared UI pieces: status badges, timing bars, the theme toggle.

import type { Timing } from "./api.js";
import { el, svg } from "./dom.js";
import { formatDuration, formatRelative, formatTimestamp } from "./format.js";
import { isPlatformFault, toneExplanation, toneIcon, toneLabel, toneOf } from "./status.js";

/** The badge every status is rendered with. Icon plus text, never colour alone. */
export function statusBadge(status: string, conclusion?: string): HTMLElement {
	const tone = toneOf(status, conclusion);
	return el(
		"span",
		{ class: `badge tone-${tone}`, title: toneExplanation(tone) || toneLabel(tone) },
		el("span", { class: "badge-icon", "aria-hidden": "true" }, toneIcon(tone)),
		toneLabel(tone),
	);
}

export function toneClass(status: string, conclusion?: string): string {
	return `tone-${toneOf(status, conclusion)}`;
}

/**
 * The banner that keeps a platform fault off the user. Returns null for
 * anything that really is the user's code failing.
 */
export function classificationBanner(status: string, conclusion: string | undefined, reason: string | undefined): HTMLElement | null {
	const tone = toneOf(status, conclusion);
	if (!isPlatformFault(tone) && !reason) return null;
	return el(
		"div",
		{ class: `panel classification tone-${tone}`, role: "note" },
		el("h2", {}, el("span", { class: "badge-icon", "aria-hidden": "true" }, toneIcon(tone)), " ", toneLabel(tone)),
		isPlatformFault(tone) ? el("p", { class: "lede" }, toneExplanation(tone)) : null,
		reason ? el("p", { class: "reason" }, reason) : null,
	);
}

/** Queued / setup / execute as one proportional bar. */
export function timingBar(t: Timing): HTMLElement {
	const parts: { key: string; label: string; value: number }[] = [
		{ key: "queued", label: "Queued", value: t.queued_for },
		{ key: "setup", label: "Setup", value: t.setup_for },
		{ key: "execute", label: "Execute", value: t.execute_for },
	];
	const total = parts.reduce((a, p) => a + Math.max(0, p.value), 0);
	const bar = el("div", { class: "timing-bar", role: "img", "aria-label": parts.map((p) => `${p.label} ${formatDuration(p.value)}`).join(", ") });
	for (const p of parts) {
		if (p.value <= 0) continue;
		bar.appendChild(
			el("span", {
				class: `timing-seg timing-${p.key}`,
				style: `flex-grow:${p.value / (total || 1)}`,
				title: `${p.label}: ${formatDuration(p.value)}`,
			}),
		);
	}
	const legend = el("div", { class: "timing-legend" });
	for (const p of parts) {
		legend.appendChild(el("span", { class: "timing-key" }, el("i", { class: `swatch timing-${p.key}` }), `${p.label} ${formatDuration(p.value)}`));
	}
	return el("div", { class: "timing" }, bar, legend);
}

export function relTime(iso: string | undefined, now = Date.now()): HTMLElement {
	return el("time", { datetime: iso ?? "", title: formatTimestamp(iso) }, formatRelative(iso, now));
}

/** A labelled chip, used for labels, events, branches. */
export function chip(text: string, extraClass = ""): HTMLElement {
	return el("span", { class: `chip ${extraClass}`.trim() }, text);
}

/** Inline sparkline-style area chart. No chart library, no dependencies. */
export function lineChart(points: { x: number; y: number }[], opts: { width?: number; height?: number; label?: string } = {}): SVGElement {
	const width = opts.width ?? 640;
	const height = opts.height ?? 160;
	const pad = 24;
	const root = svg("svg", {
		class: "chart",
		viewBox: `0 0 ${width} ${height}`,
		role: "img",
		"aria-label": opts.label ?? "chart",
	});
	if (points.length === 0) {
		root.appendChild(svg("text", { x: width / 2, y: height / 2, "text-anchor": "middle", class: "chart-empty" }, "no samples in this window"));
		return root;
	}
	const xs = points.map((p) => p.x);
	const ys = points.map((p) => p.y);
	const minX = Math.min(...xs);
	const maxX = Math.max(...xs);
	const maxY = Math.max(1, ...ys);
	const sx = (x: number) => pad + ((x - minX) / Math.max(1, maxX - minX)) * (width - pad * 2);
	const sy = (y: number) => height - pad - (y / maxY) * (height - pad * 2);

	root.appendChild(svg("line", { class: "chart-axis", x1: pad, y1: height - pad, x2: width - pad, y2: height - pad }));
	root.appendChild(svg("line", { class: "chart-axis", x1: pad, y1: pad, x2: pad, y2: height - pad }));
	root.appendChild(svg("text", { class: "chart-tick", x: 4, y: pad + 4 }, String(maxY)));
	root.appendChild(svg("text", { class: "chart-tick", x: 4, y: height - pad }, "0"));

	const line = points.map((p, i) => `${i === 0 ? "M" : "L"} ${sx(p.x)} ${sy(p.y)}`).join(" ");
	const area = `${line} L ${sx(points[points.length - 1].x)} ${height - pad} L ${sx(points[0].x)} ${height - pad} Z`;
	root.appendChild(svg("path", { class: "chart-area", d: area }));
	root.appendChild(svg("path", { class: "chart-line", d: line }));
	for (const p of points) {
		root.appendChild(svg("circle", { class: "chart-dot", cx: sx(p.x), cy: sy(p.y), r: 2.5 }));
	}
	return root;
}

const THEME_KEY = "ci-theme";

export function applyStoredTheme(): void {
	const stored = localStorage.getItem(THEME_KEY);
	if (stored === "light" || stored === "dark") document.documentElement.dataset.theme = stored;
}

export function themeToggle(): HTMLElement {
	const button = el("button", { class: "ghost", type: "button", title: "Toggle light and dark theme" });
	const paint = () => {
		const current = document.documentElement.dataset.theme;
		button.textContent = current === "dark" ? "☾ Dark" : current === "light" ? "☀ Light" : "◐ System";
	};
	button.addEventListener("click", () => {
		const order = ["", "light", "dark"];
		const next = order[(order.indexOf(document.documentElement.dataset.theme ?? "") + 1) % order.length];
		if (next === "") {
			delete document.documentElement.dataset.theme;
			localStorage.removeItem(THEME_KEY);
		} else {
			document.documentElement.dataset.theme = next;
			localStorage.setItem(THEME_KEY, next);
		}
		paint();
	});
	paint();
	return button;
}
