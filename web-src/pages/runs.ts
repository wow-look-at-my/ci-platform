// Run list: filters, search, paging, auto-refresh.

import { api, type Run } from "../api.js";
import { el, errorPanel, replace, spinner } from "../dom.js";
import { formatDuration, shortSha } from "../format.js";
import { navigate, replaceParams } from "../router.js";
import { chip, relTime, statusBadge, toneClass } from "../widgets.js";

const FILTERS = [
	{ name: "branch", label: "Branch", placeholder: "master" },
	{ name: "actor", label: "Actor", placeholder: "who triggered it" },
	{ name: "event", label: "Event", placeholder: "push" },
	{ name: "workflow", label: "Workflow", placeholder: "CI" },
] as const;

const STATUSES = ["", "queued", "in_progress", "completed", "waiting"];
const CONCLUSIONS = ["", "success", "failure", "infra_failure", "config_error", "cancelled", "timed_out", "skipped", "neutral", "stale", "action_required"];

export function renderRuns(root: HTMLElement, params: URLSearchParams, registerTimer: (id: number) => void): void {
	const results = el("div", { class: "results" }, spinner("Loading runs"));
	const form = buildForm(params);
	replace(root, el("header", { class: "page-head" }, el("h1", {}, "Runs")), form, results);

	const load = async () => {
		const filters: Record<string, string> = {};
		for (const [k, v] of params.entries()) if (v) filters[k] = v;
		try {
			const data = await api.runs(filters);
			replace(results, table(data.workflow_runs), pager(data.total_count, data.page, data.per_page));
		} catch (err) {
			replace(results, errorPanel("the run list", err));
		}
	};
	void load();
	registerTimer(setInterval(load, 10000) as unknown as number);
}

function buildForm(params: URLSearchParams): HTMLElement {
	const apply = (name: string, value: string) => {
		replaceParams({ [name]: value, page: undefined });
		dispatchEvent(new HashChangeEvent("hashchange"));
	};

	const form = el("form", { class: "filters", onsubmit: (e: Event) => e.preventDefault() });
	for (const f of FILTERS) {
		const input = el("input", { type: "search", name: f.name, placeholder: f.placeholder, value: params.get(f.name) ?? "" });
		input.addEventListener("change", () => apply(f.name, input.value.trim()));
		form.appendChild(el("label", {}, f.label, input));
	}
	form.appendChild(select("Status", "status", STATUSES, params.get("status") ?? "", apply));
	form.appendChild(select("Conclusion", "conclusion", CONCLUSIONS, params.get("conclusion") ?? "", apply));

	const search = el("input", { type: "search", name: "q", placeholder: "sha, branch or workflow", value: params.get("q") ?? "" });
	search.addEventListener("change", () => apply("q", search.value.trim()));
	form.appendChild(el("label", { class: "grow" }, "Search", search));

	form.appendChild(el("button", { type: "button", class: "ghost", onclick: () => navigate("/runs") }, "Clear"));
	return form;
}

function select(label: string, name: string, options: string[], value: string, apply: (n: string, v: string) => void): HTMLElement {
	const sel = el("select", { name });
	for (const o of options) {
		sel.appendChild(el("option", { value: o, selected: o === value }, o === "" ? "any" : o.replace(/_/g, " ")));
	}
	sel.addEventListener("change", () => apply(name, sel.value));
	return el("label", {}, label, sel);
}

function table(runs: Run[]): HTMLElement {
	if (runs.length === 0) return el("p", { class: "empty" }, "No runs match these filters.");
	const body = el("tbody");
	for (const r of runs) {
		body.appendChild(
			el("tr", { class: toneClass(r.status, r.conclusion), onclick: () => navigate(`/runs/${r.id}`), tabindex: 0,
				onkeydown: (e: Event) => { if ((e as KeyboardEvent).key === "Enter") navigate(`/runs/${r.id}`); } },
				el("td", {}, statusBadge(r.status, r.conclusion)),
				el("td", {}, el("a", { href: `#/runs/${r.id}` }, r.workflow_name), el("span", { class: "muted" }, ` #${r.run_number}`),
					r.attempt > 1 ? el("span", { class: "muted" }, ` · attempt ${r.attempt}`) : null),
				el("td", {}, chip(r.head_branch)),
				el("td", { class: "mono" }, shortSha(r.head_sha)),
				el("td", {}, r.event),
				el("td", {}, r.actor),
				el("td", {}, formatDuration(r.timing.total_for)),
				el("td", {}, relTime(r.created_at)),
			),
		);
	}
	return el("div", { class: "scroll-x" },
		el("table", { class: "runs" },
			el("thead", {}, el("tr", {},
				...["Status", "Workflow", "Branch", "Commit", "Event", "Actor", "Duration", "Started"].map((h) => el("th", {}, h)))),
			body),
	);
}

function pager(total: number, page: number, perPage: number): HTMLElement {
	const pages = Math.max(1, Math.ceil(total / perPage));
	const go = (p: number) => {
		replaceParams({ page: p });
		dispatchEvent(new HashChangeEvent("hashchange"));
	};
	return el("nav", { class: "pager" },
		el("button", { type: "button", class: "ghost", disabled: page <= 1, onclick: () => go(page - 1) }, "← Newer"),
		el("span", {}, `Page ${page} of ${pages} · ${total} run${total === 1 ? "" : "s"}`),
		el("button", { type: "button", class: "ghost", disabled: page >= pages, onclick: () => go(page + 1) }, "Older →"),
	);
}
