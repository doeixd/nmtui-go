// Package main implements a Terminal User Interface (TUI) for managing NetworkManager Wi-Fi connections.
// It allows users to scan for networks, connect to secured and open networks, view connection details,
// manage known profiles, and toggle Wi-Fi radio status.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nmtui/gonetworkmanager"
)

// =============================================================================
// Constants
// =============================================================================

const (
	debugLogFile            = "nmtui-debug.log"
	appName                 = "Go Network Manager TUI"
	cacheFileName           = "nmtui-cache.json"
	helpBarMaxWidth         = 80
	helpBarWidthPercent     = 0.80
	networkListFixedWidth   = 100
	networkListWidthPercent = 0.85
	minListHeight           = 5
	minListWidth            = 40
	minTerminalWidth        = 60
	minTerminalHeight       = 15
	passwordMaxLength       = 63 // WPA2/WPA3 max password length
	filterMaxLength         = 100
	passwordInputMaxWidth   = 60
	passwordInputMinWidth   = 40
	statusMsgTimeout        = 3 * time.Second
	connectionTimeout       = 30 * time.Second
	autoRefreshInterval     = 30 * time.Second
)

// Signal strength thresholds
const (
	signalExcellent = 70
	signalGood      = 40
)

// =============================================================================
// Styles
// =============================================================================

var (
	appStyle = lipgloss.NewStyle().Margin(1, 1)

	// Color palette (ANSI colors for broad terminal support)
	colorPrimary   = lipgloss.Color("5")  // Magenta/Purple
	colorSecondary = lipgloss.Color("4")  // Blue
	colorAccent    = lipgloss.Color("6")  // Cyan
	colorSuccess   = lipgloss.Color("2")  // Green
	colorError     = lipgloss.Color("1")  // Red
	colorWarning   = lipgloss.Color("3")  // Yellow
	colorFaint     = lipgloss.Color("8")  // Gray
	colorText      = lipgloss.Color("7")  // White/Light gray

	// Component styles
	titleStyle            = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary).Padding(0, 1).MarginBottom(1)
	listTitleStyle        = lipgloss.NewStyle().Foreground(colorSecondary).Padding(0, 1).Bold(true)
	listItemStyle         = lipgloss.NewStyle().PaddingLeft(2).Foreground(colorText)
	listSelectedItemStyle = lipgloss.NewStyle().PaddingLeft(1).Foreground(colorPrimary).Bold(true)
	listDescStyle         = lipgloss.NewStyle().PaddingLeft(2).Foreground(colorFaint)
	listSelectedDescStyle = lipgloss.NewStyle().PaddingLeft(1).Foreground(colorPrimary)
	listNoItemsStyle      = lipgloss.NewStyle().Faint(true).Margin(1, 0).Align(lipgloss.Center).Foreground(colorFaint)

	statusMessageBaseStyle     = lipgloss.NewStyle().MarginTop(1)
	errorStyle                 = statusMessageBaseStyle.Foreground(colorError).Bold(true)
	successStyle               = statusMessageBaseStyle.Foreground(colorSuccess).Bold(true)
	warningStyle               = statusMessageBaseStyle.Foreground(colorWarning)
	infoStyle                  = statusMessageBaseStyle.Foreground(colorFaint)
	connectingStyle            = lipgloss.NewStyle().Foreground(colorAccent)
	infoBoxStyle               = lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true).BorderForeground(colorAccent).Padding(1, 2).MarginTop(1)
	passwordPromptStyle        = lipgloss.NewStyle().Foreground(colorFaint)
	passwordInputContainerStyle = lipgloss.NewStyle().Padding(1).MarginTop(1).Border(lipgloss.NormalBorder(), true).BorderForeground(colorFaint)
	helpGlobalStyle            = lipgloss.NewStyle().Foreground(colorFaint)
	filterInputStyle           = lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("62")).Padding(0, 1)

	// Status indicators
	wifiStatusEnabled  = lipgloss.NewStyle().Foreground(colorSuccess)
	wifiStatusDisabled = lipgloss.NewStyle().Foreground(colorError)
	hiddenStatusStyle  = lipgloss.NewStyle().Foreground(colorFaint).Italic(true)

	// Signal strength styles
	signalExcellentStyle = lipgloss.NewStyle().Foreground(colorSuccess)
	signalGoodStyle      = lipgloss.NewStyle().Foreground(colorWarning)
	signalWeakStyle      = lipgloss.NewStyle().Foreground(colorError)
)

// =============================================================================
// View States
// =============================================================================

type viewState int

const (
	viewNetworksList viewState = iota
	viewPasswordInput
	viewConnecting
	viewConnectionResult
	viewActiveConnectionInfo
	viewConfirmDisconnect
	viewConfirmForget
	viewKnownNetworksList
	viewHiddenNetworkInput
	viewConfirmOpenNetwork
)

func (v viewState) String() string {
	names := []string{
		"NetworksList",
		"PasswordInput",
		"Connecting",
		"ConnectionResult",
		"ActiveConnectionInfo",
		"ConfirmDisconnect",
		"ConfirmForget",
		"KnownNetworksList",
		"HiddenNetworkInput",
		"ConfirmOpenNetwork",
	}
	if int(v) < len(names) {
		return names[v]
	}
	return fmt.Sprintf("Unknown(%d)", v)
}

// =============================================================================
// List Item Delegate
// =============================================================================

type itemDelegate struct{}

func (d itemDelegate) Height() int                             { return 2 }
func (d itemDelegate) Spacing() int                            { return 1 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }

func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	ap, ok := listItem.(wifiAP)
	if !ok {
		return
	}

	var title, desc string
	if index == m.Index() {
		title = listSelectedItemStyle.Render("▸ " + ap.StyledTitle())
		desc = listSelectedDescStyle.Render("  " + ap.Description())
	} else {
		title = listItemStyle.Render("  " + ap.StyledTitle())
		desc = listDescStyle.Render("  " + ap.Description())
	}
	fmt.Fprintf(w, "%s\n%s", title, desc)
}

// =============================================================================
// Wi-Fi Access Point Model
// =============================================================================

type wifiAP struct {
	gonetworkmanager.WifiAccessPoint
	IsKnown   bool
	IsActive  bool
	Interface string
}

func (ap wifiAP) SSID() string {
	if ap.WifiAccessPoint == nil {
		return ""
	}
	ssid := ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSSID]
	if ssid == "" || ssid == "--" {
		return ""
	}
	return ssid
}

func (ap wifiAP) DisplaySSID() string {
	ssid := ap.SSID()
	if ssid == "" {
		return "<Hidden Network>"
	}
	return ssid
}

func (ap wifiAP) Signal() int {
	if ap.WifiAccessPoint == nil {
		return 0
	}
	signal, _ := strconv.Atoi(ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
	return signal
}

func (ap wifiAP) Security() string {
	if ap.WifiAccessPoint == nil {
		return ""
	}
	sec := ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSecurity]
	if sec == "" || sec == "--" {
		return "Open"
	}
	return sec
}

func (ap wifiAP) IsOpen() bool {
	sec := strings.ToLower(ap.Security())
	return sec == "" || sec == "open" || sec == "--"
}

func (ap wifiAP) IsHidden() bool {
	return ap.SSID() == ""
}

func (ap wifiAP) SignalBars() string {
	signal := ap.Signal()
	switch {
	case signal >= signalExcellent:
		return signalExcellentStyle.Render("▂▄▆█")
	case signal >= signalGood:
		return signalGoodStyle.Render("▂▄▆") + lipgloss.NewStyle().Foreground(colorFaint).Render("█")
	case signal > 0:
		return signalWeakStyle.Render("▂▄") + lipgloss.NewStyle().Foreground(colorFaint).Render("▆█")
	default:
		return lipgloss.NewStyle().Foreground(colorFaint).Render("▂▄▆█")
	}
}

func (ap wifiAP) StyledTitle() string {
	title := ap.DisplaySSID()

	var indicators []string
	if ap.IsActive {
		indicators = append(indicators, lipgloss.NewStyle().Foreground(colorSuccess).Render(" ✔"))
	}
	if ap.IsKnown && !ap.IsActive {
		indicators = append(indicators, lipgloss.NewStyle().Foreground(colorAccent).Render(" ★"))
	}
	if ap.IsOpen() && ap.Signal() > 0 {
		indicators = append(indicators, lipgloss.NewStyle().Foreground(colorWarning).Render(" 🔓"))
	}

	return title + strings.Join(indicators, "")
}

func (ap wifiAP) Title() string {
	return ap.StyledTitle()
}

func (ap wifiAP) Description() string {
	labelStyle := lipgloss.NewStyle().Foreground(colorFaint)
	var parts []string

	signal := ap.Signal()

	// For known networks with no signal, show out of range
	if ap.IsKnown && signal == 0 {
		parts = append(parts, labelStyle.Render("Known (Out of Range)"))
	} else if signal > 0 {
		parts = append(parts, fmt.Sprintf("%s %s %s",
			labelStyle.Render("Signal:"),
			ap.SignalBars(),
			ap.signalPercentStyle().Render(fmt.Sprintf("%d%%", signal))))
	}

	parts = append(parts, fmt.Sprintf("%s %s",
		labelStyle.Render("Security:"),
		labelStyle.Render(ap.Security())))

	return strings.Join(parts, labelStyle.Render(" │ "))
}

func (ap wifiAP) signalPercentStyle() lipgloss.Style {
	signal := ap.Signal()
	switch {
	case signal >= signalExcellent:
		return signalExcellentStyle
	case signal >= signalGood:
		return signalGoodStyle
	default:
		return signalWeakStyle
	}
}

func (ap wifiAP) FilterValue() string {
	return ap.DisplaySSID()
}

// =============================================================================
// Messages
// =============================================================================

type wifiListLoadedMsg struct {
	allAps []wifiAP
	err    error
}

type connectionAttemptMsg struct {
	ssid                 string
	success              bool
	err                  error
	wasKnownAttemptNoPsk bool
}

type wifiStatusMsg struct {
	enabled bool
	err     error
}

type knownNetworksMsg struct {
	knownProfiles        map[string]gonetworkmanager.ConnectionProfile
	activeWifiConnection *gonetworkmanager.ConnectionProfile
	activeWifiDevice     string
	err                  error
}

type activeConnInfoMsg struct {
	details *gonetworkmanager.DeviceIPDetail
	err     error
}

type disconnectResultMsg struct {
	ssid    string
	success bool
	err     error
}

type forgetNetworkResultMsg struct {
	ssid    string
	success bool
	err     error
}

type knownWifiApsListMsg struct {
	aps []wifiAP
	err error
}

type clearStatusMsg struct{}

type connectionTimeoutMsg struct {
	ssid string
}

type autoRefreshTickMsg struct{}

// =============================================================================
// Key Bindings
// =============================================================================

type keyMap struct {
	Connect      key.Binding
	Refresh      key.Binding
	Quit         key.Binding
	Back         key.Binding
	Help         key.Binding
	Filter       key.Binding
	ToggleWifi   key.Binding
	Disconnect   key.Binding
	Info         key.Binding
	ToggleHidden key.Binding
	Forget       key.Binding
	Profiles     key.Binding
	HiddenSSID   key.Binding
	currentState viewState
}

func (k keyMap) ShortHelp() []key.Binding {
	bindings := []key.Binding{k.Help}

	switch k.currentState {
	case viewNetworksList:
		bindings = append(bindings, k.Connect, k.Refresh, k.Filter, k.ToggleWifi, k.Profiles)
	case viewKnownNetworksList:
		bindings = append(bindings, k.Back, k.Forget)
	case viewPasswordInput, viewHiddenNetworkInput, viewConnectionResult,
		viewConfirmDisconnect, viewConfirmForget, viewConfirmOpenNetwork:
		bindings = append(bindings, k.Connect, k.Back)
	case viewActiveConnectionInfo:
		bindings = append(bindings, k.Back)
	}

	return append(bindings, k.Quit)
}

func (k keyMap) FullHelp() [][]key.Binding {
	switch k.currentState {
	case viewKnownNetworksList:
		return [][]key.Binding{{k.Back, k.Forget, k.Quit}}
	default:
		return [][]key.Binding{
			{k.Help, k.Connect, k.Back, k.Quit},
			{k.Refresh, k.Filter, k.ToggleHidden, k.ToggleWifi},
			{k.Disconnect, k.Forget, k.Info, k.Profiles},
			{k.HiddenSSID},
		}
	}
}

var defaultKeyBindings = keyMap{
	Connect:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select/confirm")),
	Refresh:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Back:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back/cancel")),
	Help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Filter:       key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	ToggleWifi:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle Wi-Fi")),
	Disconnect:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "disconnect")),
	Forget:       key.NewBinding(key.WithKeys("ctrl+f"), key.WithHelp("ctrl+f", "forget")),
	Info:         key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "info")),
	ToggleHidden: key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unnamed nets")),
	Profiles:     key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "profiles")),
	HiddenSSID:   key.NewBinding(key.WithKeys("h"), key.WithHelp("h", "hidden SSID")),
}

// =============================================================================
// Main Model
// =============================================================================

type model struct {
	// State management
	state         viewState
	previousState viewState

	// UI components
	wifiList               list.Model
	knownWifiList          list.Model
	passwordInput          textinput.Model
	hiddenSSIDInput        textinput.Model
	filterInput            textinput.Model
	spinner                spinner.Model
	activeConnInfoViewport viewport.Model
	keys                   keyMap
	help                   help.Model

	// Current operation context
	selectedAP                  wifiAP
	connectionStatusMsg         string
	lastConnectionWasSuccessful bool

	// Wi-Fi state
	wifiEnabled          bool
	knownProfiles        map[string]gonetworkmanager.ConnectionProfile
	activeWifiConnection *gonetworkmanager.ConnectionProfile
	activeWifiDevice     string
	allScannedAps        []wifiAP

	// UI state flags
	showHiddenNetworks bool
	isLoading          bool
	isScanning         bool
	isFiltering        bool
	filterQuery        string
	autoRefreshEnabled bool

	// Dimensions
	width            int
	height           int
	listDisplayWidth int
}

func initialModel() model {
	// Initialize list
	delegate := itemDelegate{}
	wifiList := list.New([]list.Item{}, delegate, 0, 0)
	wifiList.Title = "Scanning for Wi-Fi Networks..."
	wifiList.Styles.Title = listTitleStyle
	wifiList.SetShowStatusBar(true)
	wifiList.SetStatusBarItemName("network", "networks")
	wifiList.SetShowHelp(false)
	wifiList.DisableQuitKeybindings()
	wifiList.Styles.NoItems = listNoItemsStyle.SetString("No Wi-Fi. Try (r)efresh, (t)oggle Wi-Fi, (u)nnamed.")
	wifiList.Styles.FilterPrompt = lipgloss.NewStyle().Foreground(colorPrimary)
	wifiList.Styles.FilterCursor = lipgloss.NewStyle().Foreground(colorPrimary)

	// Initialize known networks list
	knownList := list.New([]list.Item{}, delegate, 0, 0)
	knownList.Title = "Known Wi-Fi Profiles"
	knownList.Styles.Title = listTitleStyle
	knownList.SetShowStatusBar(false)
	knownList.SetShowHelp(false)
	knownList.DisableQuitKeybindings()
	knownList.Styles.NoItems = listNoItemsStyle.SetString("No known Wi-Fi profiles found.")

	// Initialize password input
	pwInput := textinput.New()
	pwInput.Placeholder = "Network Password"
	pwInput.EchoMode = textinput.EchoPassword
	pwInput.CharLimit = passwordMaxLength
	pwInput.Prompt = passwordPromptStyle.Render("🔑 Password: ")
	pwInput.EchoCharacter = '•'
	pwInput.Cursor.Style = lipgloss.NewStyle().Foreground(colorAccent)

	// Initialize hidden SSID input
	ssidInput := textinput.New()
	ssidInput.Placeholder = "Network Name (SSID)"
	ssidInput.CharLimit = 32 // Max SSID length
	ssidInput.Prompt = lipgloss.NewStyle().Foreground(colorAccent).Render("📡 SSID: ")
	ssidInput.Cursor.Style = lipgloss.NewStyle().Foreground(colorAccent)

	// Initialize filter input
	filterInput := textinput.New()
	filterInput.Placeholder = "Type to filter..."
	filterInput.CharLimit = filterMaxLength
	filterInput.Prompt = "/ "
	filterInput.Cursor.Style = lipgloss.NewStyle().Foreground(colorPrimary)

	// Initialize spinner
	s := spinner.New()
	s.Spinner = spinner.Globe
	s.Style = connectingStyle

	// Initialize viewport for connection info
	vp := viewport.New(0, 0)
	vp.Style = infoBoxStyle

	// Initialize help
	h := help.New()
	h.ShowAll = false
	subtleStyle := lipgloss.NewStyle().Foreground(colorFaint)
	h.Styles = help.Styles{
		ShortKey:  subtleStyle,
		ShortDesc: subtleStyle,
		FullKey:   subtleStyle,
		FullDesc:  subtleStyle,
		Ellipsis:  subtleStyle,
	}

	m := model{
		state:                  viewNetworksList,
		wifiList:               wifiList,
		knownWifiList:          knownList,
		passwordInput:          pwInput,
		hiddenSSIDInput:        ssidInput,
		filterInput:            filterInput,
		spinner:                s,
		activeConnInfoViewport: vp,
		keys:                   defaultKeyBindings,
		help:                   h,
		knownProfiles:          make(map[string]gonetworkmanager.ConnectionProfile),
		showHiddenNetworks:     false,
		isLoading:              true,
		isScanning:             true,
		autoRefreshEnabled:     false,
	}
	m.keys.currentState = m.state

	// Load cached networks for fast startup
	if cachedAps := loadCachedNetworks(); cachedAps != nil {
		m.allScannedAps = cachedAps
		m.processAndSetWifiList(cachedAps)
		log.Printf("Loaded %d cached networks", len(cachedAps))
	}

	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		getWifiStatusCmd(),
		fetchKnownNetworksCmd(),
		fetchWifiNetworksCmd(true),
		m.spinner.Tick,
	)
}

// =============================================================================
// Cache Management
// =============================================================================

func getCacheFilePath() string {
	return filepath.Join(os.TempDir(), cacheFileName)
}

func loadCachedNetworks() []wifiAP {
	data, err := os.ReadFile(getCacheFilePath())
	if err != nil {
		log.Printf("No cache file found: %v", err)
		return nil
	}

	var cached []wifiAP
	if err := json.Unmarshal(data, &cached); err != nil {
		log.Printf("Failed to parse cache: %v", err)
		return nil
	}

	return cached
}

func saveCachedNetworksCmd(aps []wifiAP) tea.Cmd {
	return func() tea.Msg {
		data, err := json.Marshal(aps)
		if err != nil {
			log.Printf("Failed to marshal cache: %v", err)
			return nil
		}

		if err := os.WriteFile(getCacheFilePath(), data, 0600); err != nil {
			log.Printf("Failed to write cache: %v", err)
		}
		return nil
	}
}

// =============================================================================
// Commands
// =============================================================================

func fetchWifiNetworksCmd(rescan bool) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Fetching Wi-Fi networks (rescan: %t)...", rescan)

		apsRaw, err := gonetworkmanager.GetWifiList(rescan)
		if err != nil {
			log.Printf("Error fetching Wi-Fi list: %v", err)
			return wifiListLoadedMsg{err: err}
		}

		aps := make([]wifiAP, len(apsRaw))
		for i, raw := range apsRaw {
			aps[i] = wifiAP{WifiAccessPoint: raw}
		}

		log.Printf("Fetched %d Wi-Fi networks", len(aps))
		return wifiListLoadedMsg{allAps: aps, err: nil}
	}
}

func connectToWifiCmd(ssid, password string, knownNoPsk bool) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Connecting to SSID: '%s', wasKnownNoPsk: %t", ssid, knownNoPsk)

		_, err := gonetworkmanager.ConnectToWifiRobustly(ssid, "*", ssid, password, false)
		if err != nil {
			log.Printf("Connect error for '%s': %v", ssid, err)
		} else {
			log.Printf("Successfully connected to '%s'", ssid)
		}

		return connectionAttemptMsg{
			ssid:                 ssid,
			success:              err == nil,
			err:                  err,
			wasKnownAttemptNoPsk: knownNoPsk,
		}
	}
}

func getWifiStatusCmd() tea.Cmd {
	return func() tea.Msg {
		log.Println("Getting Wi-Fi status...")

		status, err := gonetworkmanager.GetWifiStatus()
		if err != nil {
			log.Printf("Error getting Wi-Fi status: %v", err)
			return wifiStatusMsg{enabled: false, err: err}
		}

		enabled := status == "enabled"
		log.Printf("Wi-Fi status: %s (enabled: %t)", status, enabled)
		return wifiStatusMsg{enabled: enabled, err: nil}
	}
}

func toggleWifiCmd(enable bool) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Toggling Wi-Fi to %t...", enable)

		var err error
		if enable {
			_, err = gonetworkmanager.WifiEnable()
		} else {
			_, err = gonetworkmanager.WifiDisable()
		}

		if err != nil {
			log.Printf("Error toggling Wi-Fi: %v", err)
			return wifiStatusMsg{enabled: !enable, err: err}
		}

		return wifiStatusMsg{enabled: enable, err: nil}
	}
}

func fetchKnownNetworksCmd() tea.Cmd {
	return func() tea.Msg {
		log.Println("Fetching known networks...")

		profiles, err := gonetworkmanager.GetConnectionProfilesList(false)
		if err != nil {
			log.Printf("Error fetching known profiles: %v", err)
			return knownNetworksMsg{err: err}
		}

		log.Printf("Got %d total profiles", len(profiles))

		// Get active profiles to determine which is currently connected
		activeProfiles, _ := gonetworkmanager.GetConnectionProfilesList(true)
		activeUUIDs := make(map[string]struct{})
		for _, profile := range activeProfiles {
			if profile[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi {
				activeUUIDs[profile[gonetworkmanager.NmcliFieldConnectionUUID]] = struct{}{}
			}
		}

		known := make(map[string]gonetworkmanager.ConnectionProfile)
		var activeConn *gonetworkmanager.ConnectionProfile
		var activeDev string

		for _, profile := range profiles {
			if profile[gonetworkmanager.NmcliFieldConnectionType] != gonetworkmanager.ConnectionTypeWifi {
				continue
			}

			ssid := gonetworkmanager.GetSSIDFromProfile(profile)
			if ssid == "" {
				ssid = profile[gonetworkmanager.NmcliFieldConnectionName]
			}

			if ssid == "" {
				continue
			}

			known[ssid] = profile

			if _, isActive := activeUUIDs[profile[gonetworkmanager.NmcliFieldConnectionUUID]]; isActive {
				profileCopy := make(gonetworkmanager.ConnectionProfile)
				for k, v := range profile {
					profileCopy[k] = v
				}
				activeConn = &profileCopy
				activeDev = profile[gonetworkmanager.NmcliFieldConnectionDevice]
				log.Printf("Found active Wi-Fi: %s (device: %s)", ssid, activeDev)
			}
		}

		log.Printf("Found %d known Wi-Fi profiles, active: %v", len(known), activeConn != nil)
		return knownNetworksMsg{
			knownProfiles:        known,
			activeWifiConnection: activeConn,
			activeWifiDevice:     activeDev,
			err:                  nil,
		}
	}
}

func fetchActiveConnInfoCmd(deviceName string) tea.Cmd {
	return func() tea.Msg {
		if deviceName == "" {
			return activeConnInfoMsg{nil, fmt.Errorf("no active Wi-Fi device")}
		}

		log.Printf("Fetching IP details for device: %s", deviceName)
		details, err := gonetworkmanager.GetDeviceInfoIPDetail(deviceName)
		if err != nil {
			log.Printf("Error fetching IP details: %v", err)
		}

		return activeConnInfoMsg{details: details, err: err}
	}
}

func disconnectWifiCmd(profileID string) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Disconnecting profile: %s", profileID)

		_, err := gonetworkmanager.ConnectionDown(profileID)
		if err != nil {
			log.Printf("Error disconnecting: %v", err)
		}

		return disconnectResultMsg{
			ssid:    profileID,
			success: err == nil,
			err:     err,
		}
	}
}

func forgetNetworkCmd(profileID, ssid string) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Forgetting profile: '%s' (SSID: '%s')", profileID, ssid)

		_, err := gonetworkmanager.ConnectionDelete(profileID)
		if err != nil {
			log.Printf("Error forgetting profile: %v", err)
		}

		return forgetNetworkResultMsg{
			ssid:    ssid,
			success: err == nil,
			err:     err,
		}
	}
}

func fetchKnownWifiApsCmd() tea.Cmd {
	return func() tea.Msg {
		log.Println("Fetching all known Wi-Fi profiles...")

		profiles, err := gonetworkmanager.GetConnectionProfilesList(false)
		if err != nil {
			log.Printf("Error fetching profiles: %v", err)
			return knownWifiApsListMsg{err: err}
		}

		var aps []wifiAP
		for _, profile := range profiles {
			if profile[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi {
				ap := connectionProfileToWifiAP(profile, nil)
				aps = append(aps, ap)
			}
		}

		log.Printf("Found %d known Wi-Fi profiles", len(aps))
		return knownWifiApsListMsg{aps: aps, err: nil}
	}
}

func clearStatusAfterDelay() tea.Cmd {
	return tea.Tick(statusMsgTimeout, func(time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}

func connectionTimeoutCmd(ssid string) tea.Cmd {
	return tea.Tick(connectionTimeout, func(time.Time) tea.Msg {
		return connectionTimeoutMsg{ssid: ssid}
	})
}

// =============================================================================
// Helper Functions
// =============================================================================

func connectionProfileToWifiAP(profile gonetworkmanager.ConnectionProfile, activeConn *gonetworkmanager.ConnectionProfile) wifiAP {
	ssid := gonetworkmanager.GetSSIDFromProfile(profile)
	if ssid == "" {
		ssid = profile[gonetworkmanager.NmcliFieldConnectionName]
	}

	apMap := make(gonetworkmanager.WifiAccessPoint)
	for k, v := range profile {
		apMap[k] = v
	}
	apMap[gonetworkmanager.NmcliFieldWifiSSID] = ssid

	isActive := false
	if activeConn != nil {
		isActive = profile[gonetworkmanager.NmcliFieldConnectionUUID] == (*activeConn)[gonetworkmanager.NmcliFieldConnectionUUID]
	}

	return wifiAP{
		WifiAccessPoint: apMap,
		IsKnown:         true,
		IsActive:        isActive,
		Interface:       profile[gonetworkmanager.NmcliFieldConnectionDevice],
	}
}

func (m *model) applyFilterAndUpdateList() {
	allItems := m.getAllWifiItems()

	var filteredItems []list.Item
	if m.filterQuery != "" {
		query := strings.ToLower(m.filterQuery)
		for _, item := range allItems {
			ap := item.(wifiAP)
			ssid := strings.ToLower(ap.DisplaySSID())
			if strings.Contains(ssid, query) {
				filteredItems = append(filteredItems, item)
			}
		}
	} else {
		filteredItems = allItems
	}

	m.wifiList.SetItems(filteredItems)
	m.updateListTitle(len(allItems), len(filteredItems))
}

func (m *model) updateListTitle(totalCount, filteredCount int) {
	var knownCount, availableCount int
	for _, item := range m.wifiList.Items() {
		ap := item.(wifiAP)
		if ap.IsKnown {
			knownCount++
		} else {
			availableCount++
		}
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("Wi-Fi Networks: %d Known, %d Available", knownCount, availableCount))

	if !m.showHiddenNetworks {
		parts = append(parts, hiddenStatusStyle.Render("(hiding unnamed)"))
	}

	if m.filterQuery != "" {
		filterInfo := lipgloss.NewStyle().Foreground(colorPrimary).
			Render(fmt.Sprintf("[filtered: %d/%d]", filteredCount, totalCount))
		parts = append(parts, filterInfo)
	}

	m.wifiList.Title = strings.Join(parts, " ")
}

func (m *model) getAllWifiItems() []list.Item {
	log.Printf("Processing %d scanned APs, %d known profiles",
		len(m.allScannedAps), len(m.knownProfiles))

	// Deduplicate by SSID, keeping strongest signal
	deduped := make(map[string]wifiAP)
	for _, ap := range m.allScannedAps {
		ssid := ap.SSID()
		if ssid == "" {
			// Hidden networks: use BSSID as key
			bssid := ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiBSSID]
			key := "|" + bssid
			deduped[key] = ap
		} else {
			if existing, ok := deduped[ssid]; ok {
				if ap.Signal() > existing.Signal() {
					deduped[ssid] = ap
				}
			} else {
				deduped[ssid] = ap
			}
		}
	}

	// Add known networks not in scan
	for ssid, profile := range m.knownProfiles {
		if _, found := deduped[ssid]; !found {
			knownAP := connectionProfileToWifiAP(profile, m.activeWifiConnection)
			deduped[ssid] = knownAP
		}
	}

	// Filter based on hidden network preference and enrich with known/active status
	var items []list.Item
	for _, ap := range deduped {
		if !m.showHiddenNetworks && ap.IsHidden() {
			continue
		}

		ssid := ap.SSID()
		if ssid != "" {
			if profile, ok := m.knownProfiles[ssid]; ok {
				ap.IsKnown = true
				if m.activeWifiConnection != nil {
					ap.IsActive = profile[gonetworkmanager.NmcliFieldConnectionUUID] ==
						(*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID]
					if ap.IsActive {
						ap.Interface = profile[gonetworkmanager.NmcliFieldConnectionDevice]
					}
				}
			}
		}
		items = append(items, ap)
	}

	// Sort: active first, then known, then by signal, then alphabetically
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].(wifiAP), items[j].(wifiAP)

		if a.IsActive != b.IsActive {
			return a.IsActive
		}
		if a.IsKnown != b.IsKnown {
			return a.IsKnown
		}

		// Among known, show in-range before out-of-range
		if a.IsKnown && b.IsKnown {
			aInRange, bInRange := a.Signal() > 0, b.Signal() > 0
			if aInRange != bInRange {
				return aInRange
			}
		}

		if a.Signal() != b.Signal() {
			return a.Signal() > b.Signal()
		}

		// Hidden networks last
		if a.IsHidden() != b.IsHidden() {
			return !a.IsHidden()
		}

		return strings.ToLower(a.DisplaySSID()) < strings.ToLower(b.DisplaySSID())
	})

	return items
}

func (m *model) processAndSetWifiList(apsToProcess []wifiAP) {
	m.allScannedAps = apsToProcess
	m.applyFilterAndUpdateList()
}

func (m *model) resizeComponents() {
	appHFrame := appStyle.GetHorizontalFrameSize()
	appVFrame := appStyle.GetVerticalFrameSize()
	availableWidth := m.width - appHFrame
	availableHeight := m.height - appVFrame

	// Calculate help bar width
	desiredHelpWidth := int(float64(availableWidth) * helpBarWidthPercent)
	if desiredHelpWidth > helpBarMaxWidth {
		desiredHelpWidth = helpBarMaxWidth
	}
	if desiredHelpWidth < 20 {
		desiredHelpWidth = 20
	}
	m.help.Width = desiredHelpWidth

	// Calculate content area
	headerHeight := lipgloss.Height(m.headerView(availableWidth))
	tempKeys := m.keys
	tempKeys.currentState = m.state
	footerHeight := lipgloss.Height(m.footerView(availableWidth, m.help.View(tempKeys)))
	contentHeight := availableHeight - headerHeight - footerHeight
	if contentHeight < 0 {
		contentHeight = 0
	}

	// Reserve space for filter if active
	listHeight := contentHeight
	if m.isFiltering {
		listHeight -= 4
		if listHeight < minListHeight {
			listHeight = minListHeight
		}
	}

	// Calculate list width
	listWidth := availableWidth
	if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
		calcWidth := int(float64(availableWidth) * networkListWidthPercent)
		if networkListFixedWidth > 0 && calcWidth > networkListFixedWidth {
			calcWidth = networkListFixedWidth
		}
		if calcWidth < minListWidth {
			calcWidth = minListWidth
		}
		listWidth = calcWidth
	}
	m.listDisplayWidth = listWidth

	// Apply sizes
	m.wifiList.SetSize(m.listDisplayWidth, listHeight)
	m.knownWifiList.SetSize(m.listDisplayWidth, listHeight)

	m.activeConnInfoViewport.Width = availableWidth - infoBoxStyle.GetHorizontalFrameSize()
	m.activeConnInfoViewport.Height = contentHeight - infoBoxStyle.GetVerticalFrameSize()
	if m.activeConnInfoViewport.Height < 0 {
		m.activeConnInfoViewport.Height = 0
	}

	// Password input sizing
	pwWidth := availableWidth * 2 / 3
	if pwWidth > passwordInputMaxWidth {
		pwWidth = passwordInputMaxWidth
	}
	if pwWidth < passwordInputMinWidth {
		pwWidth = passwordInputMinWidth
	}
	m.passwordInput.Width = pwWidth - lipgloss.Width(m.passwordInput.Prompt) -
		passwordInputContainerStyle.GetHorizontalFrameSize()
	m.hiddenSSIDInput.Width = m.passwordInput.Width
}

func (m *model) setStatus(msg string, style lipgloss.Style) {
	m.connectionStatusMsg = style.Render(msg)
}

func (m *model) clearStatus() {
	m.connectionStatusMsg = ""
}

func (m *model) getProfileIdentifier(ap wifiAP) string {
	// Try UUID first
	if uuid := ap.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]; uuid != "" {
		return uuid
	}
	// Fall back to connection name
	if name := ap.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName]; name != "" {
		return name
	}
	// Last resort: SSID
	return ap.SSID()
}

// =============================================================================
// Update
// =============================================================================

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	m.keys.currentState = m.state

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeComponents()
		return m, nil

	case spinner.TickMsg:
		if m.isLoading || m.isScanning {
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}

	case clearStatusMsg:
		// Only clear if we're on the main list view
		if m.state == viewNetworksList {
			m.clearStatus()
		}

	case connectionTimeoutMsg:
		if m.state == viewConnecting && m.selectedAP.SSID() == msg.ssid {
			m.isLoading = false
			m.state = viewConnectionResult
			m.lastConnectionWasSuccessful = false
			m.setStatus(fmt.Sprintf("Connection to %s timed out", msg.ssid), errorStyle)
		}

	case wifiStatusMsg:
		m.isLoading = false
		if msg.err != nil {
			if m.state == viewNetworksList {
				m.setStatus(fmt.Sprintf("Error getting Wi-Fi status: %v", msg.err), errorStyle)
				cmds = append(cmds, clearStatusAfterDelay())
			}
		} else {
			m.wifiEnabled = msg.enabled
			if m.wifiEnabled {
				m.isLoading = true
				m.isScanning = true
				m.wifiList.Title = "Scanning..."
				cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
			} else {
				m.allScannedAps = nil
				m.isScanning = false
				m.processAndSetWifiList([]wifiAP{})
				m.wifiList.Title = "Wi-Fi is Disabled"
				m.activeWifiConnection = nil
				m.activeWifiDevice = ""
				if m.state == viewNetworksList {
					m.setStatus("Wi-Fi is disabled. Press 't' to enable.", infoStyle)
				}
			}
		}

	case knownNetworksMsg:
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Error fetching profiles: %v", msg.err), errorStyle)
			cmds = append(cmds, clearStatusAfterDelay())
		} else {
			m.knownProfiles = msg.knownProfiles
			m.activeWifiConnection = msg.activeWifiConnection
			m.activeWifiDevice = msg.activeWifiDevice
		}
		if len(m.allScannedAps) > 0 {
			m.processAndSetWifiList(m.allScannedAps)
		}

	case wifiListLoadedMsg:
		m.isScanning = false
		if msg.err != nil {
			m.isLoading = false
			if m.state == viewNetworksList {
				m.setStatus(fmt.Sprintf("Error scanning: %v", msg.err), errorStyle)
				cmds = append(cmds, clearStatusAfterDelay())
			}
			m.wifiList.Title = "Error Loading Networks"
		} else if len(msg.allAps) > 0 {
			m.isLoading = false
			m.allScannedAps = msg.allAps
			m.processAndSetWifiList(m.allScannedAps)
			cmds = append(cmds, saveCachedNetworksCmd(msg.allAps))
		}

	case connectionAttemptMsg:
		m.isLoading = false
		if msg.success {
			m.state = viewConnectionResult
			m.lastConnectionWasSuccessful = true
			m.setStatus(fmt.Sprintf("Connected to %s!", m.selectedAP.DisplaySSID()), successStyle)
		} else {
			// If it was a known network attempt without password and failed, prompt for password
			if msg.wasKnownAttemptNoPsk && m.selectedAP.SSID() == msg.ssid {
				log.Printf("Known network '%s' failed, prompting for password", msg.ssid)
				m.state = viewPasswordInput
				m.passwordInput.SetValue("")
				m.passwordInput.Focus()
				m.setStatus(fmt.Sprintf("Stored credentials for %s failed. Enter password:", m.selectedAP.DisplaySSID()), warningStyle)
				cmds = append(cmds, textinput.Blink)
				return m, tea.Batch(cmds...)
			}

			m.state = viewConnectionResult
			m.lastConnectionWasSuccessful = false
			errText := "Unknown error"
			if msg.err != nil {
				errText = msg.err.Error()
			}
			m.setStatus(fmt.Sprintf("Failed to connect to %s: %s", m.selectedAP.DisplaySSID(), errText), errorStyle)
		}
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(false))

	case activeConnInfoMsg:
		m.isLoading = false
		if msg.err != nil {
			m.activeConnInfoViewport.SetContent(errorStyle.Render(fmt.Sprintf("Error: %v", msg.err)))
		} else if msg.details == nil {
			m.activeConnInfoViewport.SetContent(infoStyle.Render("No IP details available."))
		} else {
			m.activeConnInfoViewport.SetContent(formatConnectionDetails(msg.details))
		}

	case disconnectResultMsg:
		m.isLoading = false
		if msg.success {
			m.setStatus(fmt.Sprintf("Disconnected from %s", msg.ssid), successStyle)
			m.activeWifiConnection = nil
			m.activeWifiDevice = ""
		} else {
			m.setStatus(fmt.Sprintf("Error disconnecting: %v", msg.err), errorStyle)
		}
		m.state = viewNetworksList
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), clearStatusAfterDelay())

	case forgetNetworkResultMsg:
		m.isLoading = false
		if msg.success {
			m.setStatus(fmt.Sprintf("Forgot network: %s", msg.ssid), successStyle)
			delete(m.knownProfiles, msg.ssid)
		} else {
			m.setStatus(fmt.Sprintf("Error forgetting network: %v", msg.err), errorStyle)
		}
		m.state = m.previousState
		if m.state == viewKnownNetworksList {
			cmds = append(cmds, fetchKnownWifiApsCmd())
		} else {
			cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
		}
		cmds = append(cmds, clearStatusAfterDelay())

	case knownWifiApsListMsg:
		m.isLoading = false
		if msg.err != nil {
			m.setStatus(fmt.Sprintf("Error loading profiles: %v", msg.err), errorStyle)
			m.knownWifiList.Title = "Error Loading Profiles"
		} else {
			items := make([]list.Item, len(msg.aps))
			for i, ap := range msg.aps {
				items[i] = ap
			}
			m.knownWifiList.SetItems(items)
			m.knownWifiList.Title = fmt.Sprintf("Known Wi-Fi Profiles (%d)", len(items))
		}

	case tea.KeyMsg:
		cmds = append(cmds, m.handleKeyPress(msg)...)
	}

	return m, tea.Batch(cmds...)
}

func (m *model) handleKeyPress(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	// Global key handlers
	if key.Matches(msg, m.keys.Quit) {
		return []tea.Cmd{tea.Quit}
	}

	if key.Matches(msg, m.keys.Help) && m.state != viewPasswordInput && m.state != viewHiddenNetworkInput {
		m.help.ShowAll = !m.help.ShowAll
		m.resizeComponents()
		return nil
	}

	// State-specific handlers
	switch m.state {
	case viewNetworksList:
		cmds = m.handleNetworksListKeys(msg)

	case viewKnownNetworksList:
		cmds = m.handleKnownNetworksListKeys(msg)

	case viewPasswordInput:
		cmds = m.handlePasswordInputKeys(msg)

	case viewHiddenNetworkInput:
		cmds = m.handleHiddenNetworkInputKeys(msg)

	case viewConnectionResult:
		if key.Matches(msg, m.keys.Connect) || key.Matches(msg, m.keys.Back) {
			m.state = viewNetworksList
			m.clearStatus()
		}

	case viewActiveConnectionInfo:
		if key.Matches(msg, m.keys.Back) {
			m.state = viewNetworksList
			m.clearStatus()
		} else {
			m.activeConnInfoViewport, cmd = m.activeConnInfoViewport.Update(msg)
			cmds = append(cmds, cmd)
		}

	case viewConfirmDisconnect:
		cmds = m.handleConfirmDisconnectKeys(msg)

	case viewConfirmForget:
		cmds = m.handleConfirmForgetKeys(msg)

	case viewConfirmOpenNetwork:
		cmds = m.handleConfirmOpenNetworkKeys(msg)
	}

	return cmds
}

func (m *model) handleNetworksListKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	// Handle filter mode
	if m.isFiltering {
		switch {
		case key.Matches(msg, m.keys.Back) || msg.String() == "esc":
			m.isFiltering = false
			m.filterQuery = ""
			m.filterInput.SetValue("")
			m.filterInput.Blur()
			m.clearStatus()
			m.applyFilterAndUpdateList()
			m.resizeComponents()
			return nil

		case msg.String() == "enter":
			m.isFiltering = false
			m.filterInput.Blur()
			m.clearStatus()
			m.resizeComponents()
			return nil

		default:
			m.filterInput, cmd = m.filterInput.Update(msg)
			cmds = append(cmds, cmd)
			m.filterQuery = m.filterInput.Value()
			m.applyFilterAndUpdateList()
			return cmds
		}
	}

	if m.isLoading && !m.isScanning {
		return nil
	}

	switch {
	case key.Matches(msg, m.keys.Back):
		// Clear filter if active
		if m.filterQuery != "" {
			m.filterQuery = ""
			m.filterInput.SetValue("")
			m.clearStatus()
			m.applyFilterAndUpdateList()
			return nil
		}
		m.wifiList, cmd = m.wifiList.Update(msg)
		cmds = append(cmds, cmd)

	case key.Matches(msg, m.keys.ToggleHidden):
		m.showHiddenNetworks = !m.showHiddenNetworks
		m.applyFilterAndUpdateList()
		if m.showHiddenNetworks {
			m.setStatus("Showing unnamed networks", infoStyle)
		} else {
			m.setStatus("Hiding unnamed networks", infoStyle)
		}
		cmds = append(cmds, clearStatusAfterDelay())

	case key.Matches(msg, m.keys.Filter):
		m.isFiltering = true
		m.filterInput.SetValue(m.filterQuery)
		m.filterInput.Focus()
		m.setStatus("Type to filter, ESC to cancel, Enter to accept", infoStyle)
		m.resizeComponents()
		cmds = append(cmds, textinput.Blink)

	case key.Matches(msg, m.keys.Refresh):
		m.isLoading = true
		m.isScanning = true
		m.clearStatus()
		m.filterQuery = ""
		m.isFiltering = false
		m.filterInput.SetValue("")
		m.wifiList.Title = "Refreshing..."
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)

	case key.Matches(msg, m.keys.ToggleWifi):
		m.isLoading = true
		action := "OFF"
		if !m.wifiEnabled {
			action = "ON"
		}
		m.setStatus(fmt.Sprintf("Toggling Wi-Fi %s...", action), infoStyle)
		cmds = append(cmds, toggleWifiCmd(!m.wifiEnabled), m.spinner.Tick)

	case key.Matches(msg, m.keys.Disconnect):
		if m.activeWifiConnection != nil {
			ssid := gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection)
			tempAP := make(gonetworkmanager.WifiAccessPoint)
			tempAP[gonetworkmanager.NmcliFieldWifiSSID] = ssid
			m.selectedAP = wifiAP{WifiAccessPoint: tempAP, IsActive: true, IsKnown: true, Interface: m.activeWifiDevice}
			m.state = viewConfirmDisconnect
			m.clearStatus()
		} else {
			m.setStatus("Not connected to any network", infoStyle)
			cmds = append(cmds, clearStatusAfterDelay())
		}

	case key.Matches(msg, m.keys.Forget):
		if item, ok := m.wifiList.SelectedItem().(wifiAP); ok && item.IsKnown {
			m.selectedAP = item
			m.previousState = m.state
			m.state = viewConfirmForget
			m.clearStatus()
		} else if ok {
			m.setStatus(fmt.Sprintf("%s is not a known network", item.DisplaySSID()), infoStyle)
			cmds = append(cmds, clearStatusAfterDelay())
		}

	case key.Matches(msg, m.keys.Info):
		if m.activeWifiConnection != nil && m.activeWifiDevice != "" {
			m.state = viewActiveConnectionInfo
			m.isLoading = true
			m.activeConnInfoViewport.SetContent("Loading connection details...")
			m.activeConnInfoViewport.GotoTop()
			cmds = append(cmds, fetchActiveConnInfoCmd(m.activeWifiDevice), m.spinner.Tick)
			m.clearStatus()
		} else {
			m.setStatus("No active connection", infoStyle)
			cmds = append(cmds, clearStatusAfterDelay())
		}

	case key.Matches(msg, m.keys.Profiles):
		m.state = viewKnownNetworksList
		m.isLoading = true
		m.knownWifiList.Title = "Loading..."
		cmds = append(cmds, fetchKnownWifiApsCmd(), m.spinner.Tick)

	case key.Matches(msg, m.keys.HiddenSSID):
		m.state = viewHiddenNetworkInput
		m.hiddenSSIDInput.SetValue("")
		m.hiddenSSIDInput.Focus()
		m.clearStatus()
		cmds = append(cmds, textinput.Blink)

	case key.Matches(msg, m.keys.Connect):
		if item, ok := m.wifiList.SelectedItem().(wifiAP); ok {
			m.selectedAP = item
			cmds = append(cmds, m.initiateConnection(item)...)
		}

	default:
		m.wifiList, cmd = m.wifiList.Update(msg)
		cmds = append(cmds, cmd)
	}

	return cmds
}

func (m *model) initiateConnection(ap wifiAP) []tea.Cmd {
	var cmds []tea.Cmd

	ssid := ap.SSID()

	// Already connected? Offer to disconnect
	if ap.IsActive {
		m.state = viewConfirmDisconnect
		return nil
	}

	log.Printf("Initiating connection: SSID='%s', Known=%t, Open=%t", ssid, ap.IsKnown, ap.IsOpen())

	// Open network: confirm before connecting
	if ap.IsOpen() && !ap.IsKnown {
		m.state = viewConfirmOpenNetwork
		m.clearStatus()
		return nil
	}

	// Known network or open: connect directly
	if ap.IsKnown || ap.IsOpen() {
		m.isLoading = true
		m.state = viewConnecting
		m.setStatus(fmt.Sprintf("Connecting to %s...", ap.DisplaySSID()), connectingStyle)
		cmds = append(cmds, connectToWifiCmd(ssid, "", ap.IsKnown), connectionTimeoutCmd(ssid), m.spinner.Tick)
		return cmds
	}

	// Secured network: prompt for password
	m.state = viewPasswordInput
	m.passwordInput.SetValue("")
	m.passwordInput.Focus()
	m.clearStatus()
	cmds = append(cmds, textinput.Blink)
	return cmds
}

func (m *model) handleKnownNetworksListKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	if m.isLoading {
		return nil
	}

	switch {
	case key.Matches(msg, m.keys.Back):
		m.state = viewNetworksList
		m.clearStatus()

	case key.Matches(msg, m.keys.Forget):
		if item, ok := m.knownWifiList.SelectedItem().(wifiAP); ok {
			m.selectedAP = item
			m.previousState = m.state
			m.state = viewConfirmForget
			m.clearStatus()
		}

	default:
		m.knownWifiList, cmd = m.knownWifiList.Update(msg)
		cmds = append(cmds, cmd)
	}

	return cmds
}

func (m *model) handlePasswordInputKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch {
	case key.Matches(msg, m.keys.Connect):
		password := m.passwordInput.Value()
		if password == "" {
			m.setStatus("Password cannot be empty", warningStyle)
			return nil
		}
		m.isLoading = true
		m.state = viewConnecting
		ssid := m.selectedAP.SSID()
		m.setStatus(fmt.Sprintf("Connecting to %s...", m.selectedAP.DisplaySSID()), connectingStyle)
		cmds = append(cmds, connectToWifiCmd(ssid, password, false), connectionTimeoutCmd(ssid), m.spinner.Tick)

	case key.Matches(msg, m.keys.Back):
		m.state = viewNetworksList
		m.passwordInput.Blur()
		m.clearStatus()

	default:
		m.passwordInput, cmd = m.passwordInput.Update(msg)
		cmds = append(cmds, cmd)
	}

	return cmds
}

func (m *model) handleHiddenNetworkInputKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd
	var cmd tea.Cmd

	switch {
	case key.Matches(msg, m.keys.Connect):
		ssid := strings.TrimSpace(m.hiddenSSIDInput.Value())
		if ssid == "" {
			m.setStatus("SSID cannot be empty", warningStyle)
			return nil
		}

		// Create a synthetic AP for the hidden network
		tempAP := make(gonetworkmanager.WifiAccessPoint)
		tempAP[gonetworkmanager.NmcliFieldWifiSSID] = ssid
		m.selectedAP = wifiAP{WifiAccessPoint: tempAP, IsKnown: false, IsActive: false}

		// Prompt for password (assume secured)
		m.state = viewPasswordInput
		m.passwordInput.SetValue("")
		m.passwordInput.Focus()
		m.hiddenSSIDInput.Blur()
		m.clearStatus()
		cmds = append(cmds, textinput.Blink)

	case key.Matches(msg, m.keys.Back):
		m.state = viewNetworksList
		m.hiddenSSIDInput.Blur()
		m.clearStatus()

	default:
		m.hiddenSSIDInput, cmd = m.hiddenSSIDInput.Update(msg)
		cmds = append(cmds, cmd)
	}

	return cmds
}

func (m *model) handleConfirmDisconnectKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch {
	case key.Matches(msg, m.keys.Connect):
		m.isLoading = true
		ssid := m.selectedAP.DisplaySSID()
		m.setStatus(fmt.Sprintf("Disconnecting from %s...", ssid), infoStyle)

		profileID := m.getActiveConnectionProfileID()
		if profileID == "" {
			m.setStatus("Cannot identify connection to disconnect", errorStyle)
			m.isLoading = false
			m.state = viewNetworksList
			cmds = append(cmds, clearStatusAfterDelay())
			return cmds
		}

		cmds = append(cmds, disconnectWifiCmd(profileID), m.spinner.Tick)

	case key.Matches(msg, m.keys.Back):
		m.state = viewNetworksList
		m.clearStatus()
	}

	return cmds
}

func (m *model) getActiveConnectionProfileID() string {
	if m.activeWifiConnection != nil {
		if uuid := (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID]; uuid != "" {
			return uuid
		}
		if name := (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionName]; name != "" {
			return name
		}
		return gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection)
	}

	if m.selectedAP.IsActive {
		if uuid := m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]; uuid != "" {
			return uuid
		}
		if name := m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName]; name != "" {
			return name
		}
		return m.selectedAP.SSID()
	}

	return ""
}

func (m *model) handleConfirmForgetKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch {
	case key.Matches(msg, m.keys.Connect):
		m.isLoading = true
		ssid := m.selectedAP.DisplaySSID()

		profileID := m.getProfileIdentifier(m.selectedAP)
		if profileID == "" {
			m.setStatus(fmt.Sprintf("Cannot identify profile for %s", ssid), errorStyle)
			m.isLoading = false
			m.state = viewNetworksList
			cmds = append(cmds, clearStatusAfterDelay())
			return cmds
		}

		m.setStatus(fmt.Sprintf("Forgetting %s...", ssid), infoStyle)
		cmds = append(cmds, forgetNetworkCmd(profileID, ssid), m.spinner.Tick)

	case key.Matches(msg, m.keys.Back):
		m.state = m.previousState
		m.clearStatus()
	}

	return cmds
}

func (m *model) handleConfirmOpenNetworkKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd

	switch {
	case key.Matches(msg, m.keys.Connect):
		m.isLoading = true
		m.state = viewConnecting
		ssid := m.selectedAP.SSID()
		m.setStatus(fmt.Sprintf("Connecting to %s...", m.selectedAP.DisplaySSID()), connectingStyle)
		cmds = append(cmds, connectToWifiCmd(ssid, "", false), connectionTimeoutCmd(ssid), m.spinner.Tick)

	case key.Matches(msg, m.keys.Back):
		m.state = viewNetworksList
		m.clearStatus()
	}

	return cmds
}

// =============================================================================
// View
// =============================================================================

func (m model) View() string {
	availableWidth := m.width - appStyle.GetHorizontalFrameSize()

	header := m.headerView(availableWidth)
	m.keys.currentState = m.state
	helpText := m.help.View(m.keys)
	footer := m.footerView(availableWidth, helpText)

	headerHeight := lipgloss.Height(header)
	footerHeight := lipgloss.Height(footer)
	contentHeight := m.height - appStyle.GetVerticalFrameSize() - headerHeight - footerHeight
	if contentHeight < 0 {
		contentHeight = 0
	}

	var content string
	switch m.state {
	case viewNetworksList:
		content = m.renderNetworksList(availableWidth, contentHeight)
	case viewKnownNetworksList:
		content = m.renderKnownNetworksList(availableWidth, contentHeight)
	case viewPasswordInput:
		content = m.renderPasswordInput(availableWidth, contentHeight)
	case viewHiddenNetworkInput:
		content = m.renderHiddenNetworkInput(availableWidth, contentHeight)
	case viewConnecting:
		content = m.renderConnecting(availableWidth, contentHeight)
	case viewConnectionResult:
		content = m.renderConnectionResult(availableWidth, contentHeight)
	case viewActiveConnectionInfo:
		content = m.activeConnInfoViewport.View()
	case viewConfirmDisconnect:
		content = m.renderConfirmDialog("Disconnect from", availableWidth, contentHeight)
	case viewConfirmForget:
		content = m.renderConfirmDialog("Forget network", availableWidth, contentHeight)
	case viewConfirmOpenNetwork:
		content = m.renderConfirmOpenNetwork(availableWidth, contentHeight)
	}

	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top, header, content, footer))
}

func (m model) headerView(width int) string {
	title := titleStyle.Render(appName)

	// Scanning indicator
	scanIndicator := ""
	if m.isScanning {
		scanIndicator = connectingStyle.Render(" " + m.spinner.View() + " Scanning...")
	}

	// Wi-Fi status
	var status string
	if m.wifiEnabled {
		status = "Wi-Fi: " + wifiStatusEnabled.Render("Enabled ✔")
	} else {
		status = "Wi-Fi: " + wifiStatusDisabled.Render("Disabled ✘")
	}

	// Layout calculation
	titleWidth := lipgloss.Width(title)
	statusWidth := lipgloss.Width(status)
	scanWidth := lipgloss.Width(scanIndicator)

	totalWidth := titleWidth + statusWidth + scanWidth
	if totalWidth >= width {
		spacing := width - titleWidth - statusWidth
		if spacing < 1 {
			spacing = 1
		}
		return lipgloss.JoinHorizontal(lipgloss.Left, title, strings.Repeat(" ", spacing), status)
	}

	remainingSpace := width - totalWidth
	leftSpace := remainingSpace / 2
	rightSpace := remainingSpace - leftSpace

	if leftSpace < 1 {
		leftSpace = 1
	}
	if rightSpace < 1 {
		rightSpace = 1
	}

	return lipgloss.JoinHorizontal(lipgloss.Left,
		title,
		strings.Repeat(" ", leftSpace),
		scanIndicator,
		strings.Repeat(" ", rightSpace),
		status)
}

func (m model) footerView(width int, helpText string) string {
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, helpGlobalStyle.Render(helpText))
}

func (m model) renderNetworksList(width, height int) string {
	listView := m.wifiList.View()

	if m.isFiltering {
		filterView := filterInputStyle.Render(m.filterInput.View())
		listView = lipgloss.JoinVertical(lipgloss.Top, listView, "", filterView)
	}

	// Center the list if width constraints are set
	if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
		listView = lipgloss.PlaceHorizontal(width, lipgloss.Center, listView)
	}

	// Add status message if present and not loading
	if m.connectionStatusMsg != "" && !m.isLoading {
		listView = lipgloss.JoinVertical(lipgloss.Top, listView, m.connectionStatusMsg)
	}

	return listView
}

func (m model) renderKnownNetworksList(width, height int) string {
	if m.isLoading {
		spinnerView := lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View()+" ", m.knownWifiList.Title)
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, spinnerView)
	}

	listView := m.knownWifiList.View()
	if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
		listView = lipgloss.PlaceHorizontal(width, lipgloss.Center, listView)
	}
	return listView
}

func (m model) renderPasswordInput(width, height int) string {
	prompt := fmt.Sprintf("Password for %s:", m.selectedAP.DisplaySSID())
	if m.connectionStatusMsg != "" {
		prompt = m.connectionStatusMsg
	}

	promptWidth := m.passwordInput.Width + lipgloss.Width(m.passwordInput.Prompt) +
		passwordInputContainerStyle.GetHorizontalFrameSize() + 4
	if promptWidth > width*4/5 {
		promptWidth = width * 4 / 5
	}
	if promptWidth < passwordInputMinWidth {
		promptWidth = passwordInputMinWidth
	}

	centeredPrompt := lipgloss.NewStyle().Width(promptWidth).Align(lipgloss.Center).Render(prompt)
	inputView := m.passwordInput.View()

	block := lipgloss.JoinVertical(lipgloss.Top, centeredPrompt, inputView)
	if m.passwordInput.Err != nil {
		block = lipgloss.JoinVertical(lipgloss.Top, block, errorStyle.Render(m.passwordInput.Err.Error()))
	}

	content := passwordInputContainerStyle.Render(block)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (m model) renderHiddenNetworkInput(width, height int) string {
	prompt := "Enter the name of the hidden network:"

	promptWidth := m.hiddenSSIDInput.Width + lipgloss.Width(m.hiddenSSIDInput.Prompt) +
		passwordInputContainerStyle.GetHorizontalFrameSize() + 4
	if promptWidth > width*4/5 {
		promptWidth = width * 4 / 5
	}
	if promptWidth < passwordInputMinWidth {
		promptWidth = passwordInputMinWidth
	}

	centeredPrompt := lipgloss.NewStyle().Width(promptWidth).Align(lipgloss.Center).Render(prompt)
	inputView := m.hiddenSSIDInput.View()

	block := lipgloss.JoinVertical(lipgloss.Top, centeredPrompt, inputView)
	content := passwordInputContainerStyle.Render(block)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (m model) renderConnecting(width, height int) string {
	content := connectingStyle.Render(fmt.Sprintf("\n%s %s\n", m.spinner.View(), m.connectionStatusMsg))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (m model) renderConnectionResult(width, height int) string {
	msgWidth := width * 3 / 4
	if msgWidth > 80 {
		msgWidth = 80
	}
	if msgWidth < 40 {
		msgWidth = 40
	}

	wrappedMsg := lipgloss.NewStyle().Width(msgWidth).Align(lipgloss.Center).Render(m.connectionStatusMsg)
	hint := lipgloss.NewStyle().Foreground(colorFaint).Render("(Press Enter or Esc to return)")

	content := lipgloss.JoinVertical(lipgloss.Center, wrappedMsg, "", hint)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (m model) renderConfirmDialog(action string, width, height int) string {
	message := fmt.Sprintf("%s\n%s?", action, m.selectedAP.DisplaySSID())
	hint := lipgloss.NewStyle().Foreground(colorFaint).Render("(Enter to confirm, Esc to cancel)")

	content := lipgloss.JoinVertical(lipgloss.Center, message, "", hint)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (m model) renderConfirmOpenNetwork(width, height int) string {
	warning := warningStyle.Render("⚠️  This is an open (unencrypted) network")
	message := fmt.Sprintf("Connect to %s?", m.selectedAP.DisplaySSID())
	hint := lipgloss.NewStyle().Foreground(colorFaint).Render("(Enter to confirm, Esc to cancel)")

	content := lipgloss.JoinVertical(lipgloss.Center, warning, "", message, "", hint)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func formatConnectionDetails(details *gonetworkmanager.DeviceIPDetail) string {
	lines := []string{
		fmt.Sprintf("Device:      %s (%s)", details.Device, details.Type),
		fmt.Sprintf("State:       %s", details.State),
		fmt.Sprintf("Connection:  %s", details.Connection),
		fmt.Sprintf("MAC Address: %s", details.Mac),
		"",
		"IPv4:",
		fmt.Sprintf("  Address:   %s", details.IPv4),
		fmt.Sprintf("  Netmask:   %s", details.NetV4),
		fmt.Sprintf("  Gateway:   %s", details.GatewayV4),
		fmt.Sprintf("  DNS:       %s", strings.Join(details.DNS, ", ")),
	}

	if details.IPv6 != "" {
		lines = append(lines, "",
			"IPv6:",
			fmt.Sprintf("  Address:   %s", details.IPv6),
			fmt.Sprintf("  Prefix:    %s", details.NetV6),
			fmt.Sprintf("  Gateway:   %s", details.GatewayV6))
	}

	return strings.Join(lines, "\n")
}

// =============================================================================
// Main
// =============================================================================

func main() {
	// Panic recovery
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "Application crashed: %v\n", r)
			os.Exit(1)
		}
	}()

	// Check for nmcli
	if err := checkNmcliAvailable(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		fmt.Fprintln(os.Stderr, "This application requires NetworkManager to function.")
		os.Exit(1)
	}

	// Setup logging
	logFile, err := tea.LogToFile(debugLogFile, "debug")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Could not create log file: %v\n", err)
	} else {
		defer logFile.Close()
	}

	// Run the application
	program := tea.NewProgram(initialModel(), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running application: %v\n", err)
		os.Exit(1)
	}
}

func checkNmcliAvailable() error {
	// Check common location first
	if _, err := os.Stat("/usr/bin/nmcli"); err == nil {
		return nil
	}

	// Try running nmcli
	cmd := exec.Command("nmcli", "--version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("'nmcli' is not installed or not found in PATH")
	}

	return nil
}