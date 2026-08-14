// Runner hosts: the machines allowed to run runners, and the approval an
// operator gives their key.
//
// The pending list is the reason this page exists. A host that has enrolled and
// not been approved is a runner somebody set up that will never take a job, and
// nothing else in the UI would say so.

import { api, type RunnerHost } from "../api.js";
import { el } from "../dom.js";
import { chip, relTime } from "../widgets.js";

/** Renders the host section into a container the caller owns, and reloads it. */
export function renderRunnerHosts(section: HTMLElement, onChange: () => void): void {
	void api.runnerHosts()
		.then((data) => {
			section.replaceChildren(
				el("header", { class: "page-head" },
					el("h2", {}, "Runner hosts"),
					data.pending_count > 0
						? el("span", { class: "badge tone-config" },
							`${data.pending_count} waiting for approval`)
						: el("span", { class: "muted" }, `${data.total_count} enrolled`),
				),
				data.hosts.length === 0 ? empty() : table(data.hosts, onChange),
			);
		})
		.catch((err: unknown) => {
			section.replaceChildren(el("p", { class: "mono danger" },
				`could not load runner hosts: ${err instanceof Error ? err.message : String(err)}`));
		});
}

function empty(): HTMLElement {
	return el("p", { class: "empty" },
		"No host has enrolled. Start a runner-host pointed at this control plane and its "
		+ "fingerprint will appear here for approval.");
}

function table(hosts: RunnerHost[], onChange: () => void): HTMLElement {
	const body = el("tbody");
	for (const h of hosts) {
		body.appendChild(
			el("tr", { class: `host-${h.state}` },
				el("td", {}, el("span", { class: `dot host-state-${h.state}` }), h.state),
				el("td", {},
					el("strong", {}, h.name || "(unnamed)"),
					// The fingerprint is what the operator compares against what
					// the host printed, so it is shown in full rather than
					// truncated to fit.
					el("div", { class: "muted mono fingerprint" }, h.fingerprint),
					h.enrolled_from ? el("div", { class: "muted" }, `from ${h.enrolled_from}`) : el("span"),
				),
				el("td", {}, ...h.labels.map((l) => chip(l))),
				el("td", {}, h.last_seen_at ? relTime(h.last_seen_at) : el("span", { class: "muted" }, "never")),
				el("td", {}, approvalCell(h)),
				el("td", {}, actions(h, onChange)),
			),
		);
	}
	return el("div", { class: "scroll-x" },
		el("table", { class: "runner-hosts" },
			// No platform column: the fleet table below already says what each
			// runner is, and the fingerprint needs the width more.
			el("thead", {}, el("tr", {},
				...["State", "Host", "Labels", "Last seen", "Approval", ""].map((h) => el("th", {}, h)))),
			body));
}

function approvalCell(h: RunnerHost): HTMLElement {
	if (h.state === "approved" && h.approved_by) {
		return el("span", {}, `by ${h.approved_by} `, relTime(h.approved_at ?? h.enrolled_at));
	}
	if (h.note) return el("span", { class: "muted" }, h.note);
	return el("span", { class: "muted" }, "—");
}

function actions(h: RunnerHost, onChange: () => void): HTMLElement {
	const wrap = el("div", { class: "host-actions" });
	const run = (fn: () => Promise<unknown>, button: HTMLButtonElement) => {
		button.disabled = true;
		void fn()
			.then(onChange)
			.catch((err: unknown) => {
				button.disabled = false;
				// A failed approval must not look like a slow one.
				wrap.appendChild(el("span", { class: "mono danger" },
					err instanceof Error ? err.message : String(err)));
			});
	};

	if (h.state !== "approved") {
		const approve = el("button", { class: "primary" }, "Approve") as HTMLButtonElement;
		approve.onclick = () => {
			// Approving hands this machine execution on the fleet, so the click
			// is deliberate rather than one stray tap on a row.
			const note = prompt(
				`Approve ${h.fingerprint}?\n\nCheck this matches the fingerprint the host printed. `
				+ `Optionally note which machine it is:`, h.name);
			if (note === null) return;
			run(() => api.approveRunnerHost(h.fingerprint, note), approve);
		};
		wrap.appendChild(approve);
	}
	if (h.state !== "revoked") {
		const revoke = el("button", { class: "ghost" }, "Revoke") as HTMLButtonElement;
		revoke.onclick = () => {
			const note = prompt(`Revoke ${h.fingerprint}? Say why:`, "");
			if (note === null) return;
			run(() => api.revokeRunnerHost(h.fingerprint, note), revoke);
		};
		wrap.appendChild(revoke);
	}
	return wrap;
}
