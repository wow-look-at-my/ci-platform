// Per-repo cache: entries plus hit/miss/eviction stats per key.

import { api, type CacheView } from "../api.js";
import { el, errorPanel, replace, spinner } from "../dom.js";
import { formatBytes, formatPercent } from "../format.js";
import { relTime } from "../widgets.js";

export function renderCache(root: HTMLElement, owner: string, repo: string): void {
	replace(root, spinner("Loading cache"));
	void (async () => {
		try {
			replace(root, view(await api.cache(owner, repo)));
		} catch (err) {
			replace(root, errorPanel(`the cache for ${owner}/${repo}`, err));
		}
	})();
}

function view(c: CacheView): HTMLElement {
	const frag = el("div", {});
	frag.appendChild(el("header", { class: "page-head" }, el("h1", {}, "Cache · ", el("span", { class: "mono" }, c.repository))));
	frag.appendChild(el("section", { class: "panel stat-row" },
		stat("Usage", formatBytes(c.usage_bytes)),
		stat("Entries", String(c.total_count)),
		stat("Hit rate", formatPercent(c.stats.hit_rate)),
		stat("Hits", String(c.stats.hits)),
		stat("Misses", String(c.stats.misses)),
		stat("Evictions", String(c.stats.evictions), c.stats.evictions > 0 ? "tone-infra" : ""),
	));

	if (!c.entries_complete && c.warning) {
		frag.appendChild(el("div", { class: "panel tone-config", role: "note" },
			el("h2", {}, "This entry list is incomplete"), el("p", {}, c.warning)));
	}

	frag.appendChild(el("section", { class: "panel" }, el("h2", {}, "Per-key stats"),
		el("p", { class: "muted" }, `computed over the last ${c.stats.events_considered} cache events`),
		c.by_key.length === 0
			? el("p", { class: "empty" }, "No cache activity recorded.")
			: el("div", { class: "scroll-x" }, el("table", {},
				el("thead", {}, el("tr", {}, ...["Key", "Hits", "Misses", "Stores", "Evictions", "Hit rate"].map((h) => el("th", {}, h)))),
				el("tbody", {}, ...c.by_key.map((k) => el("tr", {},
					el("td", { class: "mono" }, k.key),
					el("td", {}, String(k.hits)),
					el("td", {}, String(k.misses)),
					el("td", {}, String(k.stores)),
					el("td", { class: k.evictions > 0 ? "tone-infra" : "" }, String(k.evictions)),
					el("td", {}, formatPercent(k.hit_rate)),
				)))))));

	frag.appendChild(el("section", { class: "panel" }, el("h2", {}, "Entries"),
		c.entries.length === 0
			? el("p", { class: "empty" }, "No live cache entries.")
			: el("div", { class: "scroll-x" }, el("table", {},
				el("thead", {}, el("tr", {}, ...["Key", "Size", "Ref", "Created", "Last used"].map((h) => el("th", {}, h)))),
				el("tbody", {}, ...c.entries.map((e) => el("tr", {},
					el("td", { class: "mono" }, e.key),
					el("td", {}, formatBytes(e.size_bytes)),
					el("td", { class: "mono" }, e.ref || "—"),
					el("td", {}, relTime(e.created_at)),
					el("td", {}, relTime(e.last_accessed)),
				)))))));

	frag.appendChild(el("section", { class: "panel" }, el("h2", {}, "Recent events"),
		el("div", { class: "scroll-x" }, el("table", {},
			el("thead", {}, el("tr", {}, ...["When", "Kind", "Key", "Detail"].map((h) => el("th", {}, h)))),
			el("tbody", {}, ...c.events.slice(0, 100).map((e) => el("tr", { class: e.kind === "evict" ? "tone-infra" : "" },
				el("td", {}, relTime(e.at)),
				el("td", {}, e.kind),
				el("td", { class: "mono" }, e.key),
				el("td", {}, e.reason || e.matched_on || ""),
			)))))));
	return frag;
}

function stat(label: string, value: string, extra = ""): HTMLElement {
	return el("div", { class: `stat ${extra}`.trim() }, el("span", { class: "stat-value" }, value), el("span", { class: "stat-label" }, label));
}
