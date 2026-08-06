// SGR parser for job logs. Emits styled spans; no DOM.
//
// Carriage returns are honoured the way a terminal does (move to column 0 and
// overwrite), because progress bars otherwise render as one unreadable line.

export interface AnsiStyle {
	fg?: string;
	bg?: string;
	bold?: boolean;
	dim?: boolean;
	italic?: boolean;
	underline?: boolean;
}

export interface AnsiSpan {
	text: string;
	style: AnsiStyle;
}

/** The 16 basic colours resolve to custom properties so themes own the palette. */
function basicColour(index: number): string {
	return `var(--ansi-${index})`;
}

/** xterm 256-colour cube and greyscale ramp. */
export function xterm256(n: number): string {
	if (n < 0 || n > 255) return "inherit";
	if (n < 16) return basicColour(n);
	if (n < 232) {
		const i = n - 16;
		const steps = [0, 95, 135, 175, 215, 255];
		return `rgb(${steps[Math.floor(i / 36) % 6]}, ${steps[Math.floor(i / 6) % 6]}, ${steps[i % 6]})`;
	}
	const v = 8 + (n - 232) * 10;
	return `rgb(${v}, ${v}, ${v})`;
}

function styleKey(s: AnsiStyle): string {
	return [s.fg ?? "", s.bg ?? "", s.bold ? "b" : "", s.dim ? "d" : "", s.italic ? "i" : "", s.underline ? "u" : ""].join("|");
}

/** Applies one SGR parameter list to a style, returning the new style. */
export function applySgr(style: AnsiStyle, params: number[]): AnsiStyle {
	const out: AnsiStyle = { ...style };
	for (let i = 0; i < params.length; i++) {
		const p = params[i];
		if (p === 0) {
			for (const k of Object.keys(out) as (keyof AnsiStyle)[]) delete out[k];
		} else if (p === 1) out.bold = true;
		else if (p === 2) out.dim = true;
		else if (p === 3) out.italic = true;
		else if (p === 4) out.underline = true;
		else if (p === 22) {
			delete out.bold;
			delete out.dim;
		} else if (p === 23) delete out.italic;
		else if (p === 24) delete out.underline;
		else if (p >= 30 && p <= 37) out.fg = basicColour(p - 30);
		else if (p === 39) delete out.fg;
		else if (p >= 40 && p <= 47) out.bg = basicColour(p - 40);
		else if (p === 49) delete out.bg;
		else if (p >= 90 && p <= 97) out.fg = basicColour(p - 90 + 8);
		else if (p >= 100 && p <= 107) out.bg = basicColour(p - 100 + 8);
		else if (p === 38 || p === 48) {
			const target = p === 38 ? "fg" : "bg";
			const mode = params[i + 1];
			if (mode === 5) {
				out[target] = xterm256(params[i + 2] ?? 0);
				i += 2;
			} else if (mode === 2) {
				const r = params[i + 2] ?? 0;
				const g = params[i + 3] ?? 0;
				const b = params[i + 4] ?? 0;
				out[target] = `rgb(${r}, ${g}, ${b})`;
				i += 4;
			} else {
				// An unknown extended-colour mode: consume nothing rather than
				// swallowing the rest of the line as colour parameters.
			}
		}
	}
	return out;
}

/**
 * Parses one line into styled spans. Unknown CSI/OSC sequences are dropped
 * rather than rendered as text.
 */
export function parseAnsi(line: string): AnsiSpan[] {
	const cells: { ch: string; key: string; style: AnsiStyle }[] = [];
	let style: AnsiStyle = {};
	let key = styleKey(style);
	let col = 0;
	let i = 0;

	const put = (ch: string) => {
		cells[col] = { ch, key, style };
		col++;
	};

	while (i < line.length) {
		const c = line[i];
		if (c === "\x1b") {
			const next = line[i + 1];
			if (next === "[") {
				let j = i + 2;
				while (j < line.length && !/[@-~]/.test(line[j])) j++;
				const final = line[j];
				if (final === "m") {
					const body = line.slice(i + 2, j);
					const params = body === "" ? [0] : body.split(";").map((p) => (p === "" ? 0 : parseInt(p, 10) || 0));
					style = applySgr(style, params);
					key = styleKey(style);
				}
				i = j < line.length ? j + 1 : line.length;
				continue;
			}
			if (next === "]") {
				// OSC: runs until BEL or ST.
				let j = i + 2;
				while (j < line.length && line[j] !== "\x07" && !(line[j] === "\x1b" && line[j + 1] === "\\")) j++;
				i = line[j] === "\x1b" ? j + 2 : j + 1;
				continue;
			}
			i += 2;
			continue;
		}
		if (c === "\r") {
			col = 0;
			i++;
			continue;
		}
		if (c === "\n") {
			i++;
			continue;
		}
		put(c);
		i++;
	}

	const spans: AnsiSpan[] = [];
	for (const cell of cells) {
		const c = cell ?? { ch: " ", key: styleKey({}), style: {} };
		const last = spans[spans.length - 1];
		if (last && styleKey(last.style) === c.key) last.text += c.ch;
		else spans.push({ text: c.ch, style: c.style });
	}
	return spans;
}

/** The visible text of a line, escapes removed. Used for searching and anchors. */
export function stripAnsi(line: string): string {
	return parseAnsi(line)
		.map((s) => s.text)
		.join("");
}
