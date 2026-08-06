// ::group:: / ::endgroup:: folding. No DOM.

export interface LogLine {
	seq: number;
	ts?: string;
	step?: number;
	stream?: string;
	text: string;
	group?: string;
}

export interface LineBlock {
	kind: "line";
	line: LogLine;
}

export interface GroupBlock {
	kind: "group";
	title: string;
	/** Sequence of the ::group:: line itself, used as the fold's identity. */
	startSeq: number;
	/** Sequence of the last line inside the group. */
	endSeq: number;
	closed: boolean;
	children: Block[];
}

export type Block = LineBlock | GroupBlock;

const GROUP_START = /^\s*(?:\[[^\]]*\]\s*)?::group::(.*)$/;
const GROUP_END = /^\s*(?:\[[^\]]*\]\s*)?::endgroup::\s*$/;

/** Returns the group title when the line opens a group, else null. */
export function groupStartTitle(text: string): string | null {
	const m = GROUP_START.exec(text);
	return m ? m[1].trim() : null;
}

export function isGroupEnd(text: string): boolean {
	return GROUP_END.test(text);
}

/**
 * Folds a flat line list into a tree. A group left unclosed at the end of the
 * log is still returned, marked `closed: false`, because a job that died
 * mid-group is exactly when the operator needs to see inside it.
 */
export function foldGroups(lines: LogLine[]): Block[] {
	const root: Block[] = [];
	const stack: GroupBlock[] = [];
	const top = () => (stack.length ? stack[stack.length - 1].children : root);

	for (const line of lines) {
		const title = groupStartTitle(line.text);
		if (title !== null) {
			const g: GroupBlock = { kind: "group", title, startSeq: line.seq, endSeq: line.seq, closed: false, children: [] };
			top().push(g);
			stack.push(g);
			continue;
		}
		if (isGroupEnd(line.text)) {
			const g = stack.pop();
			if (g) {
				g.endSeq = line.seq;
				g.closed = true;
			}
			// An ::endgroup:: with no open group is dropped: rendering it as a
			// bare line is noise, and there is nothing to close.
			continue;
		}
		top().push({ kind: "line", line });
		for (const g of stack) g.endSeq = line.seq;
	}
	return root;
}

/**
 * The startSeqs of every group enclosing `seq`, outermost first. The job page
 * expands exactly these when it jumps to a line inside collapsed groups.
 */
export function groupsContaining(blocks: Block[], seq: number): number[] {
	const path: number[] = [];
	const walk = (bs: Block[]): boolean => {
		for (const b of bs) {
			if (b.kind === "line") {
				if (b.line.seq === seq) return true;
				continue;
			}
			if (seq >= b.startSeq && seq <= b.endSeq) {
				path.push(b.startSeq);
				if (walk(b.children)) return true;
				// The seq is inside this group's range but not on a line of its
				// own; keeping the group open is still the right answer.
				return true;
			}
		}
		return false;
	};
	walk(blocks);
	return path;
}

/** Flattens the tree back to lines, in order. */
export function flattenBlocks(blocks: Block[]): LogLine[] {
	const out: LogLine[] = [];
	const walk = (bs: Block[]) => {
		for (const b of bs) {
			if (b.kind === "line") out.push(b.line);
			else walk(b.children);
		}
	};
	walk(blocks);
	return out;
}

/** How many lines a group holds, including nested groups' lines. */
export function groupLineCount(g: GroupBlock): number {
	return flattenBlocks(g.children).length;
}
