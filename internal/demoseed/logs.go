package demoseed

// The demo's job logs. They carry the things the log viewer exists to handle:
// ::group:: nesting, ANSI colour, stderr interleaved with stdout, and the
// platform's own lines saying what it decided and why.

type logLine struct {
	step   int
	stream string
	text   string
	group  string
}

func out(step int, group, text string) logLine {
	return logLine{step: step, stream: "stdout", text: text, group: group}
}

func errl(step int, group, text string) logLine {
	return logLine{step: step, stream: "stderr", text: text, group: group}
}

// plat is a platform line: the control plane narrating its own work, which is
// what makes setup time and classification visible in the log rather than only
// in the UI chrome.
func plat(step int, group, text string) logLine {
	return logLine{step: step, stream: "platform", text: text, group: group}
}

var buildLog = []logLine{
	plat(1, "", "Job build assigned to runner-a1 (self-hosted, linux, x64)."),
	plat(1, "Set up job", "Creating sandbox: docker:27-dind, private network, 2 CPU, 8 GiB."),
	plat(1, "Set up job", "Image docker:27-dind already present; not pulled."),
	plat(1, "Set up job", "Sandbox ready in 38.4s (image cache warm, workspace volume created)."),
	out(1, "Set up job", "Runner version 0.4.1, workspace /workspace"),
	plat(1, "", "Setup finished. Executing 4 steps."),
	out(2, "actions/checkout@v4", "Syncing repository: acme/widget"),
	out(2, "actions/checkout@v4", "\x1b[36m/usr/bin/git\x1b[0m fetch --no-tags --prune --depth=1 origin +9f3c1ab:refs/remotes/origin/main"),
	out(2, "actions/checkout@v4", "HEAD is now at 9f3c1ab build: pin the toolchain"),
	out(2, "", "Checked out in 4.1s"),
	out(3, "", "$ make build"),
	out(3, "make build", "go build -trimpath -ldflags '-s -w' ./cmd/widget"),
	out(3, "make build", "\x1b[32mok\x1b[0m   acme/widget/internal/api"),
	out(3, "make build", "\x1b[32mok\x1b[0m   acme/widget/internal/store"),
	errl(3, "make build", "\x1b[33mwarning:\x1b[0m module golang.org/x/tools is 3 versions behind"),
	out(3, "make build", "built build/widget-linux-amd64 (18.4 MB)"),
	out(3, "", "make build finished in 2m51s"),
	out(4, "actions/upload-artifact@v4", "Uploading widget-linux-amd64 (18.4 MB) in 3 chunks"),
	out(4, "actions/upload-artifact@v4", "\x1b[32mArtifact widget-linux-amd64 uploaded\x1b[0m (sha256:2b1f0c8d…)"),
	plat(0, "", "Job completed: success. 4 steps ran, none skipped."),
}

var infraLog = []logLine{
	plat(1, "", "Attempt 2 of 3. Attempt 1 failed on an infrastructure fault (cloudflare-524)."),
	plat(1, "Set up job", "Sandbox ready in 40.2s."),
	out(2, "actions/checkout@v4", "HEAD is now at 4d81be0 release: cut v1.14.2"),
	out(3, "docker build", "#5 [builder 3/6] RUN go build -o /out/widget ./cmd/widget"),
	out(3, "docker build", "#5 DONE 182.4s"),
	out(3, "docker build", "\x1b[32m=> exporting layers\x1b[0m 21.7s"),
	out(4, "docker push", "The push refers to repository [registry.example.com/acme/widget]"),
	out(4, "docker push", "a1c9f0d2e6b4: Pushing [==================>    ]  412.9MB/512.1MB"),
	errl(4, "docker push", "\x1b[31merror parsing HTTP 524 response body\x1b[0m: unexpected end of JSON input: \"\""),
	errl(4, "docker push", "failed to push registry.example.com/acme/widget:v1.14.2 after 524s"),
	plat(4, "", "Classified as INFRA by rule cloudflare-524, matching \"error parsing HTTP 524 response body\"."),
	plat(4, "", "A 524 is Cloudflare's origin timeout in front of the registry. Your build produced no failing result."),
	plat(0, "", "Job completed: infra_failure after 2 attempts. This is not reported as a build failure."),
}

// shortLog is what a job with nothing remarkable in it looks like. Every job in
// the demo has a log: a job page with an empty one reads as broken rather than
// as uneventful.
func shortLog(name, last string) []logLine {
	return []logLine{
		plat(1, "", "Job "+name+" assigned to a self-hosted runner."),
		plat(1, "Set up job", "Sandbox ready in 47.9s (image cache warm)."),
		out(2, "actions/checkout@v4", "HEAD is now at 9f3c1ab build: pin the toolchain"),
		out(3, "", "$ "+name),
		out(3, name, "\x1b[32mok\x1b[0m   acme/widget/... (cached)"),
		plat(0, "", last),
	}
}

var cancelledLog = []logLine{
	plat(1, "", "Job build assigned to runner-a1."),
	plat(1, "Set up job", "Sandbox ready in 44.2s."),
	out(2, "actions/checkout@v4", "HEAD is now at 77b0e2c chore: bump deps"),
	out(3, "make build", "go build -trimpath ./cmd/widget"),
	plat(3, "", "Cancellation received: superseded by run #412 on the same branch (concurrency group ci-main)."),
	plat(0, "", "Job completed: cancelled. The sandbox was torn down; no partial artifact was published."),
}

var userFailLog = []logLine{
	plat(1, "", "Job test (1.25, linux) assigned to runner-a2."),
	plat(1, "Set up job", "Sandbox ready in 55.1s."),
	out(2, "actions/checkout@v4", "HEAD is now at c07e5b1 fix: parse negative timeouts"),
	out(3, "go test ./...", "\x1b[32mok\x1b[0m   acme/widget/internal/api\t0.412s"),
	out(3, "go test ./...", "\x1b[32mok\x1b[0m   acme/widget/internal/store\t1.884s"),
	errl(3, "go test ./...", "--- FAIL: TestParseTimeout/negative (0.00s)"),
	errl(3, "go test ./...", "    config_test.go:141: expected an error for \"-5s\", got a duration of -5s"),
	errl(3, "go test ./...", "\x1b[31mFAIL\x1b[0m acme/widget/internal/config\t0.207s"),
	plat(3, "", "No infrastructure rule matched this output; classified as USER."),
	plat(0, "", "Job completed: failure. The failing assertion is annotated on config_test.go:141."),
}
