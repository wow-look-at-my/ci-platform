import { computeDepths, layoutDag } from "./dag.js";
import { assert, assertEqual, test } from "./testing.js";

const diamond = [
	{ key: "build" },
	{ key: "test", needs: ["build"] },
	{ key: "lint", needs: ["build"] },
	{ key: "deploy", needs: ["test", "lint"] },
];

test("computeDepths is the longest path, not the shortest", () => {
	const d = computeDepths([{ key: "a" }, { key: "b", needs: ["a"] }, { key: "c", needs: ["a", "b"] }]);
	assertEqual(d.get("a"), 0);
	assertEqual(d.get("b"), 1);
	assertEqual(d.get("c"), 2);
});

test("computeDepths ignores a needs entry that names no job in the set", () => {
	const d = computeDepths([{ key: "a", needs: ["ghost"] }]);
	assertEqual(d.get("a"), 0);
});

test("computeDepths terminates on a cycle instead of looping", () => {
	const d = computeDepths([{ key: "a", needs: ["b"] }, { key: "b", needs: ["a"] }]);
	assert(d.get("a")! >= 0 && d.get("b")! >= 0, "depths were produced");
});

test("layout puts each depth in its own column", () => {
	const l = layoutDag(diamond);
	const col = new Map(l.nodes.map((n) => [n.key, n.column]));
	assertEqual(col.get("build"), 0);
	assertEqual(col.get("test"), 1);
	assertEqual(col.get("lint"), 1);
	assertEqual(col.get("deploy"), 2);
	assertEqual(l.columns, 3);
});

test("rows stack within a column, in input order", () => {
	const l = layoutDag(diamond);
	const byKey = new Map(l.nodes.map((n) => [n.key, n]));
	assertEqual(byKey.get("test")!.row, 0);
	assertEqual(byKey.get("lint")!.row, 1);
	assert(byKey.get("lint")!.y > byKey.get("test")!.y, "the second row sits below the first");
});

test("geometry honours the supplied box and gap sizes", () => {
	const l = layoutDag(diamond, { nodeWidth: 100, nodeHeight: 20, columnGap: 10, rowGap: 5, padding: 0 });
	const byKey = new Map(l.nodes.map((n) => [n.key, n]));
	assertEqual(byKey.get("build")!.x, 0);
	assertEqual(byKey.get("test")!.x, 110);
	assertEqual(byKey.get("deploy")!.x, 220);
	assertEqual(byKey.get("lint")!.y, 25);
	assertEqual(l.width, 320);
	assertEqual(l.height, 45);
});

test("edges exist for every resolvable needs entry and are drawable paths", () => {
	const l = layoutDag(diamond);
	assertEqual(l.edges.length, 4);
	for (const e of l.edges) {
		assert(e.path.startsWith("M "), `edge ${e.from}->${e.to} has no move command`);
		assert(e.path.includes(" C "), `edge ${e.from}->${e.to} is not a curve`);
	}
});

test("an edge to a job outside the set is dropped, not drawn to nowhere", () => {
	const l = layoutDag([{ key: "a", needs: ["ghost"] }]);
	assertEqual(l.edges.length, 0);
});

test("an empty DAG has zero size rather than a stray padding box", () => {
	const l = layoutDag([]);
	assertEqual(l.width, 0);
	assertEqual(l.height, 0);
	assertEqual(l.nodes.length, 0);
});
