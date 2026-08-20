package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"baton/internal/docker"
	"baton/internal/store"
)

// Report is the machine-readable view of one container. The menu bar app reads
// this, so the shape is part of baton's interface.
type Report struct {
	Container string        `json:"container"`
	Running   bool          `json:"running"`
	Holder    *HolderReport `json:"holder"`
	Serving   string        `json:"serving,omitempty"`
	Status    string        `json:"status,omitempty"`
	Drifted   bool          `json:"drifted"`
	Queue     []QueueEntry  `json:"queue"`
	Notes     []string      `json:"notes,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// HolderReport describes whoever currently owns a container.
type HolderReport struct {
	Label     string `json:"label"`
	Tree      string `json:"tree"`
	Kind      string `json:"kind"`
	HeldFor   string `json:"heldFor"`
	Remaining string `json:"remaining,omitempty"`
	Pinned    bool   `json:"pinned"`
	Note      string `json:"note,omitempty"`
}

// QueueEntry is one session waiting its turn.
type QueueEntry struct {
	Position int    `json:"position"`
	Label    string `json:"label"`
	Tree     string `json:"tree"`
	Waiting  string `json:"waiting"`
}

func runStatus(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "emit machine-readable output")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}
	return report(firstArgument(positionals), *asJSON, false, stdout, stderr)
}

func runLine(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("line", flag.ContinueOnError)
	flags.SetOutput(stderr)
	asJSON := flags.Bool("json", false, "emit machine-readable output")
	positionals, err := parseArguments(flags, arguments)
	if err != nil {
		return exitError
	}
	return report(firstArgument(positionals), *asJSON, true, stdout, stderr)
}

func report(only string, asJSON, queueOnly bool, stdout, stderr io.Writer) int {
	stateStore, err := openStore()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}
	state, err := stateStore.Read()
	if err != nil {
		fmt.Fprintf(stderr, "baton: %v\n", err)
		return exitError
	}

	names := state.Names()
	if only != "" {
		names = []string{only}
	}
	if len(names) == 0 {
		if asJSON {
			fmt.Fprintln(stdout, "[]")
		} else {
			fmt.Fprintln(stdout, "baton: nothing is being tracked yet")
		}
		return exitOK
	}

	reports := make([]Report, 0, len(names))
	for _, name := range names {
		reports = append(reports, buildReport(state, name, time.Now()))
	}

	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(reports); err != nil {
			fmt.Fprintf(stderr, "baton: %v\n", err)
			return exitError
		}
		return exitOK
	}

	for index, entry := range reports {
		if index > 0 {
			fmt.Fprintln(stdout)
		}
		writeReport(stdout, entry, queueOnly)
	}
	return exitOK
}

func buildReport(state *store.State, name string, now time.Time) Report {
	containerState := state.Get(name)
	entry := Report{Container: name, Queue: []QueueEntry{}}

	if containerState.Holder != nil {
		holder := containerState.Holder
		entry.Holder = &HolderReport{
			Label:   holder.Label,
			Tree:    holder.Tree,
			Kind:    string(holder.Kind),
			HeldFor: shortDuration(now.Sub(holder.Since)),
			Pinned:  holder.Pinned(),
			Note:    holder.Note,
		}
		if !holder.Expires.IsZero() {
			entry.Holder.Remaining = shortDuration(holder.Expires.Sub(now))
		}
	}

	for index, waiter := range containerState.Queue {
		entry.Queue = append(entry.Queue, QueueEntry{
			Position: index + 1,
			Label:    waiter.Label,
			Tree:     waiter.Tree,
			Waiting:  shortDuration(now.Sub(waiter.Since)),
		})
	}

	// Live container facts are best-effort. baton stays useful for reading the
	// queue even when Docker is down.
	container, err := docker.Inspect(name)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	entry.Running = container.Running
	entry.Notes = container.Notes()

	serving, status := container.Serving()
	entry.Status = status
	if serving != "" {
		entry.Serving = prettyContainerPath(serving, container.CodeMount)
	}
	if entry.Holder != nil && serving != "" {
		if wanted, err := container.ContainerPath(entry.Holder.Tree); err == nil {
			entry.Drifted = wanted != serving || status != "ready"
		}
	}
	return entry
}

func writeReport(out io.Writer, entry Report, queueOnly bool) {
	fmt.Fprintf(out, "%s\n", entry.Container)

	if !queueOnly {
		switch {
		case entry.Holder == nil:
			fmt.Fprintf(out, "  holder    free\n")
		case entry.Holder.Pinned:
			line := fmt.Sprintf("  holder    %-22s held by hand for %s", entry.Holder.Label, entry.Holder.HeldFor)
			if entry.Holder.Note != "" {
				line += " — " + entry.Holder.Note
			}
			fmt.Fprintln(out, line)
		default:
			fmt.Fprintf(out, "  holder    %-22s %s in, %s left\n",
				entry.Holder.Label, entry.Holder.HeldFor, entry.Holder.Remaining)
		}

		switch {
		case entry.Error != "":
			fmt.Fprintf(out, "  serving   unknown — %s\n", firstLine(entry.Error))
		case !entry.Running:
			fmt.Fprintf(out, "  serving   container is not running\n")
		case entry.Serving == "":
			fmt.Fprintf(out, "  serving   no supervisor installed (run `baton init %s`)\n", entry.Container)
		default:
			suffix := entry.Status
			if entry.Drifted {
				suffix += "  ** not the holder's tree **"
			}
			fmt.Fprintf(out, "  serving   %-22s %s\n", entry.Serving, suffix)
		}
	}

	// Notes are things that change how results should be read — an applied
	// migration, a schema ahead of the branch. They matter more than the queue,
	// so they go above it.
	for index, note := range entry.Notes {
		label := "  note     "
		if index > 0 {
			label = strings.Repeat(" ", len(label))
		}
		fmt.Fprintf(out, "%s %s\n", label, note)
	}

	if len(entry.Queue) == 0 {
		fmt.Fprintf(out, "  queue     empty\n")
		return
	}
	// The trailing space in the format string is what lines the entries up with
	// the holder and serving rows above.
	for index, waiter := range entry.Queue {
		label := "  queue     "
		if index > 0 {
			label = strings.Repeat(" ", len(label))
		}
		fmt.Fprintf(out, "%s%d. %-19s waiting %s\n", label, waiter.Position, waiter.Label, waiter.Waiting)
	}
}

// prettyContainerPath trims the container's code prefix so status output reads
// as worktree names rather than absolute paths.
func prettyContainerPath(containerPath, codeMount string) string {
	if codeMount == "" {
		codeMount = "/code"
	}
	trimmed := strings.TrimPrefix(containerPath, codeMount)
	trimmed = strings.TrimPrefix(trimmed, "/.worktrees/")
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return "main"
	}
	return filepath.Base(trimmed)
}

func shortDuration(duration time.Duration) string {
	if duration < 0 {
		return "0s"
	}
	duration = duration.Round(time.Second)

	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60
	seconds := int(duration.Seconds()) % 60

	switch {
	case hours > 0:
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}
