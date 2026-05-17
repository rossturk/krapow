// Package tui presents a bubbletea-driven phase tracker for long-running
// krapow operations (init linux / init win). Falls back to plain text when
// stdout isn't a TTY.
//
// Usage:
//
//	r := tui.New("win-runner-ko22s8", []tui.PhaseSpec{
//	    {ID: "token",   Label: "token"},
//	    {ID: "profile", Label: "profile"},
//	    ...
//	})
//	go func() {
//	    r.Start("token")
//	    err := requestToken()
//	    r.End("token", err)
//	    ...
//	    r.Finish(finalErr)
//	}()
//	if err := r.Run(); err != nil { ... }
//
// Logger() returns an io.Writer that line-buffers subprocess output and
// streams it into the TUI's viewport.
package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

const maxLogLines = 5

// ---------- public API ----------

type PhaseSpec struct {
	ID    string // stable id used in Start/End calls
	Label string // displayed name (kept short)
}

// New returns a Runner. If Plain is true (or stdout isn't a TTY), output is
// printed as plain lines instead of redrawing a TUI.
func New(title string, phases []PhaseSpec, plain bool) *Runner {
	r := &Runner{
		title:  title,
		phases: phases,
		plain:  plain || !isTerminal(),
		starts: map[string]time.Time{},
		done:   make(chan struct{}),
	}
	if r.plain {
		return r
	}
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styleSpinner
	r.model = model{
		title:   title,
		spinner: s,
		startAt: time.Now(),
	}
	for _, p := range phases {
		r.model.phases = append(r.model.phases, phaseState{spec: p, status: statusPending})
	}
	r.prog = tea.NewProgram(&r.model, tea.WithOutput(os.Stderr))
	return r
}

type Runner struct {
	title    string
	phases   []PhaseSpec
	plain    bool
	prog     *tea.Program
	model    model
	starts   map[string]time.Time
	mu       sync.Mutex
	done     chan struct{}
	doneOnce sync.Once
}

// Run blocks until Finish() has been called. Returns the final error (if any).
// Same contract in plain mode — plain mode uses a done channel instead of a
// bubbletea event loop, so the caller's "spawn work in a goroutine, block on
// Run, then read the err" pattern works identically with or without a TTY.
// Without this, agent/script callers race past VM creation and exit before
// the work goroutine writes state.
func (r *Runner) Run() error {
	if r.plain {
		<-r.done
		return nil
	}
	_, err := r.prog.Run()
	return err
}

// Start marks a phase as running. Records the start time for elapsed reporting.
func (r *Runner) Start(id string) {
	r.mu.Lock()
	r.starts[id] = time.Now()
	r.mu.Unlock()
	if r.plain {
		fmt.Fprintf(os.Stderr, "==> %s\n", r.labelOf(id))
		return
	}
	r.prog.Send(startMsg{id: id})
}

// End marks a phase as completed (success or failure) and records elapsed.
func (r *Runner) End(id string, err error) {
	r.mu.Lock()
	start, ok := r.starts[id]
	r.mu.Unlock()
	var elapsed time.Duration
	if ok {
		elapsed = time.Since(start)
	}
	if r.plain {
		mark := "✓"
		if err != nil {
			mark = "✗"
		}
		fmt.Fprintf(os.Stderr, "    %s %s (%s)\n", mark, r.labelOf(id), fmtDur(elapsed))
		if err != nil {
			fmt.Fprintf(os.Stderr, "      %v\n", err)
		}
		return
	}
	r.prog.Send(endMsg{id: id, elapsed: elapsed, err: err})
}

// Logger returns an io.Writer that buffers writes into lines and streams them
// to the active phase's viewport. Lines are trimmed to ~120 cols for layout.
func (r *Runner) Logger() io.Writer {
	if r.plain {
		// In plain mode, the subprocess output goes straight to stderr —
		// keeps the existing scrollback story while the TUI is off.
		return os.Stderr
	}
	return &lineWriter{r: r}
}

// Log appends a single line to the active phase's viewport. Format args use
// fmt.Sprintf rules. In plain mode it prints to stderr with a small indent so
// non-TTY users still see the trail.
func (r *Runner) Log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	if r.plain {
		fmt.Fprintf(os.Stderr, "      %s\n", line)
		return
	}
	r.prog.Send(logMsg{line: line})
}

// SetDetail updates the inline status shown beside the active phase in the
// header box (e.g. "1.2 GiB / 5.3 GiB, 23%"). Empty string clears it.
// Useful for live progress that should overwrite in place rather than scroll.
func (r *Runner) SetDetail(phaseID, detail string) {
	if r.plain {
		fmt.Fprintf(os.Stderr, "      [%s] %s\n", phaseID, detail)
		return
	}
	r.prog.Send(detailMsg{id: phaseID, detail: detail})
}

// Finish concludes the run and tears down the TUI. Safe to call more than
// once — only the first call unblocks Run.
func (r *Runner) Finish(err error) {
	if r.plain {
		if err != nil {
			fmt.Fprintf(os.Stderr, "krapow: %v\n", err)
		}
		r.doneOnce.Do(func() { close(r.done) })
		return
	}
	r.prog.Send(doneMsg{err: err})
}

func (r *Runner) labelOf(id string) string {
	for _, p := range r.phases {
		if p.ID == id {
			return p.Label
		}
	}
	return id
}

// ---------- bubbletea internals ----------

type status int

const (
	statusPending status = iota
	statusRunning
	statusDone
	statusError
)

type phaseState struct {
	spec    PhaseSpec
	status  status
	elapsed time.Duration
	err     error
	detail  string // inline status (e.g. download progress); cleared on phase end
}

type model struct {
	title   string
	phases  []phaseState
	spinner spinner.Model
	log     []string
	startAt time.Time
	done    bool
	doneErr error
}

type (
	startMsg struct{ id string }
	endMsg   struct {
		id      string
		elapsed time.Duration
		err     error
	}
	logMsg    struct{ line string }
	detailMsg struct{ id, detail string }
	doneMsg   struct{ err error }
)

func (m *model) Init() tea.Cmd { return m.spinner.Tick }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case startMsg:
		for i := range m.phases {
			if m.phases[i].spec.ID == msg.id {
				m.phases[i].status = statusRunning
				break
			}
		}
		// Clear viewport for the new phase so it doesn't carry stale output.
		m.log = nil
	case endMsg:
		for i := range m.phases {
			if m.phases[i].spec.ID == msg.id {
				m.phases[i].elapsed = msg.elapsed
				m.phases[i].err = msg.err
				m.phases[i].detail = "" // clear any inline progress on phase end
				if msg.err != nil {
					m.phases[i].status = statusError
				} else {
					m.phases[i].status = statusDone
				}
				break
			}
		}
	case detailMsg:
		for i := range m.phases {
			if m.phases[i].spec.ID == msg.id {
				m.phases[i].detail = msg.detail
				break
			}
		}
	case logMsg:
		m.log = append(m.log, msg.line)
		if len(m.log) > maxLogLines {
			m.log = m.log[len(m.log)-maxLogLines:]
		}
	case doneMsg:
		m.done = true
		m.doneErr = msg.err
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) View() string {
	// Render phase line: marker + label, plus optional detail in parens.
	var phaseCells []string
	for _, p := range m.phases {
		marker := pendingMarker
		switch p.status {
		case statusRunning:
			marker = m.spinner.View()
		case statusDone:
			marker = doneMarker
		case statusError:
			marker = errorMarker
		}
		cell := marker + " " + p.spec.Label
		if p.status == statusRunning && p.detail != "" {
			cell += " " + detailStyle.Render("("+p.detail+")")
		}
		phaseCells = append(phaseCells, cell)
	}
	header := fmt.Sprintf("krapow init %s   %s", m.title, fmtDur(time.Since(m.startAt)))

	body := strings.Join(phaseCells, "   ")
	box := boxStyle.Render(headerStyle.Render(header) + "\n" + body)

	var out strings.Builder
	out.WriteString(box)
	out.WriteString("\n")
	if len(m.log) > 0 {
		for _, line := range m.log {
			out.WriteString(logStyle.Render("  > "+line) + "\n")
		}
	}
	return out.String()
}

// ---------- styles ----------

var (
	styleSpinner  = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	doneMarker    = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓")
	errorMarker   = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("✗")
	pendingMarker = lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("◯")
	detailStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
	logStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	boxStyle      = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("63")).
			Padding(0, 1)
)

// ---------- helpers ----------

func fmtDur(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 1:
		return "<1s"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
	}
}

func isTerminal() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) && isatty.IsTerminal(os.Stderr.Fd())
}

// lineWriter buffers Writes until it sees a line terminator (\n or \r), then
// sends each line as a logMsg to the program. Treats \r as a line boundary so
// in-place progress redraws (e.g. `tart pull`'s percentage updates) become
// real log lines rather than corrupting the bubbletea view with raw cursor
// sequences. ANSI escape codes are stripped per-line for the same reason.
// Lines are trimmed to 120 chars for layout safety.
type lineWriter struct {
	r   *Runner
	buf bytes.Buffer
	mu  sync.Mutex
}

// ansiCSIRe matches CSI escape sequences: ESC [ <params> <letter>. Covers
// the cursor/clear sequences tart emits for its progress bar (e.g. ESC[1A,
// ESC[J) plus any color/style codes upstream tools throw at us.
var ansiCSIRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		idx := -1
		for i, b := range data {
			if b == '\n' || b == '\r' {
				idx = i
				break
			}
		}
		if idx < 0 {
			// No terminator yet; keep buffering. Reset the buffer to just the
			// unread tail (ReadString-style "put back partial").
			tail := make([]byte, len(data))
			copy(tail, data)
			w.buf.Reset()
			w.buf.Write(tail)
			return len(p), nil
		}
		line := string(data[:idx])
		// Consume the terminator. If it was \r followed immediately by \n,
		// consume both so we don't emit a spurious empty line for the \n.
		consumed := idx + 1
		if data[idx] == '\r' && consumed < len(data) && data[consumed] == '\n' {
			consumed++
		}
		// Reset buffer to the tail.
		tail := make([]byte, len(data)-consumed)
		copy(tail, data[consumed:])
		w.buf.Reset()
		w.buf.Write(tail)

		line = ansiCSIRe.ReplaceAllString(line, "")
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		if len(line) > 120 {
			line = line[:117] + "..."
		}
		w.r.prog.Send(logMsg{line: line})
	}
}
