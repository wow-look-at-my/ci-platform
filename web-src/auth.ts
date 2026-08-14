// Sign-in against the operator credential.
//
// The API is gated because every job container can reach this server, so the
// UI has to hold a session before it can render anything. Signing in sets an
// HttpOnly cookie server-side; nothing here ever holds the credential after the
// request that exchanges it.

import { el, replace } from "./dom.js";

export interface AuthStatus {
	authenticated: boolean;
	/** The GitHub account signed in, empty for a shared-credential session. */
	login?: string;
	method?: "github" | "token";
	sign_in_url?: string;
}

export async function status(): Promise<AuthStatus> {
	const resp = await fetch("/auth/status", { headers: { Accept: "application/json" } });
	if (!resp.ok) throw new Error(`sign-in status check failed: ${resp.status} ${resp.statusText}`);
	return (await resp.json()) as AuthStatus;
}

/** Exchanges the credential for a session cookie. Returns the server's message on refusal. */
export async function login(token: string): Promise<string | null> {
	const resp = await fetch("/auth/login", {
		method: "POST",
		headers: { "Content-Type": "application/json", Accept: "application/json" },
		body: JSON.stringify({ token }),
	});
	if (resp.ok) return null;
	const body = (await resp.json().catch(() => ({}))) as { message?: string };
	return body.message ?? `${resp.status} ${resp.statusText}`;
}

export async function logout(): Promise<void> {
	await fetch("/auth/logout", { method: "POST" });
}

/**
 * Renders the sign-in form. onSignedIn runs once the cookie is set; the caller
 * decides what to do next, which is a full reload so no page is left holding
 * state from before the session existed.
 */
export function renderSignIn(view: HTMLElement, onSignedIn: () => void): void {
	const message = el("p", { class: "mono danger", role: "alert" });
	message.hidden = true;

	const input = el("input", {
		type: "password",
		id: "operator-token",
		autocomplete: "current-password",
		placeholder: "operator credential",
		required: true,
	}) as HTMLInputElement;

	const submit = el("button", { type: "submit", class: "primary" }, "Sign in") as HTMLButtonElement;

	const form = el("form", {
		class: "signin",
		onsubmit: (e: Event) => {
			e.preventDefault();
			message.hidden = true;
			submit.disabled = true;
			void login(input.value)
				.then((err) => {
					if (err === null) {
						onSignedIn();
						return;
					}
					message.textContent = err;
					message.hidden = false;
					input.select();
				})
				.catch((err: unknown) => {
					message.textContent = err instanceof Error ? err.message : String(err);
					message.hidden = false;
				})
				.finally(() => {
					submit.disabled = false;
				});
		},
	},
		el("label", { for: "operator-token" }, "Operator credential"),
		input,
		submit,
	);

	// The GitHub button is a link, not a fetch: the flow is a navigation to
	// github.com and back, and an XHR cannot leave the origin.
	const github = el("a", { class: "button primary signin-github", href: GITHUB_SIGN_IN },
		githubMark(), "Sign in with GitHub");

	replace(view,
		el("div", { class: "panel signin-panel" },
			el("h1", {}, "Sign in"),
			el("p", {}, "This control plane is reachable from every job container, so its API is closed until you sign in."),
			github,
			el("p", { class: "muted" }, "Only the accounts in ", el("code", {}, "CIPLATFORM_ADMIN_LOGINS"), " are let in."),
			el("details", { class: "signin-fallback" },
				el("summary", {}, "Use the operator credential instead"),
				el("p", { class: "muted" }, "For a script, or a browser that cannot reach GitHub. The credential is ",
					el("code", {}, "CIPLATFORM_OPERATOR_TOKEN"), " from the server's environment; it names nobody, so an "
					+ "action taken with it is recorded as “operator”."),
				form,
				message,
			),
		),
	);
}

const GITHUB_SIGN_IN = "/auth/github/login";

/** The GitHub mark, inline so the page pulls nothing from another origin. */
function githubMark(): SVGElement {
	const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
	svg.setAttribute("viewBox", "0 0 16 16");
	svg.setAttribute("width", "16");
	svg.setAttribute("height", "16");
	svg.setAttribute("aria-hidden", "true");
	const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
	path.setAttribute("fill", "currentColor");
	path.setAttribute("d", "M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 "
		+ "0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 "
		+ "1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 "
		+ "0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c"
		+ "1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73"
		+ ".54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z");
	svg.appendChild(path);
	return svg;
}
