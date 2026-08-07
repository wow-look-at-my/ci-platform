// The demo build's sign-in module: there is no server to sign in to.
//
// The real gate exists because a job container can reach the control plane
// (docs/security.md). A static snapshot has neither, so the demo reports an
// open session rather than showing a credential prompt nothing would accept.

export async function status(): Promise<boolean> {
	return true;
}

export async function login(_token: string): Promise<string | null> {
	return "This is a static demo: there is no session to sign in to.";
}

export async function logout(): Promise<void> {}

export function renderSignIn(_view: HTMLElement, _onSignedIn: () => void): void {
	// Unreachable: status() never reports a signed-out session in the demo.
	// Kept so the module presents the same surface as the one it replaces.
}
