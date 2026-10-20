// The demo build's sign-in module: there is no server to sign in to.

export interface AuthStatus {
	authenticated: boolean;
	login?: string;
	method?: "github" | "token";
	sign_in_url?: string;
}

export async function status(): Promise<AuthStatus> {
	return { authenticated: true, login: "demo", method: "github" };
}

export async function login(_token: string): Promise<string | null> {
	return "This is a static demo: there is no session to sign in to.";
}

export async function logout(): Promise<void> {}

export function renderSignIn(_view: HTMLElement, _onSignedIn: () => void): void {
}
