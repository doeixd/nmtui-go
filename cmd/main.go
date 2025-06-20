
// Package main implements a Terminal User Interface (TUI) for managing NetworkManager Wi-Fi connections.
// It allows users to scan for networks, connect to secured and open networks, view connection details,
// manage known profiles, and toggle Wi-Fi radio status.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	// "time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nmtui/gonetworkmanager" // Local package for NetworkManager interactions
)

const (
	debugLogFile        = "nmtui-debug.log" // Log file used when DEBUG_TEA environment variable is set.
	helpBarMaxWidth     = 80                // Maximum character width for the help bar.
	helpBarWidthPercent = 0.80              // Help bar width as a percentage of screen width, if less than helpBarMaxWidth.
	// Optional: Desired width for the network list if centering is enabled.
	networkListFixedWidth = 100 // Example: Max fixed width for the list.
	networkListWidthPercent = 0.85 // Example: List width as a percentage of available width.
)

// --- Styles Definition ---
var (
	// Base application style with margin.
	appStyle = lipgloss.NewStyle().Margin(1, 1)

	// ANSI Color Palette for Theme Integration
	ansPrimaryColor   = lipgloss.Color("5") // Magenta - for primary selections, highlights
	ansSecondaryColor = lipgloss.Color("4") // Blue - for secondary elements, like list title background
	ansAccentColor    = lipgloss.Color("6") // Cyan - for accents, cursors, spinners
	ansSuccessColor   = lipgloss.Color("2") // Green - for success states
	ansErrorColor     = lipgloss.Color("1") // Red - for error states
	ansFaintTextColor = lipgloss.Color("8") // Bright Black (Dark Gray) - for faint/secondary text
	ansTextColor      = lipgloss.Color("7") // White - for general text (often terminal default is similar)
	ansBgColor        = lipgloss.Color("40") // White - for general text (often terminal default is similar)

	adaptiveTitleForegroundColor = lipgloss.AdaptiveColor{
			Light: "235",
			Dark: "235",
	}
	// Text and Title Styles
	titleStyle            = lipgloss.NewStyle().Bold(true).Foreground(ansPrimaryColor).Padding(0, 1).MarginBottom(1)
	listTitleStyle        = lipgloss.NewStyle().Background(ansSecondaryColor).Foreground(adaptiveTitleForegroundColor).Padding(0, 1).Bold(true)
	listItemStyle         = lipgloss.NewStyle().PaddingLeft(2).Foreground(ansTextColor)
	listSelectedItemStyle = lipgloss.NewStyle().PaddingLeft(1).Foreground(ansPrimaryColor).Bold(true)
	listDescStyle         = lipgloss.NewStyle().PaddingLeft(2).Foreground(ansFaintTextColor)
	listSelectedDescStyle = lipgloss.NewStyle().PaddingLeft(1).Foreground(ansPrimaryColor)
	listNoItemsStyle      = lipgloss.NewStyle().Faint(true).Margin(1, 0).Align(lipgloss.Center).Foreground(ansFaintTextColor)

	// Status and Messaging Styles
	statusMessageBaseStyle     = lipgloss.NewStyle().MarginTop(1)
	errorStyle                 = statusMessageBaseStyle.Copy().Foreground(ansErrorColor).Bold(true)
	connectingStyle            = lipgloss.NewStyle().Foreground(ansAccentColor)
	successStyle               = statusMessageBaseStyle.Copy().Foreground(ansSuccessColor).Bold(true)
	infoBoxStyle               = lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true).BorderForeground(ansAccentColor).Padding(1, 2).MarginTop(1)
	toggleHiddenStatusMsgStyle = statusMessageBaseStyle.Copy().Foreground(ansFaintTextColor)

	// Password Input Styles
	passwordPromptStyle         = lipgloss.NewStyle().Foreground(ansFaintTextColor)
	passwordInputContainerStyle = lipgloss.NewStyle().Padding(1).MarginTop(1).Border(lipgloss.NormalBorder(), true).BorderForeground(ansFaintTextColor)

	// Help Bar Styles
	helpGlobalStyle = lipgloss.NewStyle().Foreground(ansFaintTextColor)

	// Specific Indicator Styles
	wifiStatusStyleEnabled     = lipgloss.NewStyle().Foreground(ansSuccessColor)
	wifiStatusStyleDisabled    = lipgloss.NewStyle().Foreground(ansErrorColor)
	listTitleHiddenStatusStyle = lipgloss.NewStyle().Foreground(ansFaintTextColor).Italic(true)
)

// viewState defines the current screen/context of the application.
type viewState int

const (
	viewNetworksList viewState = iota
	viewPasswordInput
	viewConnecting
	viewConnectionResult
	viewActiveConnectionInfo
	viewConfirmDisconnect
)

// itemDelegate handles rendering of individual Wi-Fi APs in the list.
type itemDelegate struct{}

func (d itemDelegate) Height() int                               { return 2 }
func (d itemDelegate) Spacing() int                              { return 1 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(wifiAP)
	if !ok {
		return
	}
	var title, desc string
	if index == m.Index() {
		title = listSelectedItemStyle.Render("▸ " + i.StyledTitle())
		desc = listSelectedDescStyle.Render("  " + i.Description())
	} else {
		title = listItemStyle.Render("  " + i.StyledTitle())
		desc = listDescStyle.Render("  " + i.Description())
	}
	fmt.Fprintf(w, "%s\n%s", title, desc)
}

// wifiAP represents a Wi-Fi Access Point with TUI-specific display properties.
type wifiAP struct {
	gonetworkmanager.WifiAccessPoint
	IsKnown   bool
	IsActive  bool
	Interface string
}

func (ap wifiAP) getSSIDFromScannedAP() string {
	if ap.WifiAccessPoint == nil {
		return ""
	}
	return ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSSID]
}

func (ap wifiAP) StyledTitle() string {
	ssid := ap.getSSIDFromScannedAP()
	if ssid == "" || ssid == "--" {
		ssid = "<Hidden Network>"
	}
	indicator := ""
	if ap.IsActive {
		indicator += lipgloss.NewStyle().Foreground(ansSuccessColor).Render(" ✔")
	}
	if ap.IsKnown && !ap.IsActive {
		indicator += lipgloss.NewStyle().Foreground(ansAccentColor).Render(" ★")
	}
	return fmt.Sprintf("%s%s", ssid, indicator)
}

func (ap wifiAP) Title() string { return ap.StyledTitle() }

func (ap wifiAP) Description() string {
	signalStr, security := "", ""
	if ap.WifiAccessPoint != nil {
		signalStr = ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal]
		security = ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSecurity]
	}

	descParts := []string{}
	labelStyle := lipgloss.NewStyle().Foreground(ansFaintTextColor)

	if signalStr != "" {
		signalVal, _ := strconv.Atoi(signalStr)
		var signalValueStyle lipgloss.Style
		switch {
		case signalVal > 70:
			signalValueStyle = lipgloss.NewStyle().Foreground(ansSuccessColor)
		case signalVal > 40:
			signalValueStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		default:
			signalValueStyle = lipgloss.NewStyle().Foreground(ansErrorColor)
		}
		descParts = append(descParts, fmt.Sprintf("%s %s", labelStyle.Render("Signal:"), signalValueStyle.Render(signalStr+"%%")))
	}

	if security == "" {
		security = "Open"
	}
	descParts = append(descParts, fmt.Sprintf("%s %s", labelStyle.Render("Security:"), labelStyle.Render(security)))

	separator := labelStyle.Render(" | ")
	return strings.Join(descParts, separator)
}

func (ap wifiAP) FilterValue() string {
	ssid := ap.getSSIDFromScannedAP()
	if ssid == "" || ssid == "--" {
		return "<Hidden Network>"
	}
	return ssid
}

// --- tea.Msg Definitions ---
type wifiListLoadedMsg struct{ allAps []wifiAP; err error }
type connectionAttemptMsg struct {
	ssid    string
	success bool
	err     error
}
type wifiStatusMsg struct{ enabled bool; err error }
type knownNetworksMsg struct {
	knownProfiles        map[string]gonetworkmanager.ConnectionProfile
	activeWifiConnection *gonetworkmanager.ConnectionProfile
	activeWifiDevice     string
}
type activeConnInfoMsg struct{ details *gonetworkmanager.DeviceIPDetail; err error }
type disconnectResultMsg struct{ success bool; err error; ssid string }

type keyMap struct {
	Connect, Refresh, Quit, Back, Help, Filter, ToggleWifi, Disconnect, Info, ToggleHidden key.Binding
	currentState                                                                           viewState
}

func (k keyMap) ShortHelp() []key.Binding {
	var bindings []key.Binding
	bindings = append(bindings, k.Help)
	switch k.currentState {
	case viewNetworksList:
		bindings = append(bindings, k.Connect, k.Refresh, k.Filter, k.ToggleWifi, k.Disconnect, k.Info, k.ToggleHidden)
	case viewPasswordInput, viewConnectionResult, viewConfirmDisconnect:
		bindings = append(bindings, k.Connect, k.Back)
	case viewConnecting:
		break
	case viewActiveConnectionInfo:
		bindings = append(bindings, k.Back)
	}
	bindings = append(bindings, k.Quit)
	return bindings
}

func (k keyMap) FullHelp() [][]key.Binding {
	allBindings := []key.Binding{
		k.Help, k.Connect, k.Back, k.Refresh, k.Filter, k.ToggleHidden, k.ToggleWifi,
		k.Disconnect, k.Info, k.Quit,
	}
	return [][]key.Binding{allBindings}
}

var defaultKeyBindings = keyMap{
	Connect:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select/connect/confirm")),
	Refresh:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh list")),
	Quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q/ctrl+c", "quit")),
	Back:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back/cancel")),
	Help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "toggle help detail")),
	Filter:       key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter list")),
	ToggleWifi:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle Wi-Fi radio")),
	Disconnect:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "disconnect active")),
	Info:         key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "active connection info")),
	ToggleHidden: key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "show/hide unnamed nets")),
}

type model struct {
	state                       viewState
	wifiList                    list.Model
	passwordInput               textinput.Model
	spinner                     spinner.Model
	activeConnInfoViewport      viewport.Model
	selectedAP                  wifiAP
	connectionStatusMsg         string
	lastConnectionWasSuccessful bool // Used to tailor viewConnectionResult text in View()
	wifiEnabled                 bool
	knownProfiles               map[string]gonetworkmanager.ConnectionProfile
	activeWifiConnection        *gonetworkmanager.ConnectionProfile
	activeWifiDevice            string
	allScannedAps               []wifiAP
	showHiddenNetworks          bool
	isLoading                   bool
	width, height               int
	keys                        keyMap
	help                        help.Model
}

func initialModel() model {
	delegate := itemDelegate{}
	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Scanning for Wi-Fi Networks..."
	l.Styles.Title = listTitleStyle
	l.SetShowStatusBar(true); l.SetStatusBarItemName("network", "networks")
	l.SetShowHelp(false); l.DisableQuitKeybindings()
	l.Styles.NoItems = listNoItemsStyle.Copy().SetString("No Wi-Fi networks. Try (r)efresh, (t)oggle Wi-Fi, or (u)nnamed nets.")
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{defaultKeyBindings.Filter, defaultKeyBindings.Refresh, defaultKeyBindings.ToggleHidden}
	}
	l.AdditionalFullHelpKeys = l.AdditionalShortHelpKeys

	ti := textinput.New()
	ti.Placeholder = "Network Password"
	ti.EchoMode = textinput.EchoPassword; ti.CharLimit = 63
	ti.Prompt = passwordPromptStyle.Render("🔑 Password: ")
	ti.EchoCharacter = '•'
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(ansAccentColor)

	s := spinner.New(); s.Spinner = spinner.Globe; s.Style = connectingStyle
	vp := viewport.New(0, 0); vp.Style = infoBoxStyle.Copy()
	h := help.New(); h.ShowAll = false
	subtleHelpItemStyle := lipgloss.NewStyle().Foreground(ansFaintTextColor)
	h.Styles.ShortKey = subtleHelpItemStyle
	h.Styles.ShortDesc = subtleHelpItemStyle
	h.Styles.FullKey = subtleHelpItemStyle
	h.Styles.FullDesc = subtleHelpItemStyle
	h.Styles.Ellipsis = subtleHelpItemStyle.Copy()

	m := model{
		state:                       viewNetworksList,
		wifiList:                    l,
		passwordInput:               ti,
		spinner:                     s,
		activeConnInfoViewport:      vp,
		isLoading:                   true,
		keys:                        defaultKeyBindings,
		help:                        h,
		knownProfiles:               make(map[string]gonetworkmanager.ConnectionProfile),
		showHiddenNetworks:          false,
		lastConnectionWasSuccessful: false,
	}
	m.keys.currentState = m.state
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(getWifiStatusInternalCmd(), fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
}

func fetchWifiNetworksCmd(rescan bool) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Fetching Wi-Fi networks (rescan: %t)...", rescan)
		apsRaw, err := gonetworkmanager.GetWifiList(rescan)
		var aps []wifiAP
		if err == nil {
			aps = make([]wifiAP, len(apsRaw))
			for i, rawApData := range apsRaw {
				aps[i] = wifiAP{WifiAccessPoint: rawApData}
			}
			log.Printf("Cmd: Fetched %d Wi-Fi networks.", len(apsRaw))
		} else {
			log.Printf("Cmd: Error fetching Wi-Fi list: %v", err)
		}
		return wifiListLoadedMsg{allAps: aps, err: err}
	}
}

func connectToWifiCmd(ssid, password string) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Attempting connect to SSID: '%s'", ssid)
		_, err := gonetworkmanager.ConnectToWifiRobustly(ssid, "*", ssid, password, false)
		if err != nil {
			log.Printf("Cmd: Connect error for '%s': %v", ssid, err)
		} else {
			log.Printf("Cmd: Connect command for '%s' appears successful.", ssid)
		}
		return connectionAttemptMsg{ssid: ssid, success: err == nil, err: err}
	}
}

func getWifiStatusInternalCmd() tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Getting Wi-Fi status...")
		status, err := gonetworkmanager.GetWifiStatus()
		enabled := false; if err == nil && status == "enabled" { enabled = true }
		if err != nil { log.Printf("Cmd: Error getting Wi-Fi status: %v", err) }
		return wifiStatusMsg{enabled: enabled, err: err}
	}
}

func toggleWifiCmd(enable bool) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Toggling Wi-Fi to %t...", enable)
		var err error
		if enable { _, err = gonetworkmanager.WifiEnable()
		} else { _, err = gonetworkmanager.WifiDisable() }
		if err != nil { log.Printf("Cmd: Error toggling Wi-Fi: %v", err); return wifiStatusMsg{enabled: !enable, err: err} }
		return wifiStatusMsg{enabled: enable, err: nil}
	}
}

func fetchKnownNetworksCmd() tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Fetching known networks...")
		profiles, err := gonetworkmanager.GetConnectionProfilesList(false)
		if err != nil { log.Printf("Cmd: Error fetching known profiles: %v", err); return knownNetworksMsg{} }
		known := make(map[string]gonetworkmanager.ConnectionProfile); var activeConn *gonetworkmanager.ConnectionProfile; var activeDev string
		activeDeviceProfiles, _ := gonetworkmanager.GetConnectionProfilesList(true)
		activeUUIDs := make(map[string]struct{})
		for _, adp := range activeDeviceProfiles { if adp[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi { activeUUIDs[adp[gonetworkmanager.NmcliFieldConnectionUUID]] = struct{}{} } }
		for _, p := range profiles {
			if p[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi {
				ssid := gonetworkmanager.GetSSIDFromProfile(p)
				if ssid != "" { known[ssid] = p
					if _, isActive := activeUUIDs[p[gonetworkmanager.NmcliFieldConnectionUUID]]; isActive {
						profileCopy := make(gonetworkmanager.ConnectionProfile); for k,v := range p { profileCopy[k]=v }; activeConn = &profileCopy; activeDev = p[gonetworkmanager.NmcliFieldConnectionDevice]
					}
				}
			}
		}
		log.Printf("Cmd: Found %d known Wi-Fi profiles. Active: %v", len(known), activeConn != nil)
		return knownNetworksMsg{knownProfiles: known, activeWifiConnection: activeConn, activeWifiDevice: activeDev}
	}
}

func fetchActiveConnInfoCmd(deviceName string) tea.Cmd {
	return func() tea.Msg {
		if deviceName == "" { log.Printf("Cmd: fetchActiveConnInfoCmd called with no device name."); return activeConnInfoMsg{nil, fmt.Errorf("no active Wi-Fi device specified")} }
		log.Printf("Cmd: Fetching IP details for device: %s", deviceName)
		details, err := gonetworkmanager.GetDeviceInfoIPDetail(deviceName)
		if err != nil { log.Printf("Cmd: Error fetching IP details for %s: %v", deviceName, err) }
		return activeConnInfoMsg{details: details, err: err}
	}
}

func disconnectWifiCmd(profileIdentifier string) tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Attempting to disconnect profile: %s", profileIdentifier)
		_, err := gonetworkmanager.ConnectionDown(profileIdentifier)
		if err != nil { log.Printf("Cmd: Error disconnecting %s: %v", profileIdentifier, err) }
		return disconnectResultMsg{success: err == nil, err: err, ssid: profileIdentifier}
	}
}

func (m *model) processAndSetWifiList(apsToProcess []wifiAP) {
	var filteredAps []wifiAP
	for _, ap := range apsToProcess {
		ssid := ap.getSSIDFromScannedAP()
		isEffectivelyUnnamed := ssid == "" || ssid == "--"
		if m.showHiddenNetworks || !isEffectivelyUnnamed {
			filteredAps = append(filteredAps, ap)
		}
	}

	enrichedAps := make([]list.Item, len(filteredAps)); foundActiveInScan := false
	for i, ap := range filteredAps {
		scannedAPSSID := ap.getSSIDFromScannedAP(); ap.IsKnown, ap.IsActive = false, false
		if scannedAPSSID != "" && scannedAPSSID != "--" {
			if profile, ok := m.knownProfiles[scannedAPSSID]; ok {
				ap.IsKnown = true
				if m.activeWifiConnection != nil && profile[gonetworkmanager.NmcliFieldConnectionUUID] == (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID] {
					ap.IsActive = true; ap.Interface = profile[gonetworkmanager.NmcliFieldConnectionDevice]; foundActiveInScan = true
				}
			}
		}
		enrichedAps[i] = ap
	}

	if !foundActiveInScan && m.activeWifiConnection != nil { log.Println("ProcessList: Active connection not found in current scan.") }

	sort.SliceStable(enrichedAps, func(i, j int) bool {
		itemI, itemJ := enrichedAps[i].(wifiAP), enrichedAps[j].(wifiAP)
		if itemI.IsActive != itemJ.IsActive { return itemI.IsActive }
		if itemI.IsKnown != itemJ.IsKnown { return itemI.IsKnown }
		sigI, _ := strconv.Atoi(itemI.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
		sigJ, _ := strconv.Atoi(itemJ.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
		if sigI != sigJ { return sigI > sigJ }
		ssidI, ssidJ := strings.ToLower(itemI.getSSIDFromScannedAP()), strings.ToLower(itemJ.getSSIDFromScannedAP())
		if (ssidI == "" || ssidI == "--") && (ssidJ != "" && ssidJ != "--") { return false }
		if (ssidJ == "" || ssidJ == "--") && (ssidI != "" && ssidI != "--") { return true }
		return ssidI < ssidJ
	})

	m.wifiList.SetItems(enrichedAps)
	hiddenStatusText := ""; if !m.showHiddenNetworks { hiddenStatusText = listTitleHiddenStatusStyle.Render(" (hiding unnamed)") }
	m.wifiList.Title = fmt.Sprintf("Available Wi-Fi Networks (%d found)%s", len(enrichedAps), hiddenStatusText)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd; var cmd tea.Cmd; m.keys.currentState = m.state

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		availableWidth := m.width - appStyle.GetHorizontalFrameSize()
		availableHeight := m.height - appStyle.GetVerticalFrameSize()
		desiredHelpWidth := int(float64(availableWidth) * helpBarWidthPercent)
		if desiredHelpWidth > helpBarMaxWidth { desiredHelpWidth = helpBarMaxWidth }
		if desiredHelpWidth < 20 { desiredHelpWidth = 20 }
		m.help.Width = desiredHelpWidth
		headerHeight := lipgloss.Height(m.headerView(availableWidth))
		footerHeight := lipgloss.Height(m.footerView(availableWidth, m.help.View(m.keys)))
		contentAreaHeight := availableHeight - headerHeight - footerHeight
		if contentAreaHeight < 0 { contentAreaHeight = 0 }
		listWidthForListModel := availableWidth
		if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
			calculatedWidth := availableWidth
			if networkListWidthPercent > 0 { calculatedWidth = int(float64(availableWidth) * networkListWidthPercent) }
			if networkListFixedWidth > 0 && calculatedWidth > networkListFixedWidth { calculatedWidth = networkListFixedWidth }
			if calculatedWidth < 40 { calculatedWidth = 40 }
			listWidthForListModel = calculatedWidth
		}
		m.wifiList.SetSize(listWidthForListModel, contentAreaHeight)
		m.activeConnInfoViewport.Width = availableWidth - infoBoxStyle.GetHorizontalFrameSize()
		m.activeConnInfoViewport.Height = contentAreaHeight - infoBoxStyle.GetVerticalFrameSize()
		pwInputContentWidth := availableWidth * 2/3
		if pwInputContentWidth > 60 { pwInputContentWidth = 60 }
		if pwInputContentWidth < 40 { pwInputContentWidth = 40 }
		m.passwordInput.Width = pwInputContentWidth - lipgloss.Width(m.passwordInput.Prompt) - passwordInputContainerStyle.GetHorizontalFrameSize()

	case wifiStatusMsg:
		if msg.err != nil {
			if m.state == viewNetworksList { // Only update status message if in the main list view
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error Wi-Fi status: %v", msg.err))
			}
		} else {
			m.wifiEnabled = msg.enabled
			statusText := "enabled"; if !m.wifiEnabled { statusText = "disabled" }
			if m.state == viewNetworksList { // Only update status message if in the main list view
				m.connectionStatusMsg = fmt.Sprintf("Wi-Fi is %s.", statusText)
			}
			if m.wifiEnabled {
				cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
			} else {
				m.allScannedAps = nil; m.processAndSetWifiList([]wifiAP{}); m.wifiList.Title = "Wi-Fi is Disabled"
				m.activeWifiConnection = nil; m.activeWifiDevice = ""
				if m.state == viewNetworksList { // Clear status if disabling Wi-Fi and in list view
					m.connectionStatusMsg = "Wi-Fi is disabled."
				}
			}
		}
	case knownNetworksMsg:
		m.knownProfiles = msg.knownProfiles
		m.activeWifiConnection = msg.activeWifiConnection
		m.activeWifiDevice = msg.activeWifiDevice
		if m.allScannedAps != nil {
			m.processAndSetWifiList(m.allScannedAps)
		}
		// This message does not typically set a global status message that would conflict.

	case wifiListLoadedMsg:
		m.isLoading = false
		if msg.err != nil {
			if m.state == viewNetworksList { // Only update status message if in the main list view
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error fetching Wi-Fi: %v", msg.err))
			}
			m.wifiList.Title = "Error Loading Networks"
		} else {
			m.allScannedAps = msg.allAps
			m.processAndSetWifiList(m.allScannedAps)
			if m.state == viewNetworksList { // Only clear status message if in the main list view
				m.connectionStatusMsg = ""
			}
		}

	case connectionAttemptMsg:
		m.isLoading = false
		m.state = viewConnectionResult
		m.lastConnectionWasSuccessful = msg.success // Store success state for View rendering

		if msg.success {
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Successfully connected to %s!", m.selectedAP.StyledTitle()))
		} else {
			errorText := "Unknown error during connection attempt."
			if msg.err != nil {
				errorText = msg.err.Error()
				log.Printf("Connection Error Received in TUI: %v", msg.err) // Log the raw error
			}
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Failed to connect to %s: %s", m.selectedAP.StyledTitle(), errorText))
		}
		// Refresh network state regardless of success/failure
		// These commands might trigger wifiListLoadedMsg or knownNetworksMsg,
		// their handlers are now guarded not to clear connectionStatusMsg if state is viewConnectionResult.
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(false))


	case activeConnInfoMsg:
		m.isLoading = false
		// This view manages its own content, connectionStatusMsg is less relevant here for global status
		if msg.err != nil {
			m.activeConnInfoViewport.SetContent(errorStyle.Render(fmt.Sprintf("Error active info: %v", msg.err)))
		} else if msg.details == nil {
			m.activeConnInfoViewport.SetContent(toggleHiddenStatusMsgStyle.Render("No IP details for active connection."))
		} else {
			info := []string{fmt.Sprintf("Device: %s (%s)", msg.details.Device, msg.details.Type), fmt.Sprintf("State: %s", msg.details.State), fmt.Sprintf("Connection: %s", msg.details.Connection), fmt.Sprintf("MAC: %s", msg.details.Mac), fmt.Sprintf("IPv4: %s (%s)", msg.details.IPv4, msg.details.NetV4), fmt.Sprintf("Gateway v4: %s", msg.details.GatewayV4), fmt.Sprintf("DNS: %s", strings.Join(msg.details.DNS, ", "))}
			if msg.details.IPv6 != "" {
				info = append(info, fmt.Sprintf("IPv6: %s (%s)", msg.details.IPv6, msg.details.NetV6), fmt.Sprintf("Gateway v6: %s", msg.details.GatewayV6))
			}
			m.activeConnInfoViewport.SetContent(strings.Join(info, "\n"))
		}
	case disconnectResultMsg:
		m.isLoading = false
		if msg.success {
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Disconnected from %s.", msg.ssid))
			m.activeWifiConnection = nil; m.activeWifiDevice = ""
		} else {
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error disconnecting from %s: %v", msg.ssid, msg.err))
		}
		m.state = viewNetworksList // Return to list view
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))

	case tea.KeyMsg:
		if m.state == viewNetworksList && m.wifiList.FilterState() == list.Filtering {
			switch { case key.Matches(msg, m.keys.Back): m.wifiList.FilterInput.Blur(); m.wifiList.ResetFilter(); if m.state == viewNetworksList { m.connectionStatusMsg = ""} ; case msg.Type == tea.KeyEnter: m.wifiList.FilterInput.Blur(); if m.state == viewNetworksList {m.connectionStatusMsg = ""}; default: m.wifiList, cmd = m.wifiList.Update(msg); cmds = append(cmds, cmd) }; return m, tea.Batch(cmds...)
		}
		if key.Matches(msg, m.keys.Quit) { return m, tea.Quit }
		if key.Matches(msg, m.keys.Help) { if m.state != viewPasswordInput { m.help.ShowAll = !m.help.ShowAll } }

		switch m.state {
		case viewNetworksList:
			if m.isLoading { break }
			switch {
			case key.Matches(msg, m.keys.ToggleHidden):
				m.showHiddenNetworks = !m.showHiddenNetworks; if m.allScannedAps != nil { m.processAndSetWifiList(m.allScannedAps) }
				if m.showHiddenNetworks { m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Showing unnamed networks.")
				} else { m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Hiding unnamed networks.") }
			case key.Matches(msg, m.keys.Filter): m.wifiList.FilterInput.Focus(); m.connectionStatusMsg = "Filtering... (Esc to cancel, Enter to apply)"
			case key.Matches(msg, m.keys.Refresh): m.isLoading = true; m.connectionStatusMsg = ""; m.allScannedAps = nil; m.processAndSetWifiList([]wifiAP{}); m.wifiList.Title = "Refreshing..."; cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
			case key.Matches(msg, m.keys.ToggleWifi): m.isLoading = true; actionStr := "OFF"; if !m.wifiEnabled { actionStr = "ON" }; m.connectionStatusMsg = fmt.Sprintf("Toggling Wi-Fi to %s...", actionStr); cmds = append(cmds, toggleWifiCmd(!m.wifiEnabled))
			case key.Matches(msg, m.keys.Disconnect): if m.activeWifiConnection != nil { activeSSID := gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection); tempAPData := make(gonetworkmanager.WifiAccessPoint); tempAPData[gonetworkmanager.NmcliFieldWifiSSID] = activeSSID; if m.activeWifiConnection != nil { for k,v := range *m.activeWifiConnection { if _,exists := tempAPData[k]; !exists {tempAPData[k]=v}}} ; m.selectedAP = wifiAP{ WifiAccessPoint: tempAPData, IsActive: true, IsKnown: true, Interface: m.activeWifiDevice }; m.state = viewConfirmDisconnect } else { m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Not connected to any Wi-Fi network.") }
			case key.Matches(msg, m.keys.Info): if m.activeWifiConnection != nil && m.activeWifiDevice != "" { m.state = viewActiveConnectionInfo; m.isLoading = true; m.activeConnInfoViewport.SetContent("Loading connection details..."); m.activeConnInfoViewport.GotoTop(); cmds = append(cmds, fetchActiveConnInfoCmd(m.activeWifiDevice)) } else { m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("No active Wi-Fi connection to show info for.") }
			case key.Matches(msg, m.keys.Connect): if item, ok := m.wifiList.SelectedItem().(wifiAP); ok { m.selectedAP = item; ssidToConnect := item.getSSIDFromScannedAP(); if item.IsActive { m.state = viewConfirmDisconnect; break }; security := ""; if item.WifiAccessPoint != nil { security = strings.ToLower(item.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSecurity]) }; isOpenNetwork := security == "" || security == "open" || security == "--"; if isOpenNetwork || item.IsKnown { m.isLoading = true; m.state = viewConnecting; m.connectionStatusMsg = fmt.Sprintf("Connecting to %s...", item.StyledTitle()); cmds = append(cmds, connectToWifiCmd(ssidToConnect, "")) } else { m.state = viewPasswordInput; m.passwordInput.SetValue(""); m.passwordInput.Focus(); cmds = append(cmds, textinput.Blink) } }
			}
		case viewPasswordInput:
			switch { case key.Matches(msg, m.keys.Connect): m.isLoading = true; m.state = viewConnecting; m.connectionStatusMsg = fmt.Sprintf("Connecting to %s...", m.selectedAP.StyledTitle()); cmds = append(cmds, connectToWifiCmd(m.selectedAP.getSSIDFromScannedAP(), m.passwordInput.Value())); case key.Matches(msg, m.keys.Back): m.state = viewNetworksList; m.passwordInput.Blur(); m.connectionStatusMsg = "" }
		case viewConnectionResult:
			if key.Matches(msg, m.keys.Connect) || key.Matches(msg, m.keys.Back) {
				m.state = viewNetworksList
				m.connectionStatusMsg = "" // Clear the specific connection result message
			}
		case viewActiveConnectionInfo:
			if key.Matches(msg, m.keys.Back) { m.state = viewNetworksList; m.connectionStatusMsg = "" }
		case viewConfirmDisconnect:
			switch { case key.Matches(msg, m.keys.Connect): m.isLoading = true; m.connectionStatusMsg = fmt.Sprintf("Disconnecting from %s...", m.selectedAP.StyledTitle()); var profileToDisconnect string; if m.activeWifiConnection != nil { profileToDisconnect = (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionName]; if profileToDisconnect == "" { profileToDisconnect = (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID] }; if profileToDisconnect == "" { profileToDisconnect = gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection) } } else { profileToDisconnect = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName]; if profileToDisconnect == "" { profileToDisconnect = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID] }; if profileToDisconnect == "" { profileToDisconnect = m.selectedAP.getSSIDFromScannedAP() } }; if profileToDisconnect == "" { m.connectionStatusMsg = errorStyle.Render("Cannot identify profile to disconnect."); m.isLoading = false; m.state = viewNetworksList; break }; cmds = append(cmds, disconnectWifiCmd(profileToDisconnect)); case key.Matches(msg, m.keys.Back): m.state = viewNetworksList; m.connectionStatusMsg = "" }
		}
	}

	if m.isLoading { m.spinner, cmd = m.spinner.Update(msg); cmds = append(cmds, cmd) }
	if !m.isLoading || (m.state == viewNetworksList && m.wifiList.FilterState() == list.Filtering) {
		switch m.state {
		case viewNetworksList: m.wifiList, cmd = m.wifiList.Update(msg); cmds = append(cmds, cmd)
		case viewPasswordInput: m.passwordInput, cmd = m.passwordInput.Update(msg); cmds = append(cmds, cmd)
		case viewActiveConnectionInfo: m.activeConnInfoViewport, cmd = m.activeConnInfoViewport.Update(msg); cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

func (m model) headerView(width int) string {
	wifiStatusStr := "Wi-Fi: "; if m.wifiEnabled { wifiStatusStr += wifiStatusStyleEnabled.Render("Enabled ✔") } else { wifiStatusStr += wifiStatusStyleDisabled.Render("Disabled ✘") }
	titleStr := titleStyle.Render("Go Network Manager TUI")
	titleActualWidth, wifiStatusActualWidth := lipgloss.Width(titleStr), lipgloss.Width(wifiStatusStr)
	spacing := width - titleActualWidth - wifiStatusActualWidth; if spacing < 1 { spacing = 1 }
	return lipgloss.JoinHorizontal(lipgloss.Left, titleStr, strings.Repeat(" ", spacing), wifiStatusStr)
}

func (m model) footerView(availableWidth int, helpViewContent string) string {
	return lipgloss.PlaceHorizontal(availableWidth, lipgloss.Center, helpGlobalStyle.Render(helpViewContent))
}

func (m model) View() string {
	availableWidth := m.width - appStyle.GetHorizontalFrameSize()
	availableHeight := m.height - appStyle.GetVerticalFrameSize()
	var mainContent string

	headerViewContent := m.headerView(availableWidth)
	renderedHelpContent := m.help.View(m.keys)
	footerViewContent := m.footerView(availableWidth, renderedHelpContent)
	headerHeight := lipgloss.Height(headerViewContent)
	footerHeight := lipgloss.Height(footerViewContent)
	contentDisplayHeight := availableHeight - headerHeight - footerHeight
	if contentDisplayHeight < 0 { contentDisplayHeight = 0 }

	switch m.state {
	case viewNetworksList:
		if m.isLoading && m.wifiList.FilterState() != list.Filtering {
			loadingLine := lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View()+" ", "Scanning for Wi-Fi networks...")
			mainContent = lipgloss.Place(availableWidth, contentDisplayHeight, lipgloss.Center, lipgloss.Center, m.spinner.Style.Render(loadingLine))
		} else {
			listRendered := m.wifiList.View()
			mainContent = lipgloss.PlaceHorizontal(availableWidth, lipgloss.Center, listRendered)
		}
		if m.connectionStatusMsg != "" && m.state == viewNetworksList { // Ensure status is for this view
			statusMsgRendered := m.connectionStatusMsg
			if !strings.HasPrefix(m.connectionStatusMsg, "\x1b[") {
				currentStatusStyle := statusMessageBaseStyle.Copy()
				if strings.Contains(strings.ToLower(m.connectionStatusMsg), "unnamed networks") {
					currentStatusStyle = toggleHiddenStatusMsgStyle
				} else if strings.Contains(m.connectionStatusMsg, "Wi-Fi is") {
					currentStatusStyle = statusMessageBaseStyle.Copy().Foreground(ansTextColor)
				} else {
					currentStatusStyle = statusMessageBaseStyle.Copy().Faint(true)
				}
				statusMsgRendered = currentStatusStyle.Render(m.connectionStatusMsg)
			}
			// Only append if not currently loading (loading has its own spinner message)
			if !m.isLoading || m.wifiList.FilterState() == list.Filtering {
				mainContent = lipgloss.JoinVertical(lipgloss.Top, mainContent, statusMsgRendered)
			}
		}


	case viewPasswordInput:
		pwPromptText := fmt.Sprintf("Password for %s:", m.selectedAP.StyledTitle())
		promptContainerWidth := m.passwordInput.Width + lipgloss.Width(m.passwordInput.Prompt) + 2
		centeredPrompt := lipgloss.NewStyle().Width(promptContainerWidth).Align(lipgloss.Center).Render(pwPromptText)
		inputRendered := m.passwordInput.View()
		pwBlockContent := lipgloss.JoinVertical(lipgloss.Top, centeredPrompt, inputRendered)
		if m.passwordInput.Err != nil {
			pwBlockContent = lipgloss.JoinVertical(lipgloss.Top, pwBlockContent, errorStyle.Render(m.passwordInput.Err.Error()))
		}
		mainContent = passwordInputContainerStyle.Render(pwBlockContent)

	case viewConnecting:
		connectingMsgContent := fmt.Sprintf("\n%s Connecting to %s...\n", m.spinner.View(), m.selectedAP.StyledTitle())
		mainContent = connectingStyle.Render(connectingMsgContent)

	case viewConnectionResult:
    // resultMsgStyled is the potentially long error string (already styled with colors)
    resultMsgStyled := m.connectionStatusMsg 

    // Determine the width available for the message.
    // This should be based on the availableWidth for mainContent, minus some padding if desired.
    // For simplicity, let's use a percentage of availableWidth or a fixed cap.
    // The mainContent itself is already placed within availableWidth.
    // The width of the text block for the message should be less than or equal to
    // the width of the `lipgloss.Place` call that centers `mainContent`.
    // Let's assume contentDisplayHeight is for vertical placement,
    // and availableWidth is for horizontal.

    // Max width for the error message block, can be adjusted
    // It should be less than availableWidth to allow for centering of the block itself.
    messageBlockWidth := availableWidth * 3 / 4 // Example: 75% of available width
    if messageBlockWidth > 80 { // Cap at 80 characters for readability
        messageBlockWidth = 80
    }
    if messageBlockWidth < 40 { // Minimum sensible width
        messageBlockWidth = 40
    }
    
    // Create a style for the message that includes width for wrapping.
    // This style will wrap the pre-styled error/success string.
    messageStyleWithWrap := lipgloss.NewStyle().
        Width(messageBlockWidth). // This enables wrapping
        Align(lipgloss.Center)    // Center the text within this block

    // Render the (already color-styled) message with the wrapping style
    wrappedMessage := messageStyleWithWrap.Render(resultMsgStyled)

    helpHint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Press Enter or Esc to return to list)")
    
    // JoinVertical will stack them. The wrappedMessage will obey its own width.
    // The lipgloss.Place for mainContent will then center this entire vertical block.
    resultText := lipgloss.JoinVertical(lipgloss.Center, // Center aligns items vertically in the join
        wrappedMessage,
        "", // Spacer
        helpHint,
    )
    mainContent = resultText // This will then be centered by the outer Place

	case viewActiveConnectionInfo:
		mainContent = m.activeConnInfoViewport.View()
	case viewConfirmDisconnect:
		confirmMsgContent := fmt.Sprintf("Disconnect from %s?", m.selectedAP.StyledTitle())
		mainContent = lipgloss.JoinVertical(lipgloss.Center, confirmMsgContent, lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter to confirm, Esc to cancel)"))
	}

	if m.state != viewNetworksList && m.state != viewActiveConnectionInfo {
		mainContent = lipgloss.Place(availableWidth, contentDisplayHeight, lipgloss.Center, lipgloss.Center, mainContent)
	}

	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top, headerViewContent, mainContent, footerViewContent))
}

func main() {
	var logFile *os.File
	if os.Getenv("DEBUG_TEA") != "" {
		var err error; logFile, err = tea.LogToFile(debugLogFile, "nmtui:")
		if err != nil { fmt.Fprintf(os.Stderr, "Error creating log file %s: %v\n", debugLogFile, err)
		} else { log.SetFlags(log.Ltime | log.Lshortfile); log.Println("--- NMTUI Debug Log Started ---") }
	}
	if logFile != nil {
		defer func() { log.Println("--- NMTUI Debug Log Ended ---"); if err := logFile.Close(); err != nil { fmt.Fprintf(os.Stderr, "Error closing log file: %v\n", err) } }()
	}

	initialM := initialModel()
	p := tea.NewProgram(initialM, tea.WithAltScreen(), tea.WithMouseCellMotion())

	finalModel, err := p.Run()
	if err != nil {
		errorMsg := fmt.Sprintf("Error running TUI: %v\n", err)
		if logFile != nil { log.Print(errorMsg) } else { fmt.Fprint(os.Stderr, errorMsg) }
		if fm, ok := finalModel.(model); ok {
			stateMsg := fmt.Sprintf("Final model state on error: %+v\n", fm)
			if logFile != nil { log.Print(stateMsg) } else { fmt.Fprint(os.Stderr, stateMsg) }
		}
		os.Exit(1)
	}
}
