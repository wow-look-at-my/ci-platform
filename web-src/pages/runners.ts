// Runner fleet: labels, current job, heartbeat age coloured by staleness.

import { api, type Runner } from "../api.js";
import { el, errorPanel, replace, spinner } from "../dom.js";
import { formatDuration, heartbeatStaleness } from "../format.js";
import { chip, relTime } from "../widgets.js";

export function renderRunners(root: HTMLElement, registerTimer: (id: number) => void): void {
	replace(root, spinner("Loading fleet"));
	const load = async () => {
		try {
			const data = await api.runners();
			replace(root,
				el("header", { class: "page-head" }, el("h1", {}, "Runner fleet")),
				el("section", { class: "panel stat-row" },
					stat("Runners", data.total_count),
					stat("Online", data.online),
					stat("Busy", data.busy),
					stat("Idle", data.idle),
					stat("Offline", data.offline, data.offline > 0 ? "tone-infra" : ""),
					stat("Capacity", data.capacity),
				),
				table(data.runners),
			);
		} catch (err) {
			replace(root, errorPanel("the runner fleet", err));
		}
	};
	void load();
	registerTimer(setInterval(load, 5000) as unknown as number);
}

function stat(label: string, value: number | string, extra = ""): HTMLElement {
	return el("div", { class: `stat ${extra}`.trim() }, el("span", { class: "stat-value" }, String(value)), el("span", { class: "stat-label" }, label));
}

function table(runners: Runner[]): HTMLElement {
	if (runners.length === 0) {
		return el("p", { class: "empty" }, "No runners have ever registered. Nothing can be dispatched.");
	}
	const body = el("tbody");
	for (const r of runners) {
		const staleness = heartbeatStaleness(r.heartbeat_age);
		body.appendChild(
			el("tr", { class: `runner-${r.state}` },
				el("td", {}, el("span", { class: `dot state-${r.state}` }), r.state),
				el("td", {}, el("strong", {}, r.name), el("div", { class: "muted mono" }, r.id)),
				el("td", {}, ...r.labels.map((l) => chip(l))),
				el("td", {}, r.current_job_id ? el("a", { href: `#/jobs/${r.current_job_id}` }, `job ${r.current_job_id}`) : el("span", { class: "muted" }, "—")),
				el("td", {}, el("span", { class: `heartbeat hb-${staleness}`, title: `last heartbeat ${r.last_heartbeat}` },
					formatDuration(r.heartbeat_age), " ago")),
				el("td", {}, String(r.capacity)),
				el("td", { class: "mono" }, `${r.os}/${r.arch}`),
				el("td", { class: "mono" }, r.version || "—"),
				el("td", {}, relTime(r.last_heartbeat)),
			),
		);
	}
	return el("div", { class: "scroll-x" },
		el("table", { class: "runners" },
			el("thead", {}, el("tr", {}, ...["State", "Runner", "Labels", "Current job", "Heartbeat", "Capacity", "Platform", "Version", "Last seen"].map((h) => el("th", {}, h)))),
			body));
}
