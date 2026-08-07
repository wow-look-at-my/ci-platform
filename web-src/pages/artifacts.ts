// Artifacts browser for one run.

import { api } from "../api.js";
import { el, errorPanel, replace, spinner } from "../dom.js";
import { formatBytes } from "../format.js";
import { relTime } from "../widgets.js";

export function renderArtifacts(root: HTMLElement, runID: number): void {
	replace(root, spinner("Loading artifacts"));
	void (async () => {
		try {
			const data = await api.artifacts(runID);
			replace(root,
				el("header", { class: "page-head" },
					el("h1", {}, "Artifacts"),
					el("p", { class: "sub" }, el("a", { href: `#/runs/${runID}` }, `← run ${runID}`), " · ",
						`${data.total_count} artifact${data.total_count === 1 ? "" : "s"} · ${formatBytes(data.size_bytes)}`)),
				data.artifacts.length === 0
					? el("p", { class: "empty" }, "This run uploaded no artifacts.")
					: el("div", { class: "scroll-x" }, el("table", {},
						el("thead", {}, el("tr", {}, ...["Name", "Size", "Digest", "Job", "Created", "Expires", ""].map((h) => el("th", {}, h)))),
						el("tbody", {}, ...data.artifacts.map((a) => el("tr", { class: a.expired ? "tone-neutral" : "" },
							el("td", {}, a.name),
							el("td", {}, formatBytes(a.size_bytes)),
							el("td", { class: "mono" }, a.digest || "—"),
							el("td", {}, a.job_id ? el("a", { href: `#/jobs/${a.job_id}` }, String(a.job_id)) : "—"),
							el("td", {}, relTime(a.created_at)),
							el("td", {}, a.expired ? el("span", { class: "tone-infra" }, "expired") : relTime(a.expires_at)),
							el("td", {}, a.finalized && !a.expired
								? el("a", { class: "button ghost", href: a.download_url }, "Download")
								: el("span", { class: "muted" }, a.finalized ? "expired" : "still uploading")),
						))))),
			);
		} catch (err) {
			replace(root, errorPanel(`artifacts for run ${runID}`, err));
		}
	})();
}
