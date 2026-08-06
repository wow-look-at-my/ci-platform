// Hash routing. Deep-linkable, and the log-line anchor rides in the same hash
// as a second segment: #/jobs/12?attempt=2#L840 is not expressible, so line
// anchors are a query parameter of the route instead: #/jobs/12?line=840.

export interface Route {
	path: string;
	params: URLSearchParams;
}

export type RouteHandler = (params: URLSearchParams, match: RegExpExecArray) => void;

interface Entry {
	pattern: RegExp;
	handler: RouteHandler;
}

const entries: Entry[] = [];
let fallback: RouteHandler | null = null;

export function route(pattern: RegExp, handler: RouteHandler): void {
	entries.push({ pattern, handler });
}

export function notFound(handler: RouteHandler): void {
	fallback = handler;
}

export function parseHash(hash: string): Route {
	const raw = hash.replace(/^#/, "") || "/";
	const [path, qs] = raw.split("?");
	return { path: path || "/", params: new URLSearchParams(qs ?? "") };
}

export function navigate(path: string, params: Record<string, string | number | undefined> = {}): void {
	const q = new URLSearchParams();
	for (const [k, v] of Object.entries(params)) {
		if (v !== undefined && v !== "") q.set(k, String(v));
	}
	const qs = q.toString();
	location.hash = `#${path}${qs ? `?${qs}` : ""}`;
}

/** Replaces the query part without adding a history entry. */
export function replaceParams(params: Record<string, string | number | undefined>): void {
	const current = parseHash(location.hash);
	for (const [k, v] of Object.entries(params)) {
		if (v === undefined || v === "") current.params.delete(k);
		else current.params.set(k, String(v));
	}
	const qs = current.params.toString();
	history.replaceState(null, "", `#${current.path}${qs ? `?${qs}` : ""}`);
}

export function currentRoute(): Route {
	return parseHash(location.hash);
}

export function dispatch(): void {
	const { path, params } = currentRoute();
	for (const e of entries) {
		const m = e.pattern.exec(path);
		if (m) {
			e.handler(params, m);
			return;
		}
	}
	fallback?.(params, [] as unknown as RegExpExecArray);
}

export function start(): void {
	addEventListener("hashchange", dispatch);
	dispatch();
}
