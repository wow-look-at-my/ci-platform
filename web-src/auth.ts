// Sign-in against the operator credential.
//
// The API is gated because every job container can reach this server, so the
// UI has to hold a session before it can render anything. Signing in sets an
// HttpOnly cookie server-side; nothing here ever holds the credential after the
// request that exchanges it.

import { el, replace } from "./dom.js";

export interface AuthStatus {
	authenticated: boolean;
}

export async function status(): Promise<boolean> {
	const resp = await fetch("/auth/status", { headers: { Accept: "application/json" } });
	if (!resp.ok) throw new Error(`sign-in status check failed: ${resp.status} ${resp.statusText}`);
	return ((await resp.json()) as AuthStatus).authenticated;
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

	replace(view,
		el("div", { class: "panel signin-panel" },
			el("h1", {}, "Sign in"),
			el("p", {}, "This control plane is reachable from every job container, so its API is closed until you sign in."),
			el("p", { class: "muted" }, "The credential is ", el("code", {}, "CIPLATFORM_OPERATOR_TOKEN"), " from the server's environment."),
			form,
			message,
		),
	);
	input.focus();
}
