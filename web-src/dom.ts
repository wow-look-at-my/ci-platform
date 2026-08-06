// Minimal DOM helpers. Everything is created as a node; nothing here ever
// assigns untrusted text through innerHTML.

type Attrs = Record<string, string | number | boolean | ((e: Event) => void) | undefined>;
type Child = Node | string | number | null | undefined | false;

export function el<K extends keyof HTMLElementTagNameMap>(
	tag: K,
	attrs: Attrs = {},
	...children: Child[]
): HTMLElementTagNameMap[K] {
	const node = document.createElement(tag);
	applyAttrs(node, attrs);
	append(node, children);
	return node;
}

const SVG_NS = "http://www.w3.org/2000/svg";

export function svg(tag: string, attrs: Attrs = {}, ...children: Child[]): SVGElement {
	const node = document.createElementNS(SVG_NS, tag);
	for (const [k, v] of Object.entries(attrs)) {
		if (v === undefined || v === false) continue;
		if (typeof v === "function") node.addEventListener(k.replace(/^on/, ""), v as EventListener);
		else node.setAttribute(k, String(v));
	}
	append(node, children);
	return node;
}

function applyAttrs(node: HTMLElement, attrs: Attrs): void {
	for (const [k, v] of Object.entries(attrs)) {
		if (v === undefined || v === false) continue;
		if (typeof v === "function") {
			node.addEventListener(k.replace(/^on/, ""), v as EventListener);
		} else if (k === "class") {
			node.className = String(v);
		} else if (k === "html") {
			// Only ever used with markup this module built itself.
			node.innerHTML = String(v);
		} else if (v === true) {
			node.setAttribute(k, "");
		} else {
			node.setAttribute(k, String(v));
		}
	}
}

function append(node: Node, children: Child[]): void {
	for (const c of children) {
		if (c === null || c === undefined || c === false) continue;
		node.appendChild(typeof c === "object" ? c : document.createTextNode(String(c)));
	}
}

export function clear(node: Element): void {
	while (node.firstChild) node.removeChild(node.firstChild);
}

export function replace(node: Element, ...children: Child[]): void {
	clear(node);
	append(node, children);
}

/** A labelled key/value pair for the detail panels. */
export function kv(label: string, value: Child, extra: Attrs = {}): HTMLElement {
	return el("div", { class: "kv", ...extra }, el("dt", {}, label), el("dd", {}, value));
}

/** An error panel. Errors are shown, never swallowed into an empty view. */
export function errorPanel(where: string, err: unknown): HTMLElement {
	const message = err instanceof Error ? err.message : String(err);
	return el("div", { class: "panel tone-failure", role: "alert" },
		el("h2", {}, `Could not load ${where}`),
		el("p", { class: "mono" }, message),
	);
}

export function spinner(label = "Loading"): HTMLElement {
	return el("div", { class: "loading", "aria-live": "polite" }, label + "…");
}

export function link(href: string, ...children: Child[]): HTMLAnchorElement {
	return el("a", { href }, ...children);
}
