// Run page: the job DAG as a real graph, attempt selector, timing breakdown.

import { api, type Job, type RunDetail } from "../api.js";
import { layoutDag } from "../dag.js";
import { el, errorPanel, kv, replace, spinner, svg } from "../dom.js";
import { chooseFailingJob } from "../failfirst.js";
import { formatBytes, formatDuration, shortSha } from "../format.js";
import { toneOf } from "../status.js";
import { navigate } from "../router.js";
import { chip, classificationBanner, relTime, statusBadge, timingBar, toneClass } from "../widgets.js";
import { confirmCancel, runAction } from "./actions.js";

export function renderRun(root: HTMLElement, id: number, params: URLSearchParams, registerTimer: (id: number) => void): void {
	replace(root, spinner("Loading run"));
	const load = async () => {
		try {
			const run = await api.run(id);
			replace(root, view(run, params));
		} catch (err) {
			replace(root, errorPanel(`run ${id}`, err));
		}
	};
	void load();
	registerTimer(setInterval(load, 8000) as unknown as number);
}

function view(run: RunDetail, params: URLSearchParams): HTMLElement {
	const failing = chooseFailingJob(run.jobs);
	const frag = el("div", { class: "run-page" });

	frag.appendChild(
		el("header", { class: "page-head" },
			el("div", {},
				el("h1", {}, statusBadge(run.status, run.conclusion), " ", run.workflow_name, el("span", { class: "muted" }, ` #${run.run_number}`)),
				el("p", { class: "sub" },
					el("a", { href: `#/repos/${run.repository}/cache` }, run.repository), " · ",
					chip(run.head_branch), " · ",
					el("span", { class: "mono" }, shortSha(run.head_sha)), " · ",
					run.event, " · by ", run.actor, " · ", relTime(run.created_at)),
			),
			actions(run, failing),
		),
	);

	const banner = classificationBanner(run.status, run.conclusion, run.cancel?.sentence ?? run.classification_reason);
	if (banner) frag.appendChild(banner);

	if (failing) {
		frag.appendChild(
			el("div", { class: "panel jump" },
				el("p", {}, "This run failed in ", el("strong", {}, failing.name), "."),
				el("button", { type: "button", class: "primary", onclick: () => navigate(`/jobs/${failing.id}`, { focus: "1" }) },
					"Jump to the failure"),
			),
		);
	}

	frag.appendChild(el("section", { class: "panel" }, el("h2", {}, "Timing"), timingBar(run.timing),
		el("dl", { class: "kvs" },
			kv("Total", formatDuration(run.timing.total_for)),
			kv("Attempt", `${run.attempt}`),
			kv("Started", relTime(run.started_at)),
			kv("Completed", relTime(run.completed_at)),
		)));

	frag.appendChild(attemptSelector(run, params));
	frag.appendChild(dagPanel(run));
	frag.appendChild(jobTable(run.jobs));
	frag.appendChild(artifactsPanel(run.id));
	if (run.events.length) frag.appendChild(timeline(run.events));
	return frag;
}

function actions(run: RunDetail, failing: Job | null): HTMLElement {
	const box = el("div", { class: "actions" });
	if (run.status !== "completed") {
		box.appendChild(el("button", {
			type: "button", class: "danger",
			onclick: () => void confirmCancel("run", (reason) => api.cancelRun(run.id, reason)),
		}, "Cancel run"));
	}
	box.appendChild(el("button", { type: "button", onclick: () => void runAction("Re-run", () => api.rerunRun(run.id)) }, "Re-run all jobs"));
	if (failing) {
		box.appendChild(el("button", { type: "button", onclick: () => void runAction("Re-run failed", () => api.rerunFailed(run.id)) }, "Re-run failed jobs"));
	}
	return box;
}

function attemptSelector(run: RunDetail, params: URLSearchParams): HTMLElement {
	const current = Number(params.get("attempt") ?? run.attempt);
	const box = el("section", { class: "panel attempts" }, el("h2", {}, "Attempts"));
	const list = el("div", { class: "attempt-list", role: "tablist" });
	for (const a of run.attempts) {
		list.appendChild(
			el("button", {
				type: "button",
				role: "tab",
				class: `attempt${a.attempt === current ? " selected" : ""}`,
				"aria-selected": a.attempt === current,
				onclick: () => navigate(`/runs/${run.id}`, { attempt: a.attempt }),
			}, `Attempt ${a.attempt}`, a.current ? el("span", { class: "muted" }, " (latest)") : null),
		);
	}
	box.appendChild(list);
	return box;
}

function dagPanel(run: RunDetail): HTMLElement {
	const nodes = run.graph.nodes;
	const box = el("section", { class: "panel" }, el("h2", {}, "Job graph"));
	if (nodes.length === 0) {
		box.appendChild(el("p", { class: "empty" }, "This run has no jobs."));
		return box;
	}
	const layout = layoutDag(nodes.map((n) => ({ key: n.key, name: n.name, needs: n.needs })));
	const byKey = new Map(nodes.map((n) => [n.key, n]));
	const jobByKey = new Map(run.jobs.map((j) => [j.key, j]));

	const graph = svg("svg", {
		class: "dag",
		viewBox: `0 0 ${layout.width} ${layout.height}`,
		width: layout.width,
		height: layout.height,
		role: "img",
		"aria-label": `job graph with ${nodes.length} jobs`,
	});
	const defs = svg("defs", {}, svg("marker", { id: "arrow", viewBox: "0 0 10 10", refX: 9, refY: 5, markerWidth: 6, markerHeight: 6, orient: "auto-start-reverse" },
		svg("path", { d: "M 0 0 L 10 5 L 0 10 z" })));
	graph.appendChild(defs);
	for (const e of layout.edges) {
		graph.appendChild(svg("path", { class: "dag-edge", d: e.path, "marker-end": "url(#arrow)" }));
	}
	for (const n of layout.nodes) {
		const meta = byKey.get(n.key)!;
		const job = jobByKey.get(n.key);
		const tone = toneOf(meta.status, meta.conclusion);
		const g = svg("g", {
			class: `dag-node tone-${tone}`,
			tabindex: 0,
			role: "link",
			"aria-label": `${meta.name}: ${meta.status} ${meta.conclusion ?? ""}`,
			onclick: () => job && navigate(`/jobs/${job.id}`),
			onkeydown: (ev: Event) => { if ((ev as KeyboardEvent).key === "Enter" && job) navigate(`/jobs/${job.id}`); },
		});
		g.appendChild(svg("rect", { x: n.x, y: n.y, width: n.width, height: n.height, rx: 8 }));
		g.appendChild(svg("text", { x: n.x + 12, y: n.y + 27, class: "dag-label" }, truncate(meta.name, 22)));
		graph.appendChild(g);
	}
	box.appendChild(el("div", { class: "scroll-x" }, graph));
	return box;
}

function truncate(s: string, n: number): string {
	return s.length <= n ? s : s.slice(0, n - 1) + "…";
}

function jobTable(jobs: Job[]): HTMLElement {
	const body = el("tbody");
	for (const j of jobs) {
		body.appendChild(
			el("tr", { class: toneClass(j.status, j.conclusion) },
				el("td", {}, statusBadge(j.status, j.conclusion)),
				el("td", {}, el("a", { href: `#/jobs/${j.id}` }, j.name)),
				el("td", {}, ...j.labels.map((l) => chip(l))),
				el("td", {}, `${j.attempt}/${j.max_attempts}`),
				el("td", {}, formatDuration(j.timing.queued_for)),
				el("td", {}, formatDuration(j.timing.setup_for)),
				el("td", {}, formatDuration(j.timing.execute_for)),
				el("td", { class: "reason-cell" }, j.classification_reason ?? ""),
			),
		);
	}
	return el("section", { class: "panel" }, el("h2", {}, "Jobs"),
		el("div", { class: "scroll-x" },
			el("table", { class: "jobs" },
				el("thead", {}, el("tr", {}, ...["Status", "Job", "Labels", "Attempt", "Queued", "Setup", "Execute", "Why"].map((h) => el("th", {}, h)))),
				body)));
}

function artifactsPanel(runID: number): HTMLElement {
	const box = el("section", { class: "panel" }, el("h2", {}, "Artifacts"), spinner("Loading artifacts"));
	void (async () => {
		try {
			const data = await api.artifacts(runID);
			const content = data.artifacts.length === 0
				? el("p", { class: "empty" }, "This run uploaded no artifacts.")
				: el("div", { class: "scroll-x" }, el("table", {},
					el("thead", {}, el("tr", {}, ...["Name", "Size", "Digest", "Expires", ""].map((h) => el("th", {}, h)))),
					el("tbody", {}, ...data.artifacts.map((a) => el("tr", {},
						el("td", {}, a.name),
						el("td", {}, formatBytes(a.size_bytes)),
						el("td", { class: "mono" }, a.digest || "—"),
						el("td", {}, relTime(a.expires_at)),
						el("td", {}, a.finalized
							? el("a", { class: "button ghost", href: a.download_url }, "Download")
							: el("span", { class: "muted" }, "still uploading")),
					)))));
			replace(box, el("h2", {}, "Artifacts"), content);
		} catch (err) {
			replace(box, el("h2", {}, "Artifacts"), errorPanel("artifacts", err));
		}
	})();
	return box;
}

function timeline(events: RunDetail["events"]): HTMLElement {
	return el("section", { class: "panel" }, el("h2", {}, "Timeline"),
		el("ol", { class: "timeline" }, ...events.map((e) =>
			el("li", {}, el("span", { class: "kind" }, e.kind), " ", e.message, " ", relTime(e.at)))));
}
