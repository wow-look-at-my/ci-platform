import {
	formatBytes,
	formatDuration,
	formatDurationShort,
	formatPercent,
	formatRelative,
	heartbeatStaleness,
	shortSha,
} from "./format.js";
import { assertEqual, test } from "./testing.js";

test("formatDuration: sub-second reads in milliseconds", () => {
	assertEqual(formatDuration(0.25), "250ms");
	assertEqual(formatDuration(0.999), "999ms");
});

test("formatDuration: zero and negatives are 0s, never blank or NaN", () => {
	assertEqual(formatDuration(0), "0s");
	assertEqual(formatDuration(-5), "0s");
	assertEqual(formatDuration(NaN), "0s");
});

test("formatDuration: seconds, minutes, hours", () => {
	assertEqual(formatDuration(1), "1.0s");
	assertEqual(formatDuration(9.5), "9.5s");
	assertEqual(formatDuration(42), "42s");
	assertEqual(formatDuration(60), "1m");
	assertEqual(formatDuration(90), "1m 30s");
	assertEqual(formatDuration(3600), "1h 0m");
	assertEqual(formatDuration(3725), "1h 2m");
});

test("formatDurationShort: always one unit", () => {
	assertEqual(formatDurationShort(45), "45s");
	assertEqual(formatDurationShort(300), "5m");
	assertEqual(formatDurationShort(7200), "2h");
	assertEqual(formatDurationShort(172800), "2d");
});

test("formatBytes", () => {
	assertEqual(formatBytes(0), "0 B");
	assertEqual(formatBytes(512), "512 B");
	assertEqual(formatBytes(1024), "1.0 KiB");
	assertEqual(formatBytes(1536), "1.5 KiB");
	assertEqual(formatBytes(1048576), "1.0 MiB");
});

test("formatRelative: past, future and unparseable", () => {
	const now = Date.parse("2026-03-01T12:00:00Z");
	assertEqual(formatRelative("2026-03-01T11:55:00Z", now), "5m ago");
	assertEqual(formatRelative("2026-03-01T12:00:30Z", now), "in 30s");
	assertEqual(formatRelative(null, now), "never");
	assertEqual(formatRelative("not a date", now), "unknown");
});

test("heartbeatStaleness bands", () => {
	assertEqual(heartbeatStaleness(5), "fresh");
	assertEqual(heartbeatStaleness(29.9), "fresh");
	assertEqual(heartbeatStaleness(30), "stale");
	assertEqual(heartbeatStaleness(119), "stale");
	assertEqual(heartbeatStaleness(120), "dead");
	assertEqual(heartbeatStaleness(3600), "dead");
});

test("shortSha leaves short values alone", () => {
	assertEqual(shortSha("deadbeefcafe"), "deadbee");
	assertEqual(shortSha("abc"), "abc");
});

test("formatPercent", () => {
	assertEqual(formatPercent(0.6667), "66.7%");
	assertEqual(formatPercent(1), "100.0%");
	assertEqual(formatPercent(NaN), "n/a");
});
