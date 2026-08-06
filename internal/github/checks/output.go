package checks

import (
	"fmt"
	"strings"
	"time"

	"github.com/wow-look-at-my/ci-platform/internal/model"
)

// Output is the rendered check run output block.
type Output struct {
	Title       string       `json:"title"`
	Summary     string       `json:"summary"`
	Text        string       `json:"text,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

// stepTableNote says plainly what a third-party App cannot do. GitHub renders
// its native per-step expander only for its own runner, so the step list is a
// markdown table here and the live view lives at details_url.
const stepTableNote = "GitHub renders its collapsible per-step view only for its own Actions runner, so the steps below are a table. The live view, with logs, is at the details link above."

// Title is the one-line headline of the check run.
func Title(u Update) string {
	if !u.Terminal() {
		return truncate(statusTitle(u.Status), MaxOutputTitle)
	}
	_, prefix := ConclusionToCheck(u.Conclusion)
	return truncate(prefix, MaxOutputTitle)
}

// Summary renders the summary block: the details link first, then attempt,
// classification, cancellation, and timing, then any caller-supplied text.
func Summary(u Update, now time.Time) string {
	var b strings.Builder
	if u.DetailsURL != "" {
		fmt.Fprintf(&b, "Live logs, step timings, and the full job view: %s\n\n", u.DetailsURL)
	}
	if u.Attempt > 0 && u.MaxAttempts > 1 {
		fmt.Fprintf(&b, "Attempt %d of %d.\n\n", u.Attempt, u.MaxAttempts)
	} else if u.Attempt > 1 {
		fmt.Fprintf(&b, "Attempt %d.\n\n", u.Attempt)
	}
	if u.Class != "" && u.Class != model.ClassNone {
		fmt.Fprintf(&b, "Classified as **%s**", classLabel(u.Class))
		if u.ClassReason != "" {
			fmt.Fprintf(&b, ": %s", u.ClassReason)
		}
		b.WriteString(".\n\n")
	}
	if u.Conclusion == model.ConclusionInfraFailure {
		b.WriteString("This is a failure of the CI infrastructure, not of the code under test. It is reported as `action_required` rather than as a red build.\n\n")
	}
	if u.Conclusion == model.ConclusionConfigError {
		b.WriteString("The workflow file itself is wrong; retrying cannot help. Reported as `action_required` rather than as a red build.\n\n")
	}
	if u.Cancel != nil {
		fmt.Fprintf(&b, "Cancelled by **%s**: %s", u.Cancel.Actor, u.Cancel.Sentence)
		if u.Cancel.TriggeredBy != "" {
			fmt.Fprintf(&b, " (triggered by %s)", u.Cancel.TriggeredBy)
		}
		b.WriteString("\n\n")
	}
	if t := u.Timing; t != nil {
		fmt.Fprintf(&b, "Queued %s, setup %s, execute %s (total %s).\n\n",
			formatDuration(t.QueuedFor), formatDuration(t.SetupFor),
			formatDuration(t.ExecuteFor), formatDuration(t.TotalFor))
	}
	if u.Summary != "" {
		b.WriteString(u.Summary)
		if !strings.HasSuffix(u.Summary, "\n") {
			b.WriteString("\n")
		}
	}
	s := strings.TrimRight(b.String(), "\n")
	if s == "" {
		s = Title(u)
	}
	return truncateMarked(s, MaxOutputSummary)
}

func classLabel(c model.FailureClass) string {
	switch c {
	case model.ClassUser:
		return "user"
	case model.ClassInfra:
		return "infrastructure"
	case model.ClassConfig:
		return "workflow configuration"
	}
	return string(c)
}

// StepTable renders the step list as a markdown table.
func StepTable(steps []model.Step, now time.Time) string {
	if len(steps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("| Step | Result | Duration |\n|---|---|---|\n")
	for _, s := range steps {
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("step %d", s.Number)
		}
		b.WriteString("| " + escapeCell(name) + " | " + escapeCell(stepResult(s)) + " | " + formatDuration(s.Duration(now)) + " |\n")
	}
	return b.String()
}

func stepResult(s model.Step) string {
	if s.Status != model.StatusCompleted {
		return statusTitle(s.Status)
	}
	if s.Conclusion == "" {
		return "completed"
	}
	_, prefix := ConclusionToCheck(s.Conclusion)
	if s.ContinueOnError && s.Conclusion.IsFailure() {
		return prefix + " (continue-on-error)"
	}
	return prefix
}

// Text renders the output body: the step table plus the note explaining why it
// is a table.
func Text(u Update, now time.Time) string {
	table := StepTable(u.Steps, now)
	if table == "" {
		return ""
	}
	body := "### Steps\n\n" + table + "\n" + stepTableNote + "\n"
	return truncateMarked(body, MaxOutputText)
}

// Render builds the whole output block for an update, excluding annotations,
// which the reporter chunks separately.
func Render(u Update, now time.Time) Output {
	return Output{Title: Title(u), Summary: Summary(u, now), Text: Text(u, now)}
}

// escapeCell keeps a step name from breaking the table.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// formatDuration renders a duration the way an operator reads it.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// truncate cuts to a byte budget on a rune boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// truncateMarked cuts and says so, because a silently shortened summary is a
// lie about what happened.
func truncateMarked(s string, max int) string {
	const marker = "\n\n[truncated: output exceeded GitHub's limit]"
	if len(s) <= max {
		return s
	}
	return truncate(s, max-len(marker)) + marker
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
