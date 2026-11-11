// Command latency-report summarizes per-stage Response latency from the
// assistant's JSON logs. Pipe logs in, for example:
//
//	docker compose --project-name clanka-latency logs --no-log-prefix assistant \
//	  | go run ./cmd/latency-report
//
// It reports nearest-rank p50/p95 per routed path and flags any path over the
// #12 budgets. Only requests whose reply audio started count as samples.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Jairik/clanka-bot/assistant/internal/latency"
)

func main() {
	source := flag.String("source", string(latency.SourceVoice), "request source to report (voice, debug, or empty for all)")
	file := flag.String("file", "-", "log file to read, or - for stdin")
	flag.Parse()

	reader := os.Stdin
	if *file != "-" {
		opened, err := os.Open(*file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "latency-report:", err)
			os.Exit(2)
		}
		defer opened.Close()
		reader = opened
	}

	records, err := latency.ReadRecords(reader, *source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "latency-report:", err)
		os.Exit(2)
	}
	summary := latency.Summarize(records)
	write(os.Stdout, summary, *source)
}

func write(out io.Writer, summary latency.Summary, source string) {
	label := "all sources"
	if source != "" {
		label = "source=" + source
	}
	fmt.Fprintf(out, "latency report: %d request lines (%s)\n", summary.Records, label)
	if summary.Records == 0 {
		fmt.Fprintln(out, "no request lines found; check the log filter and that requests were made")
		return
	}

	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	anyAgentToken := false
	for _, path := range summary.Paths {
		fmt.Fprintf(writer, "\n%s\t%d attempts\t%s\n", path.Path, path.Attempts, outcomes(path))
		fmt.Fprintf(writer, "  response\t%d samples\t%s", path.ResponseCount, stat(path.Response))
		if budget, ok := latency.BudgetFor(path.Path); ok {
			overP50, overP95 := path.Over(budget)
			fmt.Fprintf(writer, "\tbudget p50 %s / p95 %s\t%s",
				millis(budget.P50), millis(budget.P95), overFlags(overP50, overP95))
		} else {
			fmt.Fprint(writer, "\t(no budget)")
		}
		fmt.Fprintln(writer)

		names := make([]string, 0, len(path.Stages))
		for name := range path.Stages {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == string(latency.StageAgentFirstToken)+"_ms" {
				continue // reported below with the model-choice flag
			}
			stage := path.Stages[name]
			missing := ""
			if path.ResponseCount > 0 && stage.N < path.ResponseCount {
				missing = fmt.Sprintf("(missing on %d)", path.ResponseCount-stage.N)
			}
			fmt.Fprintf(writer, "  %s\t%d samples\t%s\t%s\n", trimStage(name), stage.N, stat(stage), missing)
		}
		if path.FirstChunk.N > 0 {
			fmt.Fprintf(writer, "  first_chunk\t%d samples\t%s\tincluded in response\n",
				path.FirstChunk.N, stat(path.FirstChunk))
		}
		if path.AgentToken.N > 0 {
			anyAgentToken = true
			fmt.Fprintf(writer, "  agent_first_token\t%d samples\t%s\t%s\n",
				path.AgentToken.N, stat(path.AgentToken), agentTokenFlag(path.AgentToken))
		}
	}
	writer.Flush()

	if !anyAgentToken {
		fmt.Fprintln(out, "\nagent first token: no Agent path samples (the Agent path was not exercised)")
	}
	fmt.Fprintln(out, "\nResponse latency is last voiced frame to first actual reply sample; chimes and fillers are excluded.")
	fmt.Fprintln(out, "Only ok requests with reply audio are sampled; failed, cancelled and audio-less requests are counted separately.")
}

func outcomes(path latency.PathSummary) string {
	names := make([]string, 0, len(path.Outcomes))
	for name := range path.Outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	text := ""
	for i, name := range names {
		if i > 0 {
			text += ", "
		}
		text += fmt.Sprintf("%s=%d", name, path.Outcomes[name])
	}
	return text
}

func stat(value latency.Stat) string {
	if value.N == 0 {
		return "not measured"
	}
	return fmt.Sprintf("p50 %s  p95 %s", millis(value.P50), millis(value.P95))
}

func millis(value float64) string {
	return time.Duration(value * float64(time.Millisecond)).Round(time.Millisecond).String()
}

func overFlags(overP50, overP95 bool) string {
	switch {
	case overP50 && overP95:
		return "OVER p50 and p95"
	case overP50:
		return "OVER p50"
	case overP95:
		return "OVER p95"
	default:
		return "within budget"
	}
}

func agentTokenFlag(token latency.Stat) string {
	if token.P50 > latency.AgentFirstTokenP50 {
		return fmt.Sprintf("OVER %s: flag the model choice first", millis(latency.AgentFirstTokenP50))
	}
	return fmt.Sprintf("within %s", millis(latency.AgentFirstTokenP50))
}

// trimStage drops the _ms suffix the log uses so the table reads as stages.
func trimStage(name string) string {
	if len(name) > 3 && name[len(name)-3:] == "_ms" {
		return name[:len(name)-3]
	}
	return name
}
