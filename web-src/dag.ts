// Layered layout for the run page's job DAG. No DOM: this computes geometry,
// the run page turns it into SVG.

export interface DagInput {
	key: string;
	name?: string;
	needs?: string[];
}

export interface LayoutOptions {
	nodeWidth?: number;
	nodeHeight?: number;
	columnGap?: number;
	rowGap?: number;
	padding?: number;
}

export interface PlacedNode {
	key: string;
	name: string;
	column: number;
	row: number;
	x: number;
	y: number;
	width: number;
	height: number;
}

export interface PlacedEdge {
	from: string;
	to: string;
	/** Cubic bezier, left edge of the source box to the right edge of the target. */
	path: string;
}

export interface DagLayout {
	nodes: PlacedNode[];
	edges: PlacedEdge[];
	width: number;
	height: number;
	columns: number;
}

const DEFAULTS: Required<LayoutOptions> = {
	nodeWidth: 180,
	nodeHeight: 44,
	columnGap: 56,
	rowGap: 16,
	padding: 16,
};

/**
 * Longest-path depth per node. A `needs` entry naming a job that is not in the
 * set is ignored; a cycle (which the workflow loader rejects long before here)
 * stops relaxing rather than looping.
 */
export function computeDepths(nodes: DagInput[]): Map<string, number> {
	const present = new Set(nodes.map((n) => n.key));
	const depth = new Map<string, number>(nodes.map((n) => [n.key, 0]));
	for (let pass = 0; pass < nodes.length; pass++) {
		let changed = false;
		for (const n of nodes) {
			let d = 0;
			for (const need of n.needs ?? []) {
				if (!present.has(need)) continue;
				d = Math.max(d, (depth.get(need) ?? 0) + 1);
			}
			if (d > (depth.get(n.key) ?? 0)) {
				depth.set(n.key, d);
				changed = true;
			}
		}
		if (!changed) break;
	}
	return depth;
}

/** Places nodes in depth columns, preserving input order within a column. */
export function layoutDag(nodes: DagInput[], options: LayoutOptions = {}): DagLayout {
	const o = { ...DEFAULTS, ...options };
	const depth = computeDepths(nodes);
	const rowOf = new Map<string, number>();
	const perColumn = new Map<number, number>();

	const placed: PlacedNode[] = nodes.map((n) => {
		const column = depth.get(n.key) ?? 0;
		const row = perColumn.get(column) ?? 0;
		perColumn.set(column, row + 1);
		rowOf.set(n.key, row);
		return {
			key: n.key,
			name: n.name ?? n.key,
			column,
			row,
			x: o.padding + column * (o.nodeWidth + o.columnGap),
			y: o.padding + row * (o.nodeHeight + o.rowGap),
			width: o.nodeWidth,
			height: o.nodeHeight,
		};
	});

	const byKey = new Map(placed.map((p) => [p.key, p]));
	const edges: PlacedEdge[] = [];
	for (const n of nodes) {
		for (const need of n.needs ?? []) {
			const a = byKey.get(need);
			const b = byKey.get(n.key);
			if (!a || !b) continue;
			const x1 = a.x + a.width;
			const y1 = a.y + a.height / 2;
			const x2 = b.x;
			const y2 = b.y + b.height / 2;
			const mid = (x2 - x1) / 2;
			edges.push({ from: need, to: n.key, path: `M ${x1} ${y1} C ${x1 + mid} ${y1}, ${x2 - mid} ${y2}, ${x2} ${y2}` });
		}
	}

	const columns = perColumn.size;
	const maxRows = Math.max(0, ...perColumn.values());
	return {
		nodes: placed,
		edges,
		columns,
		width: columns === 0 ? 0 : o.padding * 2 + columns * o.nodeWidth + (columns - 1) * o.columnGap,
		height: maxRows === 0 ? 0 : o.padding * 2 + maxRows * o.nodeHeight + (maxRows - 1) * o.rowGap,
	};
}
