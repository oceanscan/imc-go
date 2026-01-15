package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// findBundledExe looks for an executable in the same directory as logsync.
// If found, returns the absolute path. Otherwise, returns just the name.
func findBundledExe(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	dir := filepath.Dir(exe)
	bundled := filepath.Join(dir, name)
	if _, err := os.Stat(bundled); err == nil {
		return bundled
	}
	// On Windows, try with .exe extension
	if filepath.Ext(name) == "" {
		bundled = filepath.Join(dir, name+".exe")
		if _, err := os.Stat(bundled); err == nil {
			return bundled
		}
	}
	return name
}

func getpass(ip string) string {
	data := fmt.Sprintf("%s//%s", ip, ip)
	hash := sha1.Sum([]byte(data))
	return fmt.Sprintf("%x", hash)[:10]
}

func testAuth(ip, password string) bool {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", ip+":22", config)
	if err != nil {
		return false
	}
	client.Close()
	return true
}

func listRemoteDirs(ip, password, path string) ([]string, error) {
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	client, err := ssh.Dial("tcp", ip+":22", config)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var out bytes.Buffer
	session.Stdout = &out
	if err := session.Run("ls -1 " + path); err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var dirs []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			dirs = append(dirs, line)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs, nil
}

type item struct {
	title, desc string
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

const (
	levelVehicles = iota
	levelDays
	levelLogs
	levelAction
)

type model struct {
	list            list.Model
	items           []item
	choice          []string
	quitting        bool
	ip              string
	password        string
	src             string
	currentLevel    int // 0: vehicles, 1: days, 2: logs, 3: action
	selectedVehicle string
	selectedDay     string
	selectedAction  string // "Download" or "Delete"
}

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			m.choice = nil
			return m, tea.Quit
		case "backspace":
			if m.currentLevel == levelDays {
				return m.gotoVehiclesSelection()
			} else if m.currentLevel == levelLogs {
				return m.gotoDaysSelection()
			} else if m.currentLevel == levelAction {
				if m.selectedDay != "" {
					return m.gotoLogsSelection()
				} else if m.selectedVehicle != "" {
					return m.gotoDaysSelection()
				} else {
					return m.gotoVehiclesSelection()
				}
			}
		case "enter":
			i, ok := m.list.SelectedItem().(item)
			if ok {
				if m.currentLevel == levelVehicles {
					m.selectedVehicle = i.title
					if i.title == "ALL" {
						m.choice = []string{""} // Empty string pattern means all
						return m.gotoActionSelection()
					}
					// Load days for this vehicle
					days, err := listRemoteDirs(m.ip, m.password, m.src+"/"+m.selectedVehicle)
					if err != nil {
						m.choice = []string{m.selectedVehicle}
						return m.gotoActionSelection()
					}
					var items []list.Item
					items = append(items, item{title: "ALL", desc: "Select ALL for " + m.selectedVehicle})
					for _, day := range days {
						items = append(items, item{title: day, desc: ""})
					}
					m.list.SetItems(items)
					m.list.Title = "Select Day in " + m.selectedVehicle
					m.currentLevel = levelDays
					return m, nil
				} else if m.currentLevel == levelDays {
					m.selectedDay = i.title
					if i.title == "ALL" {
						m.choice = []string{m.selectedVehicle}
						return m.gotoActionSelection()
					}
					// Load logs for this day
					logs, err := listRemoteDirs(m.ip, m.password, m.src+"/"+m.selectedVehicle+"/"+m.selectedDay)
					if err != nil {
						m.choice = []string{m.selectedVehicle + "/" + m.selectedDay}
						return m.gotoActionSelection()
					}
					var items []list.Item
					items = append(items, item{title: "ALL", desc: "Select ALL for " + m.selectedDay})
					for _, log := range logs {
						items = append(items, item{title: log, desc: ""})
					}
					m.list.SetItems(items)
					m.list.Title = "Select Log in " + m.selectedDay
					m.currentLevel = levelLogs
					return m, nil
				} else if m.currentLevel == levelLogs {
					if i.title == "ALL" {
						m.choice = []string{m.selectedVehicle + "/" + m.selectedDay}
					} else {
						m.choice = []string{m.selectedVehicle + "/" + m.selectedDay + "/" + i.title}
					}
					return m.gotoActionSelection()
				} else if m.currentLevel == levelAction {
					m.selectedAction = i.title
					return m, tea.Quit
				}
			}
		}
	case tea.WindowSizeMsg:
		h, v := lipgloss.NewStyle().Margin(1, 2).GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v)
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *model) gotoVehiclesSelection() (tea.Model, tea.Cmd) {
	vehicles, err := listRemoteDirs(m.ip, m.password, m.src)
	if err != nil {
		return m, nil
	}
	var items []list.Item
	items = append(items, item{title: "ALL", desc: "Select ALL"})
	for _, vehicle := range vehicles {
		items = append(items, item{title: vehicle, desc: ""})
	}
	m.list.SetItems(items)
	m.list.Title = "Select Vehicle"
	m.currentLevel = levelVehicles
	m.selectedVehicle = ""
	m.selectedDay = ""
	return m, nil
}

func (m *model) gotoDaysSelection() (tea.Model, tea.Cmd) {
	days, err := listRemoteDirs(m.ip, m.password, m.src+"/"+m.selectedVehicle)
	if err != nil {
		return m.gotoVehiclesSelection()
	}
	var items []list.Item
	items = append(items, item{title: "ALL", desc: "Select ALL for " + m.selectedVehicle})
	for _, day := range days {
		items = append(items, item{title: day, desc: ""})
	}
	m.list.SetItems(items)
	m.list.Title = "Select Day in " + m.selectedVehicle
	m.currentLevel = levelDays
	m.selectedDay = ""
	return m, nil
}

func (m *model) gotoLogsSelection() (tea.Model, tea.Cmd) {
	logs, err := listRemoteDirs(m.ip, m.password, m.src+"/"+m.selectedVehicle+"/"+m.selectedDay)
	if err != nil {
		return m.gotoDaysSelection()
	}
	var items []list.Item
	items = append(items, item{title: "ALL", desc: "Select ALL for " + m.selectedDay})
	for _, log := range logs {
		items = append(items, item{title: log, desc: ""})
	}
	m.list.SetItems(items)
	m.list.Title = "Select Log in " + m.selectedDay
	m.currentLevel = levelLogs
	return m, nil
}

func (m *model) gotoActionSelection() (tea.Model, tea.Cmd) {
	var items []list.Item
	items = append(items, item{title: "Download", desc: "Sync selected logs to local machine"})
	items = append(items, item{title: "Delete", desc: "Permanently REMOVE logs from remote system"})
	m.list.SetItems(items)
	m.list.Title = "Select Action"
	m.currentLevel = levelAction
	return m, nil
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	return lipgloss.NewStyle().Margin(1, 2).Render(m.list.View())
}

// parseDryRunStats extracts file count and transfer size from rsync --stats output
func parseDryRunStats(output string) (files int, sizeBytes int64) {
	// Look for: "Number of regular files transferred: X"
	filesRe := regexp.MustCompile(`Number of regular files transferred:\s*([\d,]+)`)
	if m := filesRe.FindStringSubmatch(output); len(m) > 1 {
		fmt.Sscanf(strings.ReplaceAll(m[1], ",", ""), "%d", &files)
	}

	// Look for: "Total transferred file size: X bytes"
	sizeRe := regexp.MustCompile(`Total transferred file size:\s*([\d,]+)`)
	if m := sizeRe.FindStringSubmatch(output); len(m) > 1 {
		fmt.Sscanf(strings.ReplaceAll(m[1], ",", ""), "%d", &sizeBytes)
	}

	return files, sizeBytes
}

// formatBytes converts bytes to human readable string
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func main() {
	ip := flag.String("ip", "", "IP address of the remote vehicle")
	src := flag.String("src", "/opt/lsts/dune/log", "Source directory on the remote vehicle")
	dest := flag.String("dest", ".", "Destination directory locally")
	dryRun := flag.Bool("dryrun", false, "Perform a dry run only (no confirmation prompt)")
	yes := flag.Bool("y", false, "Skip confirmation prompt and proceed with transfer")
	dateFilter := flag.String("date", "", "Filter logs by date pattern in directory name (e.g. '202601' for Jan 2026, '20260114' for specific day)")
	today := flag.Bool("today", false, "Shortcut to sync only today's logs")
	week := flag.Bool("week", false, "Shortcut to sync logs from this week (last 7 days)")
	interactive := flag.Bool("i", false, "Interactive mode to select folders and logs")
	removeRemote := flag.Bool("rm", false, "Remove remote logs instead of syncing")
	skipTypes := flag.String("skip", "", "Comma-separated list of file extensions to skip (e.g. 'mp4,txt')")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// Validate required IP argument
	if *ip == "" {
		fmt.Fprintln(os.Stderr, "Error: -ip flag is required")
		flag.Usage()
		os.Exit(1)
	}

	// 1. Determine working password with fallback
	password := getpass(*ip)
	fmt.Printf("Probing connection to %s...\n", *ip)
	if !testAuth(*ip, password) {
		fmt.Println("Calculated password failed, trying fallback password 'root'...")
		if testAuth(*ip, "root") {
			password = "root"
			fmt.Println("Authenticated with 'root' password.")
		} else {
			fmt.Fprintf(os.Stderr, "Error: Authentication failed for both calculated password and 'root'.\n")
			os.Exit(1)
		}
	} else {
		fmt.Println("Authenticated with calculated password.")
	}

	// 2. Determine date filter and action
	var datePatterns []string
	action := "Download"
	if *removeRemote {
		action = "Delete"
	}

	if *interactive {
		days, err := listRemoteDirs(*ip, password, *src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing remote directories: %v\n", err)
			os.Exit(1)
		}

		var items []list.Item
		items = append(items, item{title: "ALL", desc: "Select ALL"})
		for _, day := range days {
			items = append(items, item{title: day, desc: ""})
		}

		delegate := list.NewDefaultDelegate()
		delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
			Foreground(lipgloss.Color("39")).
			BorderForeground(lipgloss.Color("39"))
		delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
			Foreground(lipgloss.Color("33")).
			BorderForeground(lipgloss.Color("33"))

		m := model{
			list:     list.New(items, delegate, 0, 0),
			ip:       *ip,
			password: password,
			src:      *src,
		}
		m.list.Title = "Select Vehicle"
		m.list.Styles.Title = m.list.Styles.Title.
			Background(lipgloss.Color("39")).
			Foreground(lipgloss.Color("255"))
		m.list.Styles.ActivePaginationDot = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))

		p := tea.NewProgram(m, tea.WithAltScreen())
		finalModel, err := p.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
			os.Exit(1)
		}

		m = finalModel.(model)
		if len(m.choice) == 0 {
			fmt.Println("No logs selected, exiting.")
			os.Exit(0)
		}
		datePatterns = m.choice
		if m.selectedAction != "" {
			action = m.selectedAction
		}
	} else if *today {
		datePatterns = []string{time.Now().Format("20060102")}
	} else if *week {
		// Generate patterns for the last 7 days
		for i := 0; i < 7; i++ {
			datePatterns = append(datePatterns, time.Now().AddDate(0, 0, -i).Format("20060102"))
		}
	} else if *dateFilter != "" {
		datePatterns = []string{*dateFilter}
	}

	// 3. Perform action
	if action == "Delete" {
		fmt.Printf("Preparing to DELETE remote directories from root@%s:%s\n", *ip, *src)
		fmt.Printf("Selected patterns: %v\n", datePatterns)

		if !*yes {
			fmt.Print("\nAre you sure you want to PERMANENTLY DELETE these remote logs? [y/N]: ")
			reader := bufio.NewReader(os.Stdin)
			response, _ := reader.ReadString('\n')
			response = strings.TrimSpace(strings.ToLower(response))
			if response != "y" && response != "yes" {
				fmt.Println("Deletion cancelled.")
				return
			}
		}

		fmt.Println("\nDeleting remote logs...")
		config := &ssh.ClientConfig{
			User: "root",
			Auth: []ssh.AuthMethod{
				ssh.Password(password),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}
		client, err := ssh.Dial("tcp", *ip+":22", config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error connecting for deletion: %v\n", err)
			os.Exit(1)
		}
		defer client.Close()

		for _, pattern := range datePatterns {
			remotePath := *src
			if pattern != "" {
				remotePath = *src + "/" + pattern
			}
			fmt.Printf("Removing %s...\n", remotePath)

			session, err := client.NewSession()
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error creating session for %s: %v\n", remotePath, err)
				continue
			}
			if err := session.Run("rm -rf " + remotePath); err != nil {
				fmt.Fprintf(os.Stderr, "Error deleting %s: %v\n", remotePath, err)
			}
			session.Close()
		}
		fmt.Println("\nRemote deletion completed.")
		return
	}

	// 3. Build rsync command
	sshpassPath := findBundledExe("sshpass")
	sshPath := findBundledExe("ssh")
	rshCmd := fmt.Sprintf("%s -p %s %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", sshpassPath, password, sshPath)
	remotePath := fmt.Sprintf("root@%s:%s", *ip, *src)

	args := []string{
		"-av",
		"--append",
		"--stats",
		"--prune-empty-dirs",
		"-e", rshCmd,
	}

	// Add file type exclusions (MUST come before includes)
	if *skipTypes != "" {
		exts := strings.Split(*skipTypes, ",")
		for _, ext := range exts {
			ext = strings.TrimSpace(ext)
			if ext != "" {
				if !strings.HasPrefix(ext, ".") {
					ext = "*." + ext
				}
				args = append(args, fmt.Sprintf("--exclude=%s", ext))
			}
		}
	}

	// Add include/exclude patterns for date filtering
	if len(datePatterns) > 0 {
		if datePatterns[0] != "" { // "" means all
			// To avoid creating all empty directories, we need to:
			// 1. Include each component of the path
			// 2. Include the path and everything inside it
			// 3. Exclude everything else

			// Build a set of directory components to include
			includes := make(map[string]bool)
			for _, pattern := range datePatterns {
				// E.g. lauv-omst-3/20260113/113305
				parts := strings.Split(pattern, "/")
				curr := ""
				for _, part := range parts {
					if curr == "" {
						curr = part
					} else {
						curr = curr + "/" + part
					}
					includes[curr+"/"] = true // Include the directory itself
				}
				includes[pattern+"/**"] = true // Include contents
			}

			// Sort includes to ensure parents come before children
			var sortedIncludes []string
			for inc := range includes {
				sortedIncludes = append(sortedIncludes, inc)
			}
			sort.Strings(sortedIncludes)

			for _, inc := range sortedIncludes {
				args = append(args, "--include="+inc)
			}
			args = append(args, "--exclude=*")
		}
	}

	args = append(args, remotePath+"/", *dest)

	// 4. First, always do a dry run to check what will be transferred
	fmt.Printf("Analyzing transfer from %s to %s...\n", remotePath, *dest)
	if len(datePatterns) > 0 && datePatterns[0] != "" {
		fmt.Printf("Filtering for dates: %v\n", datePatterns)
	}

	dryArgs := append([]string{"-n"}, args...)
	rsyncPath := findBundledExe("rsync")
	dryCmd := exec.Command(rsyncPath, dryArgs...)
	var dryOutput bytes.Buffer
	dryCmd.Stdout = &dryOutput
	dryCmd.Stderr = os.Stderr

	if err := dryCmd.Run(); err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			fmt.Fprintf(os.Stderr, "\nError: 'rsync' was not found in your system path.\n")
			fmt.Fprintf(os.Stderr, "To run logsync, you must have rsync and ssh installed.\n")
			if os.PathSeparator == '\\' {
				fmt.Fprintf(os.Stderr, "On Windows, we recommend installing 'rsync' via:\n")
				fmt.Fprintf(os.Stderr, " 1. WSL (Ubuntu): 'sudo apt install rsync'\n")
				fmt.Fprintf(os.Stderr, " 2. MSYS2: 'pacman -S rsync'\n")
				fmt.Fprintf(os.Stderr, " 3. Chocolatey: 'choco install rsync'\n")
				fmt.Fprintf(os.Stderr, " 4. Git for Windows (if included): add 'C:\\Program Files\\Git\\usr\\bin' to your PATH\n")
			} else {
				fmt.Fprintf(os.Stderr, "On Linux/MacOS, install it via your package manager (e.g., 'sudo apt install rsync').\n")
			}
		} else {
			fmt.Fprintf(os.Stderr, "\nError: rsync dry run failed: %v\n", err)
		}
		os.Exit(1)
	}

	// Parse the dry run output
	files, sizeBytes := parseDryRunStats(dryOutput.String())

	if files == 0 {
		fmt.Println("\nNo new files to transfer. Everything is up to date.")
		return
	}

	fmt.Printf("\n=== Transfer Summary ===\n")
	fmt.Printf("Files to transfer: %d\n", files)
	fmt.Printf("Total size: %s\n", formatBytes(sizeBytes))
	fmt.Println("========================")

	// If dryrun mode, just exit here
	if *dryRun {
		fmt.Println("\n(DRY RUN - no files transferred)")
		return
	}

	// 5. Ask for confirmation unless -y flag is set
	if !*yes {
		fmt.Print("\nProceed with transfer? [y/N]: ")
		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Println("Transfer cancelled.")
			return
		}
	}

	// 6. Perform actual transfer with progress
	fmt.Println("\nStarting transfer...")
	args = append([]string{"--progress"}, args...)
	cmd := exec.Command(rsyncPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nError: rsync execution failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\nSync operation completed successfully.")
}
