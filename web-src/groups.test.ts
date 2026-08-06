import { flattenBlocks, foldGroups, groupLineCount, groupsContaining, groupStartTitle, isGroupEnd, type GroupBlock, type LogLine } from "./groups.js";
import { assert, assertEqual, test } from "./testing.js";

function lines(...texts: string[]): LogLine[] {
	return texts.map((text, i) => ({ seq: i + 1, text }));
}

test("groupStartTitle recognises ::group:: with and without a timestamp prefix", () => {
	assertEqual(groupStartTitle("::group::Install dependencies"), "Install dependencies");
	assertEqual(groupStartTitle("[2026-03-01T12:00:00Z] ::group::Build"), "Build");
	assertEqual(groupStartTitle("not a group"), null);
});

test("isGroupEnd", () => {
	assert(isGroupEnd("::endgroup::"), "plain endgroup");
	assert(isGroupEnd("  ::endgroup::  "), "padded endgroup");
	assert(!isGroupEnd("::endgroup::trailing"), "endgroup must be the whole line");
});

test("a flat log folds to flat blocks", () => {
	const blocks = foldGroups(lines("a", "b"));
	assertEqual(blocks.length, 2);
	assertEqual(blocks[0].kind, "line");
});

test("one group collects its lines and records its range", () => {
	const blocks = foldGroups(lines("before", "::group::Setup", "x", "y", "::endgroup::", "after"));
	assertEqual(blocks.length, 3);
	const g = blocks[1] as GroupBlock;
	assertEqual(g.kind, "group");
	assertEqual(g.title, "Setup");
	assertEqual(g.startSeq, 2);
	assertEqual(g.endSeq, 5);
	assertEqual(g.closed, true);
	assertEqual(groupLineCount(g), 2);
});

test("nested groups nest", () => {
	const blocks = foldGroups(lines("::group::Outer", "a", "::group::Inner", "b", "::endgroup::", "c", "::endgroup::"));
	const outer = blocks[0] as GroupBlock;
	assertEqual(outer.title, "Outer");
	assertEqual(outer.children.length, 3);
	const inner = outer.children[1] as GroupBlock;
	assertEqual(inner.kind, "group");
	assertEqual(inner.title, "Inner");
	assertEqual(groupLineCount(inner), 1);
	assertEqual(groupLineCount(outer), 3);
});

test("a group left open by a dead job is still returned, marked unclosed", () => {
	const blocks = foldGroups(lines("::group::Run tests", "boom"));
	const g = blocks[0] as GroupBlock;
	assertEqual(g.closed, false);
	assertEqual(g.endSeq, 2);
	assertEqual(groupLineCount(g), 1);
});

test("a stray ::endgroup:: is dropped rather than rendered", () => {
	const blocks = foldGroups(lines("a", "::endgroup::", "b"));
	assertEqual(blocks.length, 2);
	assertEqual(flattenBlocks(blocks).map((l) => l.text).join(","), "a,b");
});

test("groupsContaining names every enclosing group, outermost first", () => {
	const blocks = foldGroups(lines("::group::Outer", "::group::Inner", "target", "::endgroup::", "::endgroup::"));
	assertEqual(groupsContaining(blocks, 3).join(","), "1,2");
	assertEqual(groupsContaining(blocks, 99).length, 0);
});

test("flattenBlocks restores the original order without the markers", () => {
	const blocks = foldGroups(lines("a", "::group::G", "b", "c", "::endgroup::", "d"));
	assertEqual(flattenBlocks(blocks).map((l) => l.text).join(""), "abcd");
});
