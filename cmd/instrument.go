package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/dave/dst/decorator"
	"github.com/newrelic/go-easy-instrumentation/internal/comment"
	"github.com/newrelic/go-easy-instrumentation/parser"
	"github.com/spf13/cobra"
	"golang.org/x/tools/go/packages"
)

const (
	defaultAgentVariableName = "NewRelicAgent"
	defaultPackageName       = "./..."
	defaultPackagePath       = ""
	defaultAppName           = ""
	defaultOutputFilePath    = ""
	defaultDiffFileName      = "new-relic-instrumentation.diff"
	defaultDebug             = false
)

var (
	debug    bool
	diffFile string
)

var instrumentCmd = &cobra.Command{
	Use:   "instrument <path>",
	Short: "add instrumentation",
	Long:  "add instrumentation to an application's source files and write these changes to a diff file",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		Instrument(args[0])
	},
}

// validateOutputFile checks that the custom output path is valid
func validateOutputFile(path string) error {
	if filepath.Ext(path) != ".diff" {
		return errors.New("output file must have a .diff extension")
	}

	_, err := os.Stat(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("output file directory does not exist: %v", err)
	}

	return nil
}

// setOutputFilePath returns a complete output file path based on the provided
// diffFile flag value. If the flag is empty, the default path will be based
// on the applicationPath.
//
// This will fail if the packagePath is not valid, and must be run after
// validating it.
func setOutputFilePath(outputFilePath, applicationPath string) (string, error) {
	if outputFilePath == "" {
		outputFilePath = filepath.Join(applicationPath, defaultDiffFileName)
	}

	err := validateOutputFile(outputFilePath)
	if err != nil {
		return "", err
	}

	return outputFilePath, nil
}

const LoadMode = packages.LoadSyntax | packages.NeedForTest

// Bubble Tea Model
type model struct {
	spinner     spinner.Model
	progress    progress.Model
	stepDesc    string
	totalSteps  int
	currentStep int
	done        bool
	err         error
	packages    []*decorator.Package
	pkgPath     string
	sub         chan tea.Msg
	outputFile  string
}

// Messages
// Messages
type progressMsg struct {
	desc string
}
type pkgLoadedMsg []*decorator.Package
type errMsg error
type completedMsg struct{}

func initialModel(pkgPath, outputFile string) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("#1CE783"))
	return model{
		spinner:    s,
		progress:   progress.New(progress.WithGradient("#008C99", "#1CE783")),
		stepDesc:   "Loading packages...",
		totalSteps: 8,
		pkgPath:    pkgPath,
		outputFile: outputFile,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		waitForNext(m.sub),
	)
}

func Instrument(packagePath string, patterns ...string) {
	if packagePath == "" {
		cobra.CheckErr("path argument cannot be empty")
	}
	if _, err := os.Stat(packagePath); err != nil {
		cobra.CheckErr(fmt.Errorf("path argument \"%s\" is invalid: %v", packagePath, err))
	}
	outputFile, err := setOutputFilePath(diffFile, packagePath)
	cobra.CheckErr(err)
	if debug {
		comment.EnableConsolePrinter(packagePath)
	}

	// Channel to receive updates from the worker
	updates := make(chan tea.Msg)

	// Worker goroutine
	go func() {
		loadPatterns := patterns
		if len(loadPatterns) == 0 {
			loadPatterns = []string{defaultPackageName}
		}

		pkgs, err := decorator.Load(&packages.Config{Dir: packagePath, Mode: LoadMode, Tests: true}, loadPatterns...)
		if err != nil {
			updates <- errMsg(err)
			return
		}

		updates <- pkgLoadedMsg(pkgs)

		manager := parser.NewInstrumentationManager(pkgs, defaultAppName, defaultAgentVariableName, outputFile, packagePath)

		steps := []struct {
			desc string
			fn   func() error
		}{
			{"Creating diff file", manager.CreateDiffFile},
			{"Detecting dependencies", manager.DetectDependencyIntegrations},
			{"Tracing package calls", manager.TracePackageCalls},
			{"Scanning application", manager.ScanApplication},
			{"Instrumenting application", manager.InstrumentApplication},
			{"Resolving unit tests", manager.ResolveUnitTests},
			{"Adding required modules", manager.AddRequiredModules},
			{"Writing diff file", func() error {
				comment.WriteAll()
				// Pass a callback to WriteDiff to receive granular progress updates.
				// This callback updates the UI with the name of the file currently being written,
				// avoiding a "stalled" UI during this potentially long-running step.
				return manager.WriteDiff(func(msg string) {
					updates <- progressMsg{desc: msg}
				})
			}},
		}

		for _, step := range steps {
			updates <- progressMsg{desc: step.desc}
			if err := step.fn(); err != nil {
				updates <- errMsg(err)
				return
			}
		}

		updates <- completedMsg{}
		close(updates)
	}()

	initialM := initialModel(packagePath, outputFile)
	initialM.sub = updates

	finalModel, err := tea.NewProgram(initialM).Run()
	if err != nil {
		fmt.Printf("Error running program: %v\n", err)
		os.Exit(1)
	}

	if m, ok := finalModel.(model); ok {
		if m.err != nil {
			os.Exit(1)
		}
		if m.done {
			fmt.Printf("\nDone! Changes written to: %s\nTip: Apply these changes with: git apply %s\n", m.outputFile, m.outputFile)
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case progress.FrameMsg:
		newModel, cmd := m.progress.Update(msg)
		if p, ok := newModel.(progress.Model); ok {
			m.progress = p
		}
		return m, cmd
	case pkgLoadedMsg:
		m.packages = msg
		m.stepDesc = "Starting instrumentation..."
		return m, waitForNext(m.sub)
	case progressMsg:
		m.currentStep++
		m.stepDesc = msg.desc
		cmd := m.progress.SetPercent(float64(m.currentStep) / float64(m.totalSteps))
		return m, tea.Batch(cmd, waitForNext(m.sub))
	case errMsg:
		m.err = msg
		return m, tea.Quit
	case completedMsg:
		m.done = true
		return m, tea.Quit
	}

	// If we are strictly in Init, we should return the initial batch.
	// But Update is called for every message.
	// If the message is none of the above (shouldn't happen usually), we return nil.

	// Wait, we need to ensure the first waitForNext is called.
	// We can do it in a special "start" message or just include it in Init.
	return m, nil
}

func waitForNext(sub chan tea.Msg) tea.Cmd {
	if sub == nil {
		return nil
	}
	return func() tea.Msg {
		msg, ok := <-sub
		if !ok {
			return nil
		}
		return msg
	}
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("\nError: %v\n", m.err)
	}
	if m.err != nil {
		return fmt.Sprintf("\nError: %v\n", m.err)
	}

	pad := strings.Repeat(" ", padding(m.stepDesc, 30))

	if m.packages == nil {
		// Loading phase
		return fmt.Sprintf("\n %s %s%s\n\n", m.spinner.View(), m.stepDesc, pad)
	}

	// Instrumentation phase
	return fmt.Sprintf("\n %s%s\n %s\n\n", m.stepDesc, pad, m.progress.View())
}

func padding(s string, width int) int {
	l := len(s)
	if l > width {
		return 0
	}
	return width - l
}

func init() {
	instrumentCmd.Flags().BoolVarP(&debug, "debug", "D", defaultDebug, "enable debugging output")
	instrumentCmd.Flags().StringVarP(&diffFile, "output", "o", defaultOutputFilePath, "specify diff output file path")
	cobra.MarkFlagFilename(instrumentCmd.Flags(), "output", ".diff") // for file completion

	rootCmd.AddCommand(instrumentCmd)
}
