// Rewrites .github/workflows/ci.yml so every job that set up Google's Go uses
// the go-toolchain-go action and runs its go commands through go-toolchain.
const fs = require("fs");
const path = ".github/workflows/ci.yml";
let s = fs.readFileSync(path, "utf8");

const setup = "      - uses: actions/setup-go@v5\n        with:\n          go-version-file: go.mod\n";
const n = s.split(setup).length - 1;
if (n !== 5) throw new Error(`expected 5 setup-go steps, found ${n}`);
s = s.split(setup).join("      - uses: ./.github/actions/go-toolchain-go\n");

const before = s;
s = s.replace(/run: go (run|test) /g, "run: go-toolchain go $1 ");
if (s === before) throw new Error("no go run/test commands rewritten");
if (/run: go /.test(s)) throw new Error("a plain go command is left");

fs.writeFileSync(path, s);
