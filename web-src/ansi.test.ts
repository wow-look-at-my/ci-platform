import { applySgr, parseAnsi, stripAnsi, xterm256 } from "./ansi.js";
import { assert, assertDeepEqual, assertEqual, test } from "./testing.js";

const ESC = "\x1b";

test("plain text is one unstyled span", () => {
	assertDeepEqual(parseAnsi("hello"), [{ text: "hello", style: {} }]);
});

test("basic foreground colours 30-37", () => {
	const spans = parseAnsi(`${ESC}[31mred${ESC}[0m plain`);
	assertEqual(spans.length, 2);
	assertEqual(spans[0].text, "red");
	assertEqual(spans[0].style.fg, "var(--ansi-1)");
	assertEqual(spans[1].text, " plain");
	assertEqual(spans[1].style.fg, undefined);
});

test("bright foreground 90-97 maps to the top half of the palette", () => {
	assertEqual(parseAnsi(`${ESC}[91mx`)[0].style.fg, "var(--ansi-9)");
	assertEqual(parseAnsi(`${ESC}[97mx`)[0].style.fg, "var(--ansi-15)");
});

test("background colours 40-47 and 100-107", () => {
	assertEqual(parseAnsi(`${ESC}[42mx`)[0].style.bg, "var(--ansi-2)");
	assertEqual(parseAnsi(`${ESC}[104mx`)[0].style.bg, "var(--ansi-12)");
});

test("attributes 1/2/3/4 and their resets 22/23/24", () => {
	let s = applySgr({}, [1, 2, 3, 4]);
	assertEqual(s.bold, true);
	assertEqual(s.dim, true);
	assertEqual(s.italic, true);
	assertEqual(s.underline, true);
	s = applySgr(s, [22]);
	assertEqual(s.bold, undefined);
	assertEqual(s.dim, undefined);
	s = applySgr(s, [23, 24]);
	assertEqual(s.italic, undefined);
	assertEqual(s.underline, undefined);
});

test("39 and 49 reset only the colour they own", () => {
	let s = applySgr({}, [31, 42, 1]);
	s = applySgr(s, [39]);
	assertEqual(s.fg, undefined);
	assertEqual(s.bg, "var(--ansi-2)");
	assertEqual(s.bold, true);
	s = applySgr(s, [49]);
	assertEqual(s.bg, undefined);
});

test("0 clears everything", () => {
	const s = applySgr({ fg: "x", bg: "y", bold: true, underline: true }, [0]);
	assertDeepEqual(s, {});
});

test("256-colour 38;5;n", () => {
	assertEqual(parseAnsi(`${ESC}[38;5;196mx`)[0].style.fg, "rgb(255, 0, 0)");
	assertEqual(parseAnsi(`${ESC}[48;5;21mx`)[0].style.bg, "rgb(0, 0, 255)");
	// The first 16 of the 256 palette are the theme's own colours.
	assertEqual(xterm256(3), "var(--ansi-3)");
	// Greyscale ramp.
	assertEqual(xterm256(232), "rgb(8, 8, 8)");
	assertEqual(xterm256(255), "rgb(238, 238, 238)");
});

test("truecolour 38;2;r;g;b", () => {
	assertEqual(parseAnsi(`${ESC}[38;2;12;34;56mx`)[0].style.fg, "rgb(12, 34, 56)");
	assertEqual(parseAnsi(`${ESC}[48;2;1;2;3mx`)[0].style.bg, "rgb(1, 2, 3)");
});

test("combined parameters in one escape", () => {
	const s = parseAnsi(`${ESC}[1;4;31mboom`)[0].style;
	assertEqual(s.bold, true);
	assertEqual(s.underline, true);
	assertEqual(s.fg, "var(--ansi-1)");
});

test("bare ESC[m is a reset", () => {
	const spans = parseAnsi(`${ESC}[31ma${ESC}[mb`);
	assertEqual(spans[1].style.fg, undefined);
});

test("non-SGR CSI sequences are dropped, not rendered", () => {
	assertEqual(stripAnsi(`${ESC}[2Kclean${ESC}[1A`), "clean");
	assertEqual(stripAnsi(`${ESC}]0;a window title\x07text`), "text");
});

test("carriage return overwrites from column zero", () => {
	assertEqual(stripAnsi("50%\r100%"), "100%");
	assertEqual(stripAnsi("abcdef\rXY"), "XYcdef");
});

test("carriage return keeps the style of each written cell", () => {
	const spans = parseAnsi(`${ESC}[31mABCDEF\r${ESC}[32mXY`);
	assertEqual(stripAnsi(`${ESC}[31mABCDEF\r${ESC}[32mXY`), "XYCDEF");
	assertEqual(spans[0].text, "XY");
	assertEqual(spans[0].style.fg, "var(--ansi-2)");
	assertEqual(spans[1].text, "CDEF");
	assertEqual(spans[1].style.fg, "var(--ansi-1)");
});

test("adjacent identical styles merge into one span", () => {
	const spans = parseAnsi(`${ESC}[31mab${ESC}[31mcd`);
	assertEqual(spans.length, 1);
	assertEqual(spans[0].text, "abcd");
});

test("an unterminated escape does not leak into the text", () => {
	assert(!stripAnsi(`text${ESC}[31`).includes(ESC), "escape character survived");
});
