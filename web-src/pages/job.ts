// Job page: steps, classification, timing, and the live log viewer.
//
// Failure-first is the headline behaviour here: a failed job opens on its
// failing step, scrolled to the first error line, never at the top of a
// 40,000-line log.

import { api, type JobDetail, type LogLineWire, type Step } from "../api.js";
import { parseAnsi } from "../ansi.js";
import { el, errorPanel, kv, replace, spinner } from "../dom.js";
import { chooseFocus, formatLineAnchor, parseLineAnchor, type Focus } from "../failfirst.js";
import { formatDuration } from "../format.js";
import { foldGroups, groupsContaining, type Block, type GroupBlock, type LogLine } from "../groups.js";
import { replaceParams } from "../router.js";
import { chip, classificationBanner, relTime, statusBadge, timingBar, toneClass } from "../widgets.js";
import { confirmCancel, runAction, toast } from "./actions.js";

const FIRST_PAGE = 5000;

export function renderJob(root: HTMLElement, id: number, params: URLSearchParams, registerCleanup: (fn: () => void) => void): void {
	replace(root, spinner("Loading job"));
	void (async () => {
		let job: JobDetail;
		try {
			job = await api.job(id, params.has("attempt") ? Number(params.get("attempt")) : undefined);
		} catch (err) {
			replace(root, errorPanel(`job ${id}`, err));
			return;
		}
		const view = new JobView(job, params, registerCleanup);
		replace(root, view.root);
		void view.loadLogs();
	})();
}

class JobView {
	readonly root: HTMLElement;
	private readonly job: JobDetail;
	private readonly params: URLSearchParams;
	private readonly logBody: HTMLElement;
	private readonly statusLine: HTMLElement;
	private readonly focusReason: HTMLElement;
	private logSection: HTMLElement | null = null;
	private lines: LogLineWire[] = [];
	private nextSeq = 0;
	private focus: Focus = { stepNumber: null, seq: null, reason: "" };
	private selection: { from: number; to: number } | null = null;
	private openGroups = new Set<number>();
	private source: EventSource | null = null;
	private followTail = true;

	constructor(job: JobDetail, params: URLSearchParams, registerCleanup: (fn: () => void) => void) {
		this.job = job;
		this.params = params;
		this.logBody = el("div", { class: "log", role: "log", tabindex: 0 });
		this.statusLine = el("div", { class: "log-status" }, "connecting…");
		this.focusReason = el("p", { class: "focus-reason" });
		this.root = this.build();
		registerCleanup(() => this.source?.close());
	}

	private build(): HTMLElement {
		const job = this.job;
		const frag = el("div", { class: "job-page" });
		frag.appendChild(
			el("header", { class: "page-head" },
				el("div", {},
					el("h1", {}, statusBadge(job.status, job.conclusion), " ", job.name),
					el("p", { class: "sub" },
						job.repository ? el("a", { href: `#/runs/${job.run_id}` }, `${job.workflow_name} #${job.run_number}`) : null,
						" · ", chip(job.head_branch ?? ""), " · attempt ", `${job.attempt}/${job.max_attempts}`,
						job.runner_id ? el("span", {}, " · on ", el("a", { href: "#/runners" }, job.runner_id)) : null),
				),
				this.actions(),
			),
		);

		const banner = classificationBanner(job.status, job.conclusion, job.cancel?.sentence ?? job.classification_reason);
		if (banner) frag.appendChild(banner);

		frag.appendChild(el("section", { class: "panel" }, el("h2", {}, "Timing"), timingBar(job.timing),
			el("dl", { class: "kvs" },
				kv("Queued", formatDuration(job.timing.queued_for)),
				kv("Setup", formatDuration(job.timing.setup_for)),
				kv("Execute", formatDuration(job.timing.execute_for)),
				kv("Requeues", `${job.requeue_count}`),
				kv("Infra retries", `${job.infra_retry_count}`),
				kv("Started", relTime(job.started_at)),
			)));

		this.focus = chooseFocus(job, job.steps, []);
		this.focusReason.textContent = this.focus.reason;
		frag.appendChild(this.stepsPanel());
		if (job.annotations.length) frag.appendChild(this.annotationsPanel());
		frag.appendChild(this.logPanel());
		if (job.events.length) {
			frag.appendChild(el("section", { class: "panel" }, el("h2", {}, "Timeline"),
				el("ol", { class: "timeline" }, ...job.events.map((e) =>
					el("li", {}, el("span", { class: "kind" }, e.kind), " ", e.message, " ", relTime(e.at))))));
		}
		return frag;
	}

	private actions(): HTMLElement {
		const job = this.job;
		const box = el("div", { class: "actions" });
		if (job.status !== "completed") {
			box.appendChild(el("button", {
				type: "button", class: "danger",
				onclick: () => void confirmCancel("job", (reason) => api.cancelJob(job.id, reason)),
			}, "Cancel job"));
		}
		box.appendChild(el("button", { type: "button", onclick: () => void runAction("Re-run job", () => api.rerunJob(job.id)) }, "Re-run job"));
		box.appendChild(el("a", { class: "button ghost", href: api.rawLogUrl(job.id, job.attempt), download: "" }, "Download raw log"));
		return box;
	}

	private stepsPanel(): HTMLElement {
		const rows = this.job.steps.map((s) => this.stepRow(s));
		return el("section", { class: "panel" }, el("h2", {}, "Steps"),
			rows.length ? el("ol", { class: "steps" }, ...rows) : el("p", { class: "empty" }, "This attempt recorded no steps."));
	}

	private stepRow(s: Step): HTMLElement {
		const isFocus = this.focus.stepNumber === s.number;
		return el("li", { class: `step ${toneClass(s.status, s.conclusion)}${isFocus ? " focused" : ""}` },
			el("button", {
				type: "button", class: "step-head",
				onclick: () => this.jumpToSeq(s.log_start, true),
				title: `Jump to line ${s.log_start}`,
			},
				statusBadge(s.status, s.conclusion),
				el("span", { class: "step-name" }, `${s.number}. ${s.name}`),
				el("span", { class: "muted" }, formatDuration(s.duration)),
				s.exit_code !== 0 ? el("span", { class: "muted" }, `exit ${s.exit_code}`) : null,
			));
	}

	private annotationsPanel(): HTMLElement {
		return el("section", { class: "panel" }, el("h2", {}, "Annotations"),
			el("ul", { class: "annotations" }, ...this.job.annotations.map((a) =>
				el("li", { class: `tone-${a.annotation_level === "failure" ? "failure" : a.annotation_level === "warning" ? "config" : "neutral"}` },
					el("span", { class: "mono" }, `${a.path}:${a.start_line}`), " ", a.title ? el("strong", {}, a.title + " ") : null, a.message))));
	}

	private logPanel(): HTMLElement {
		const toolbar = el("div", { class: "log-toolbar" },
			el("label", {}, el("input", {
				type: "checkbox", checked: true,
				onchange: (e: Event) => { this.followTail = (e.target as HTMLInputElement).checked; },
			}), "Follow tail"),
			el("button", { type: "button", class: "ghost", onclick: () => this.expandAll(true) }, "Expand all"),
			el("button", { type: "button", class: "ghost", onclick: () => this.expandAll(false) }, "Collapse all"),
			el("button", { type: "button", class: "ghost", onclick: () => this.copyLink() }, "Copy link to selection"),
			this.statusLine,
		);
		this.logSection = el("section", { class: "panel log-panel" },
			el("h2", {}, "Log"),
			this.focusReason,
			toolbar,
			this.logBody);
		return this.logSection;
	}

	async loadLogs(): Promise<void> {
		try {
			const page = await api.logs(this.job.id, { limit: FIRST_PAGE, attempt: this.job.attempt });
			this.lines = page.lines;
			this.nextSeq = page.next_seq;
		} catch (err) {
			replace(this.logBody, errorPanel("the job log", err));
			this.statusLine.textContent = "log unavailable";
			return;
		}
		// Recompute now that the lines are here: with no lines the chooser can
		// only offer the step's first line, and the stale sentence would name
		// a line the page never scrolled to.
		this.focus = chooseFocus(this.job, this.job.steps, this.lines);
		this.focusReason.textContent = this.focus.reason;
		const anchor = parseLineAnchor(this.params.get("line") ?? "");
		if (anchor) this.selection = anchor;
		this.render();

		const target = anchor?.from ?? this.focus.seq;
		// Highlight what we jumped to, unless an explicit anchor already
		// selected a range.
		if (target !== null) this.jumpToSeq(target, this.selection === null);

		if (this.job.status !== "completed") this.startStream();
		else this.statusLine.textContent = `${this.lines.length} lines`;
	}

	private startStream(): void {
		const url = api.streamUrl(this.job.id, { from_seq: this.nextSeq, attempt: this.job.attempt });
		const source = new EventSource(url);
		this.source = source;
		this.statusLine.textContent = "live";
		source.addEventListener("log", (ev) => {
			const line = JSON.parse((ev as MessageEvent).data) as LogLineWire;
			this.lines.push(line);
			this.nextSeq = line.seq + 1;
			this.render();
			if (this.followTail) this.logBody.scrollTop = this.logBody.scrollHeight;
		});
		source.addEventListener("eof", () => {
			this.statusLine.textContent = `finished · ${this.lines.length} lines`;
			source.close();
		});
		source.addEventListener("error", () => {
			// EventSource reconnects on its own with Last-Event-ID; say so
			// rather than leaving a page that has quietly stopped updating.
			this.statusLine.textContent = "reconnecting…";
		});
	}

	private expandAll(open: boolean): void {
		const walk = (blocks: Block[]) => {
			for (const b of blocks) {
				if (b.kind !== "group") continue;
				if (open) this.openGroups.add(b.startSeq);
				else this.openGroups.delete(b.startSeq);
				walk(b.children);
			}
		};
		walk(foldGroups(this.lines));
		this.render();
	}

	private jumpToSeq(seq: number, select: boolean): void {
		const blocks = foldGroups(this.lines);
		for (const g of groupsContaining(blocks, seq)) this.openGroups.add(g);
		if (select) this.selection = { from: seq, to: seq };
		this.followTail = false;
		this.render();
		// Scroll inside the log pane, not the page: scrollIntoView would push
		// the classification banner and the step list off screen, which is the
		// context the operator needs alongside the error line.
		const node = this.logBody.querySelector(`[data-seq="${seq}"]`);
		if (node) {
			// Two scrolls, deliberately: the pane scrolls internally so the line
			// sits mid-pane, and the page scrolls to the log section so the line
			// is actually on screen. scrollIntoView on the line alone would do
			// both at once and push the classification banner off the top.
			const line = node.getBoundingClientRect();
			const pane = this.logBody.getBoundingClientRect();
			this.logBody.scrollTop += line.top - pane.top - pane.height / 2;
			this.logSection?.scrollIntoView({ block: "start" });
		}
		replaceParams({ line: formatLineAnchor(seq, seq) });
	}

	private copyLink(): void {
		if (!this.selection) {
			toast("Select a line first: click a line number, shift-click another for a range.", true);
			return;
		}
		const anchor = formatLineAnchor(this.selection.from, this.selection.to);
		replaceParams({ line: anchor });
		const url = `${location.origin}${location.pathname}${location.hash}`;
		void navigator.clipboard?.writeText(url).then(
			() => toast(`Copied link to ${anchor}`),
			() => toast(`Link: ${url}`),
		);
	}

	private render(): void {
		const blocks = foldGroups(this.lines);
		replace(this.logBody, ...this.renderBlocks(blocks));
	}

	private renderBlocks(blocks: Block[]): HTMLElement[] {
		const out: HTMLElement[] = [];
		for (const b of blocks) {
			if (b.kind === "line") {
				out.push(this.renderLine(b.line));
				continue;
			}
			out.push(this.renderGroup(b));
		}
		return out;
	}

	private renderGroup(g: GroupBlock): HTMLElement {
		const open = this.openGroups.has(g.startSeq);
		const head = el("button", {
			type: "button",
			class: "group-head",
			"aria-expanded": open,
			onclick: () => {
				if (open) this.openGroups.delete(g.startSeq);
				else this.openGroups.add(g.startSeq);
				this.render();
			},
		}, el("span", { class: "twisty", "aria-hidden": "true" }, open ? "▾" : "▸"), g.title,
			g.closed ? null : el("span", { class: "muted" }, " (unclosed — the job ended inside this group)"));
		const body = el("div", { class: "group-body" }, ...(open ? this.renderBlocks(g.children) : []));
		return el("div", { class: `group${open ? " open" : ""}` }, head, body);
	}

	private renderLine(line: LogLine): HTMLElement {
		const selected = this.selection && line.seq >= this.selection.from && line.seq <= this.selection.to;
		const number = el("a", {
			class: "ln",
			href: `#`,
			"data-line": line.seq,
			onclick: (ev: Event) => {
				ev.preventDefault();
				const mouse = ev as MouseEvent;
				if (mouse.shiftKey && this.selection) {
					this.selection = { from: Math.min(this.selection.from, line.seq), to: Math.max(this.selection.to, line.seq) };
				} else {
					this.selection = { from: line.seq, to: line.seq };
				}
				replaceParams({ line: formatLineAnchor(this.selection.from, this.selection.to) });
				this.render();
			},
		}, String(line.seq));

		const text = el("span", { class: "lt" });
		for (const span of parseAnsi(line.text)) {
			const style: string[] = [];
			if (span.style.fg) style.push(`color:${span.style.fg}`);
			if (span.style.bg) style.push(`background:${span.style.bg}`);
			if (span.style.bold) style.push("font-weight:700");
			if (span.style.dim) style.push("opacity:.7");
			if (span.style.italic) style.push("font-style:italic");
			if (span.style.underline) style.push("text-decoration:underline");
			text.appendChild(style.length ? el("span", { style: style.join(";") }, span.text) : document.createTextNode(span.text));
		}

		return el("div", {
			class: `line${selected ? " selected" : ""}${line.stream === "stderr" ? " stderr" : ""}`,
			"data-seq": line.seq,
			id: `L${line.seq}`,
		}, number, text);
	}
}
