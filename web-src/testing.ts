// A ~60-line test harness, because the UI ships with no runtime dependencies
// and that includes its test runner.

interface Case {
	name: string;
	fn: () => void;
}

const cases: Case[] = [];

export function test(name: string, fn: () => void): void {
	cases.push({ name, fn });
}

export function assert(cond: unknown, message: string): void {
	if (!cond) throw new Error(message);
}

export function assertEqual<T>(got: T, want: T, message = ""): void {
	if (!Object.is(got, want)) {
		throw new Error(`${message ? message + ": " : ""}got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
	}
}

export function assertDeepEqual(got: unknown, want: unknown, message = ""): void {
	const a = JSON.stringify(got);
	const b = JSON.stringify(want);
	if (a !== b) throw new Error(`${message ? message + ": " : ""}\n  got  ${a}\n  want ${b}`);
}

export function assertThrows(fn: () => void, message = "expected a throw"): void {
	try {
		fn();
	} catch {
		return;
	}
	throw new Error(message);
}

/** Runs every registered case, printing TAP-ish output. Returns the exit code. */
export function runAll(): number {
	let failed = 0;
	for (const c of cases) {
		try {
			c.fn();
			console.log(`ok   ${c.name}`);
		} catch (err) {
			failed++;
			console.log(`FAIL ${c.name}`);
			console.log(`     ${err instanceof Error ? err.message : String(err)}`);
		}
	}
	console.log(`\n${cases.length - failed}/${cases.length} passed`);
	return failed === 0 ? 0 : 1;
}
