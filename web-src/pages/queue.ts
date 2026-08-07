// Queue: depth over time, the oldest waiting job, per-label starvation.

import { api, type QueueSample, type QueueStats } from "../api.js";
import { el, errorPanel, replace, spinner } from "../dom.js";
import { formatDuration } from "../format.js";
import { lineChart } from "../widgets.js";

const WINDOWS = ["15m", "1h", "6h", "24h"];

export function renderQueue(root: HTMLElement, params: URLSearchParams, registerTimer: (id: number) => void): void {
	const since = params.get("since") ?? "1h";
	replace(root, spinner("Loading queue"));
	const load = async () => {
		try {
			const [stats, history] = await Promise.all([api.queue(), api.queueHistory(since)]);
			replace(root,
				el("header", { class: "page-head" }, el("h1", {}, "Queue")),
				starvation(stats),
				summary(stats),
				chartPanel(history.samples, since),
				byLabel(stats),
			);
		} catch (err) {
			replace(root, errorPanel("the queue", err));
		}
	};
	void load();
	registerTimer(setInterval(load, 5000) as unknown as number);
}

function starvation(stats: QueueStats): HTMLElement | null {
	if (stats.starved_labels.length === 0) return null;
	return el("div", { class: "panel tone-infra", role: "alert" },
		el("h2", {}, "⚠ Starved labels"),
		el("p", {}, "These labels have queued work and no online runner, which is why those jobs are not starting:"),
		el("ul", {}, ...stats.starved_labels.map((l) => el("li", { class: "mono" }, l))),
	);
}

function summary(stats: QueueStats): HTMLElement {
	return el("section", { class: "panel stat-row" },
		el("div", { class: "stat" }, el("span", { class: "stat-value" }, String(stats.depth)), el("span", { class: "stat-label" }, "Queued jobs")),
		el("div", { class: "stat" }, el("span", { class: "stat-value" }, formatDuration(stats.oldest_waiting)), el("span", { class: "stat-label" }, "Oldest wait")),
		el("div", { class: "stat" },
			el("span", { class: "stat-value" }, stats.oldest_job_id ? el("a", { href: `#/jobs/${stats.oldest_job_id}` }, `#${stats.oldest_job_id}`) : "—"),
			el("span", { class: "stat-label" }, "Oldest waiting job")),
	);
}

function chartPanel(samples: QueueSample[], since: string): HTMLElement {
	const points = samples.map((s) => ({ x: Date.parse(s.at), y: s.depth }));
	const picker = el("div", { class: "window-picker" });
	for (const w of WINDOWS) {
		picker.appendChild(el("a", { class: `button ghost${w === since ? " selected" : ""}`, href: `#/queue?since=${w}` }, w));
	}
	return el("section", { class: "panel" },
		el("h2", {}, "Depth over time"),
		picker,
		el("div", { class: "chart-box" }, lineChart(points, { label: `queue depth over the last ${since}` })),
		el("p", { class: "muted" }, `${samples.length} sample${samples.length === 1 ? "" : "s"} in this window`),
	);
}

function byLabel(stats: QueueStats): HTMLElement {
	const labels = new Set([...Object.keys(stats.depth_by_label), ...Object.keys(stats.runners_by_label)]);
	if (labels.size === 0) return el("section", { class: "panel" }, el("h2", {}, "By label"), el("p", { class: "empty" }, "Nothing queued."));
	const body = el("tbody");
	for (const l of [...labels].sort()) {
		const starved = stats.starved_labels.includes(l);
		body.appendChild(el("tr", { class: starved ? "tone-infra" : "" },
			el("td", { class: "mono" }, l),
			el("td", {}, String(stats.depth_by_label[l] ?? 0)),
			el("td", {}, String(stats.runners_by_label[l] ?? 0)),
			el("td", {}, String(stats.idle_by_label[l] ?? 0)),
			el("td", {}, starved ? "starved — no runner can take this work" : ""),
		));
	}
	return el("section", { class: "panel" }, el("h2", {}, "By label"),
		el("div", { class: "scroll-x" },
			el("table", {},
				el("thead", {}, el("tr", {}, ...["Label", "Queued", "Runners", "Idle", ""].map((h) => el("th", {}, h)))),
				body)));
}
