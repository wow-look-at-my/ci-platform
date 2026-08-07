// Entry point: chrome, routing table, and per-page teardown.

import { api, onUnauthorized } from "./api.js";
import { logout, renderSignIn, status } from "./auth.js";
import { el, replace } from "./dom.js";
import { notFound, route, start } from "./router.js";
import { applyStoredTheme, themeToggle } from "./widgets.js";
import { renderArtifacts } from "./pages/artifacts.js";
import { renderCache } from "./pages/cache.js";
import { renderJob } from "./pages/job.js";
import { renderQueue } from "./pages/queue.js";
import { renderRun } from "./pages/run.js";
import { renderRunners } from "./pages/runners.js";
import { renderRuns } from "./pages/runs.js";

const NAV = [
	{ href: "#/runs", label: "Runs" },
	{ href: "#/runners", label: "Runners" },
	{ href: "#/queue", label: "Queue" },
];

// Every page registers its timers and streams here so a route change tears them
// down; otherwise auto-refresh timers accumulate for the life of the tab.
let cleanups: (() => void)[] = [];

function resetPage(): HTMLElement {
	for (const fn of cleanups) fn();
	cleanups = [];
	const view = document.getElementById("view")!;
	replace(view);
	return view;
}

const registerTimer = (id: number) => cleanups.push(() => clearInterval(id));
const registerCleanup = (fn: () => void) => cleanups.push(fn);

function chrome(): void {
	const nav = el("nav", { class: "nav" }, ...NAV.map((n) => el("a", { href: n.href }, n.label)));
	const health = el("span", { class: "health", title: "control plane health" }, "…");
	const signOut = el("button", {
		class: "ghost signout",
		title: "forget this browser's operator session",
		onclick: () => {
			void logout().then(() => location.reload());
		},
	}, "Sign out");
	const header = el("header", { class: "topbar" },
		el("a", { class: "brand", href: "#/runs" }, "CI"),
		nav,
		el("div", { class: "spacer" }),
		health,
		themeToggle(),
		signOut,
	);
	document.body.insertBefore(header, document.body.firstChild);

	const paintHealth = async () => {
		try {
			const h = await api.health();
			const degraded = h.subsystems.filter((s) => s.state !== "ok");
			health.className = `health health-${h.status}`;
			health.textContent = h.status === "ok" ? "healthy" : `${h.status}: ${degraded.map((s) => s.name).join(", ")}`;
			health.title = degraded.length ? degraded.map((s) => `${s.name}: ${s.detail || s.state}`).join("\n") : "all subsystems ok";
		} catch (err) {
			health.className = "health health-down";
			health.textContent = "control plane unreachable";
			health.title = err instanceof Error ? err.message : String(err);
		}
	};
	void paintHealth();
	setInterval(paintHealth, 15000);
}

function routes(): void {
	route(/^\/(runs)?\/?$/, (params) => renderRuns(resetPage(), params, registerTimer));
	route(/^\/runs\/(\d+)$/, (params, m) => renderRun(resetPage(), Number(m[1]), params, registerTimer));
	route(/^\/runs\/(\d+)\/artifacts$/, (_p, m) => renderArtifacts(resetPage(), Number(m[1])));
	route(/^\/jobs\/(\d+)$/, (params, m) => renderJob(resetPage(), Number(m[1]), params, registerCleanup));
	route(/^\/runners$/, () => renderRunners(resetPage(), registerTimer));
	route(/^\/queue$/, (params) => renderQueue(resetPage(), params, registerTimer));
	route(/^\/repos\/([^/]+)\/([^/]+)\/cache$/, (_p, m) => renderCache(resetPage(), m[1], m[2]));
	notFound(() => {
		replace(resetPage(), el("div", { class: "panel" },
			el("h1", {}, "No such page"),
			el("p", {}, "Nothing is routed at ", el("code", {}, location.hash || "#/"), "."),
			el("p", {}, el("a", { href: "#/runs" }, "Back to runs"))));
	});
}

// The API is closed to an uncredentialed caller, so the session is checked
// before anything is painted: without it every panel would render the same
// unauthorized error, which reads as a broken control plane rather than as a
// session that ended.
function signIn(): void {
	const view = document.getElementById("view")!;
	for (const fn of cleanups) fn();
	cleanups = [];
	document.querySelector("header.topbar")?.remove();
	renderSignIn(view, () => location.reload());
}

applyStoredTheme();
void status()
	.then((authenticated) => {
		if (!authenticated) {
			signIn();
			return;
		}
		onUnauthorized(signIn);
		chrome();
		routes();
		start();
	})
	.catch((err: unknown) => {
		replace(document.getElementById("view")!,
			el("div", { class: "panel tone-failure", role: "alert" },
				el("h1", {}, "Control plane unreachable"),
				el("p", { class: "mono" }, err instanceof Error ? err.message : String(err))));
	});
