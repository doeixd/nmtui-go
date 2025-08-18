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
	debugLogFile            = "nmtui-debug.log"
	helpBarMaxWidth         = 80
	helpBarWidthPercent     = 0.80
	networkListFixedWidth   = 100
	networkListWidthPercent = 0.85
)

// --- Styles Definition ---
var (
	appStyle = lipgloss.NewStyle().Margin(1, 1)

	ansPrimaryColor   = lipgloss.Color("5")
	ansSecondaryColor = lipgloss.Color("4")
	ansAccentColor    = lipgloss.Color("6")
	ansSuccessColor   = lipgloss.Color("2")
	ansErrorColor     = lipgloss.Color("1")
	ansFaintTextColor = lipgloss.Color("8")
	ansTextColor      = lipgloss.Color("7")

	adaptiveTitleForegroundColor = lipgloss.AdaptiveColor{Light: "235", Dark: "250"}
	titleStyle                   = lipgloss.NewStyle().Bold(true).Foreground(ansPrimaryColor).Padding(0, 1).MarginBottom(1)
	listTitleStyle               = lipgloss.NewStyle().Foreground(ansSecondaryColor).Padding(0, 1).Bold(true)
	listItemStyle                = lipgloss.NewStyle().PaddingLeft(2).Foreground(ansTextColor)
	listSelectedItemStyle        = lipgloss.NewStyle().PaddingLeft(1).Foreground(ansPrimaryColor).Bold(true)
	listDescStyle                = lipgloss.NewStyle().PaddingLeft(2).Foreground(ansFaintTextColor)
	listSelectedDescStyle        = lipgloss.NewStyle().PaddingLeft(1).Foreground(ansPrimaryColor)
	listNoItemsStyle             = lipgloss.NewStyle().Faint(true).Margin(1, 0).Align(lipgloss.Center).Foreground(ansFaintTextColor)

	statusMessageBaseStyle     = lipgloss.NewStyle().MarginTop(1)
	errorStyle                 = statusMessageBaseStyle.Copy().Foreground(ansErrorColor).Bold(true)
	connectingStyle            = lipgloss.NewStyle().Foreground(ansAccentColor)
	successStyle               = statusMessageBaseStyle.Copy().Foreground(ansSuccessColor).Bold(true)
	infoBoxStyle               = lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true).BorderForeground(ansAccentColor).Padding(1, 2).MarginTop(1)
	toggleHiddenStatusMsgStyle = statusMessageBaseStyle.Copy().Foreground(ansFaintTextColor)

	passwordPromptStyle         = lipgloss.NewStyle().Foreground(ansFaintTextColor)
	passwordInputContainerStyle = lipgloss.NewStyle().Padding(1).MarginTop(1).Border(lipgloss.NormalBorder(), true).BorderForeground(ansFaintTextColor)

	helpGlobalStyle = lipgloss.NewStyle().Foreground(ansFaintTextColor)

	wifiStatusStyleEnabled     = lipgloss.NewStyle().Foreground(ansSuccessColor)
	wifiStatusStyleDisabled    = lipgloss.NewStyle().Foreground(ansErrorColor)
	listTitleHiddenStatusStyle = lipgloss.NewStyle().Foreground(ansFaintTextColor).Italic(true)
)

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
)

type itemDelegate struct{}

func (d itemDelegate) Height() int                               { return 2 }
func (d itemDelegate) Spacing() int                              { return 1 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(wifiAP)
	if !ok { return }
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

type wifiAP struct {
	gonetworkmanager.WifiAccessPoint
	IsKnown   bool
	IsActive  bool
	Interface string
}

func (ap wifiAP) getSSIDFromScannedAP() string {
	if ap.WifiAccessPoint == nil { return "" }
	return ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSSID]
}
func (ap wifiAP) StyledTitle() string {
	ssid := ap.getSSIDFromScannedAP()
	if ssid == "" || ssid == "--" { ssid = "<Hidden Network>" }
	indicator := ""
	if ap.IsActive { indicator += lipgloss.NewStyle().Foreground(ansSuccessColor).Render(" ✔") }
	if ap.IsKnown && !ap.IsActive { indicator += lipgloss.NewStyle().Foreground(ansAccentColor).Render(" ★") }
	return fmt.Sprintf("%s%s", ssid, indicator)
}
func (ap wifiAP) Title() string       { return ap.StyledTitle() }
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
		var sStyle lipgloss.Style
		switch {
		case signalVal > 70: sStyle = lipgloss.NewStyle().Foreground(ansSuccessColor)
		case signalVal > 40: sStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		default: sStyle = lipgloss.NewStyle().Foreground(ansErrorColor)
		}
		descParts = append(descParts, fmt.Sprintf("%s %s", labelStyle.Render("Signal:"), sStyle.Render(signalStr+"%%")))
	}
	if security == "" || security == "--" { security = "Open" }
	descParts = append(descParts, fmt.Sprintf("%s %s", labelStyle.Render("Security:"), labelStyle.Render(security)))
	return strings.Join(descParts, labelStyle.Render(" | "))
}
func (ap wifiAP) FilterValue() string {
	ssid := ap.getSSIDFromScannedAP()
	if ssid == "" || ssid == "--" { return "<Hidden Network>" }
	return ssid
}

type wifiListLoadedMsg struct{ allAps []wifiAP; err error }
type connectionAttemptMsg struct{ ssid string; success bool; err error; WasKnownAttemptNoPsk bool }
type wifiStatusMsg struct{ enabled bool; err error }
type knownNetworksMsg struct{ knownProfiles map[string]gonetworkmanager.ConnectionProfile; activeWifiConnection *gonetworkmanager.ConnectionProfile; activeWifiDevice string }
type activeConnInfoMsg struct{ details *gonetworkmanager.DeviceIPDetail; err error }
type disconnectResultMsg struct{ success bool; err error; ssid string }
type forgetNetworkResultMsg struct{ ssid string; success bool; err error }
type knownWifiApsListMsg struct{ aps []wifiAP; err error }

type keyMap struct {
	Connect, Refresh, Quit, Back, Help, Filter, ToggleWifi, Disconnect, Info, ToggleHidden, Forget, Profiles key.Binding
	currentState viewState
}

func (k keyMap) ShortHelp() []key.Binding {
	b := []key.Binding{k.Help}
	switch k.currentState {
	case viewNetworksList:
		b = append(b, k.Connect, k.Refresh, k.Filter, k.ToggleWifi, k.Profiles)
	case viewKnownNetworksList:
		b = append(b, k.Back, k.Forget)
	case viewPasswordInput, viewConnectionResult, viewConfirmDisconnect, viewConfirmForget:
		b = append(b, k.Connect, k.Back)
	case viewActiveConnectionInfo:
		b = append(b, k.Back)
	}
	return append(b, k.Quit)
}
func (k keyMap) FullHelp() [][]key.Binding {
	switch k.currentState {
	case viewKnownNetworksList:
		return [][]key.Binding{{k.Back, k.Forget, k.Quit}}
	default: // viewNetworksList
		return [][]key.Binding{
			{k.Help, k.Connect, k.Back, k.Quit},
			{k.Refresh, k.Filter, k.ToggleHidden, k.ToggleWifi},
			{k.Disconnect, k.Forget, k.Info, k.Profiles},
		}
	}
}
var defaultKeyBindings = keyMap{
	Connect:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select/conn/confirm")),
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
}

type model struct {
	state                  viewState
	previousState          viewState
	wifiList               list.Model
	knownWifiList          list.Model
	passwordInput          textinput.Model
	spinner                spinner.Model
	activeConnInfoViewport viewport.Model
	selectedAP             wifiAP
	connectionStatusMsg    string
	lastConnectionWasSuccessful bool
	wifiEnabled            bool
	knownProfiles          map[string]gonetworkmanager.ConnectionProfile
	activeWifiConnection   *gonetworkmanager.ConnectionProfile
	activeWifiDevice       string
	allScannedAps          []wifiAP
	showHiddenNetworks     bool
	isLoading              bool
	width, height          int
	listDisplayWidth       int
	keys                   keyMap
	help                   help.Model
}

func initialModel() model {
	delegate := itemDelegate{}; l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Scanning for Wi-Fi Networks..."; l.Styles.Title = listTitleStyle
	l.SetShowStatusBar(true); l.SetStatusBarItemName("network", "networks"); l.SetShowHelp(false); l.DisableQuitKeybindings()
	l.Styles.NoItems = listNoItemsStyle.Copy().SetString("No Wi-Fi. Try (r)efresh, (t)oggle Wi-Fi, (u)nnamed.")
	l.Styles.FilterPrompt = lipgloss.NewStyle().Foreground(ansPrimaryColor)
	l.Styles.FilterCursor = lipgloss.NewStyle().Foreground(ansPrimaryColor)
	l.AdditionalShortHelpKeys = func() []key.Binding { return []key.Binding{defaultKeyBindings.Filter, defaultKeyBindings.Refresh, defaultKeyBindings.ToggleHidden} }
	l.AdditionalFullHelpKeys = l.AdditionalShortHelpKeys
	ti := textinput.New(); ti.Placeholder = "Network Password"; ti.EchoMode = textinput.EchoPassword; ti.CharLimit = 63
	ti.Prompt = passwordPromptStyle.Render("🔑 Password: "); ti.EchoCharacter = '•'; ti.Cursor.Style = lipgloss.NewStyle().Foreground(ansAccentColor)
	s := spinner.New(); s.Spinner = spinner.Globe; s.Style = connectingStyle
	vp := viewport.New(0, 0); vp.Style = infoBoxStyle.Copy()
	h := help.New(); h.ShowAll = false
	subtleHelp := lipgloss.NewStyle().Foreground(ansFaintTextColor)
	h.Styles = help.Styles{ShortKey: subtleHelp, ShortDesc: subtleHelp, FullKey: subtleHelp, FullDesc: subtleHelp, Ellipsis: subtleHelp.Copy()}
	pl := list.New([]list.Item{}, delegate, 0, 0)
	pl.Title = "Known Wi-Fi Profiles"
	pl.Styles.Title = listTitleStyle
	pl.SetShowStatusBar(false)
	pl.SetShowHelp(false)
	pl.DisableQuitKeybindings()
	pl.Styles.NoItems = listNoItemsStyle.Copy().SetString("No known Wi-Fi profiles found.")

	m := model{
		state:         viewNetworksList,
		wifiList:      l,
		knownWifiList: pl,
		passwordInput: ti,
		spinner:       s,
		activeConnInfoViewport: vp,
		isLoading:              true,
		keys:                   defaultKeyBindings,
		help:                   h,
		knownProfiles:          make(map[string]gonetworkmanager.ConnectionProfile),
		showHiddenNetworks:     false,
	}
	m.keys.currentState = m.state
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(getWifiStatusInternalCmd(), fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
}

func fetchWifiNetworksCmd(rescan bool) tea.Cmd { /* Same */ 
	return func() tea.Msg {
		log.Printf("Cmd: Fetching Wi-Fi networks (rescan: %t)...", rescan)
		apsRaw, err := gonetworkmanager.GetWifiList(rescan)
		var aps []wifiAP
		if err == nil { aps = make([]wifiAP, len(apsRaw)); for i, r := range apsRaw { aps[i] = wifiAP{WifiAccessPoint: r} }; log.Printf("Cmd: Fetched %d Wi-Fi networks.", len(apsRaw))
		} else { log.Printf("Cmd: Error fetching Wi-Fi list: %v", err) }
		return wifiListLoadedMsg{allAps: aps, err: err}
	}
}
func connectToWifiCmd(ssid, pw string, knownNoPsk bool) tea.Cmd { /* Same */ 
	return func() tea.Msg {
		log.Printf("Cmd: Connect to SSID: '%s', WasKnownNoPsk: %t", ssid, knownNoPsk)
		_, err := gonetworkmanager.ConnectToWifiRobustly(ssid, "*", ssid, pw, false) 
		if err != nil { log.Printf("Cmd: Connect error for '%s': %v", ssid, err) } else { log.Printf("Cmd: Connect for '%s' appears successful.", ssid) }
		return connectionAttemptMsg{ssid: ssid, success: err == nil, err: err, WasKnownAttemptNoPsk: knownNoPsk}
	}
}
func getWifiStatusInternalCmd() tea.Cmd { /* Same */ 
	return func() tea.Msg {
		log.Printf("Cmd: Getting Wi-Fi status...")
		st, err := gonetworkmanager.GetWifiStatus(); enabled := false
		if err == nil && st == "enabled" { enabled = true }
		if err != nil { log.Printf("Cmd: Error getting Wi-Fi status: %v", err) }
		return wifiStatusMsg{enabled: enabled, err: err}
	}
}
func toggleWifiCmd(enable bool) tea.Cmd { /* Same */ 
	return func() tea.Msg {
		log.Printf("Cmd: Toggling Wi-Fi to %t...", enable)
		var err error
		if enable { _, err = gonetworkmanager.WifiEnable() } else { _, err = gonetworkmanager.WifiDisable() }
		if err != nil { log.Printf("Cmd: Error toggling Wi-Fi: %v", err); return wifiStatusMsg{enabled: !enable, err: err} }
		return wifiStatusMsg{enabled: enable, err: nil}
	}
}
func fetchKnownNetworksCmd() tea.Cmd { /* Same */ 
	return func() tea.Msg {
		log.Printf("Cmd: Fetching known networks...")
		profiles, err := gonetworkmanager.GetConnectionProfilesList(false) 
		if err != nil { log.Printf("Cmd: Error fetching known profiles: %v", err); return knownNetworksMsg{} }
		known := make(map[string]gonetworkmanager.ConnectionProfile); var activeConn *gonetworkmanager.ConnectionProfile; var activeDev string
		activeDevProfiles, _ := gonetworkmanager.GetConnectionProfilesList(true) 
		activeUUIDs := make(map[string]struct{})
		for _, adp := range activeDevProfiles { if adp[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi { activeUUIDs[adp[gonetworkmanager.NmcliFieldConnectionUUID]] = struct{}{} } }
		for _, p := range profiles {
			if p[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi {
				ssid := gonetworkmanager.GetSSIDFromProfile(p)
				if ssid != "" { known[ssid] = p 
					if _, isActive := activeUUIDs[p[gonetworkmanager.NmcliFieldConnectionUUID]]; isActive {
						pCopy := make(gonetworkmanager.ConnectionProfile); for k,v := range p { pCopy[k]=v }; activeConn = &pCopy; activeDev = p[gonetworkmanager.NmcliFieldConnectionDevice]
					}
				}
			}
		}
		log.Printf("Cmd: Found %d known Wi-Fi profiles. Active: %v", len(known), activeConn != nil)
		return knownNetworksMsg{knownProfiles: known, activeWifiConnection: activeConn, activeWifiDevice: activeDev}
	}
}
func fetchActiveConnInfoCmd(devName string) tea.Cmd { /* Same */ 
	return func() tea.Msg {
		if devName == "" { log.Printf("Cmd: fetchActiveConnInfo called with no device."); return activeConnInfoMsg{nil, fmt.Errorf("no active Wi-Fi device")} }
		log.Printf("Cmd: Fetching IP details for device: %s", devName)
		details, err := gonetworkmanager.GetDeviceInfoIPDetail(devName)
		if err != nil { log.Printf("Cmd: Error fetching IP details for %s: %v", devName, err) }
		return activeConnInfoMsg{details: details, err: err}
	}
}
func disconnectWifiCmd(profileID string) tea.Cmd { /* Same */ 
	return func() tea.Msg {
		log.Printf("Cmd: Attempting to disconnect profile: %s", profileID)
		_, err := gonetworkmanager.ConnectionDown(profileID)
		if err != nil { log.Printf("Cmd: Error disconnecting %s: %v", profileID, err) }
		return disconnectResultMsg{success: err == nil, err: err, ssid: profileID}
	}
}
func forgetNetworkCmd(profileID, ssidForMsg string) tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Attempting to forget profile ID: '%s' (SSID: '%s')", profileID, ssidForMsg)
		_, err := gonetworkmanager.ConnectionDelete(profileID)
		if err != nil { log.Printf("Cmd: Error forgetting profile '%s': %v", profileID, err) }
		return forgetNetworkResultMsg{ssid: ssidForMsg, success: err == nil, err: err}
	}
}

func fetchKnownWifiApsCmd() tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Fetching all known Wi-Fi profiles...")
		profiles, err := gonetworkmanager.GetConnectionProfilesList(false)
		if err != nil {
			log.Printf("Cmd: Error fetching known profiles: %v", err)
			return knownWifiApsListMsg{err: err}
		}

		var wifiAps []wifiAP
		for _, p := range profiles {
			if p[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi {
				ap := connectionProfileToWifiAP(p, nil) // No active connection check needed here
				wifiAps = append(wifiAps, ap)
			}
		}
		log.Printf("Cmd: Found %d known Wi-Fi profiles.", len(wifiAps))
		return knownWifiApsListMsg{aps: wifiAps, err: nil}
	}
}

// connectionProfileToWifiAP converts a known profile into a list item.
func connectionProfileToWifiAP(p gonetworkmanager.ConnectionProfile, activeConn *gonetworkmanager.ConnectionProfile) wifiAP {
	ssid := gonetworkmanager.GetSSIDFromProfile(p)
	if ssid == "" {
		ssid = p[gonetworkmanager.NmcliFieldConnectionName]
	}
	// Create a WifiAccessPoint map from the ConnectionProfile for wifiAP
	apMap := make(gonetworkmanager.WifiAccessPoint)
	for k, v := range p {
		apMap[k] = v
	}
	apMap[gonetworkmanager.NmcliFieldWifiSSID] = ssid // Ensure SSID is set for display

	isActive := false
	if activeConn != nil && p[gonetworkmanager.NmcliFieldConnectionUUID] == (*activeConn)[gonetworkmanager.NmcliFieldConnectionUUID] {
		isActive = true
	}

	return wifiAP{
		WifiAccessPoint: apMap,
		IsKnown:         true, // By definition
		IsActive:        isActive,
		Interface:       p[gonetworkmanager.NmcliFieldConnectionDevice],
	}
}

func (m *model) processAndSetWifiList(apsToProcess []wifiAP) { /* Same, relies on m.knownProfiles & m.activeWifiConnection being correct */
	var filteredAps []wifiAP 
	for _, ap := range apsToProcess { ssid := ap.getSSIDFromScannedAP(); isUnnamed := ssid == "" || ssid == "--"; if m.showHiddenNetworks || !isUnnamed { filteredAps = append(filteredAps, ap) } }
	enrichedAps := make([]list.Item, len(filteredAps)); foundActive := false 
	for i, ap := range filteredAps {
		pAP := ap; ssid := pAP.getSSIDFromScannedAP(); pAP.IsKnown, pAP.IsActive = false, false 
		if ssid != "" && ssid != "--" { 
			if profile, ok := m.knownProfiles[ssid]; ok { pAP.IsKnown = true
				if m.activeWifiConnection != nil && profile[gonetworkmanager.NmcliFieldConnectionUUID] == (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID] {
					pAP.IsActive = true; pAP.Interface = profile[gonetworkmanager.NmcliFieldConnectionDevice]; foundActive = true
				}
			}
		}
		enrichedAps[i] = pAP
	}
	if !foundActive && m.activeWifiConnection != nil { log.Println("ProcessList: Active conn (", gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection), ") not in scan.") }
	sort.SliceStable(enrichedAps, func(i, j int) bool {
		itemI, itemJ := enrichedAps[i].(wifiAP), enrichedAps[j].(wifiAP)
		if itemI.IsActive != itemJ.IsActive { return itemI.IsActive }
		if itemI.IsKnown != itemJ.IsKnown { return itemI.IsKnown }
		sigi, _ := strconv.Atoi(itemI.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal]); sigj, _ := strconv.Atoi(itemJ.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
		if sigi != sigj { return sigi > sigj }
		ssidi, ssidj := strings.ToLower(itemI.getSSIDFromScannedAP()), strings.ToLower(itemJ.getSSIDFromScannedAP())
		isIUn := ssidi == "" || ssidi == "--"; isJUn := ssidj == "" || ssidj == "--"
		if isIUn && !isJUn { return false }; if !isIUn && isJUn { return true }
		return ssidi < ssidj
	})
	m.wifiList.SetItems(enrichedAps)
	hiddenStatus := ""; if !m.showHiddenNetworks { hiddenStatus = listTitleHiddenStatusStyle.Render(" (hiding unnamed)") }
	m.wifiList.Title = fmt.Sprintf("Available Wi-Fi Networks (%d found)%s", len(enrichedAps), hiddenStatus)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd; var cmd tea.Cmd
	m.keys.currentState = m.state

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		appStyleHorizontalFrame := appStyle.GetHorizontalFrameSize(); appStyleVerticalFrame := appStyle.GetVerticalFrameSize()
		availableWidth := m.width - appStyleHorizontalFrame; availableHeight := m.height - appStyleVerticalFrame
		desiredHelpWidth := int(float64(availableWidth) * helpBarWidthPercent); if desiredHelpWidth > helpBarMaxWidth { desiredHelpWidth = helpBarMaxWidth }; if desiredHelpWidth < 20 { desiredHelpWidth = 20 }
		m.help.Width = desiredHelpWidth
		headerHeight := lipgloss.Height(m.headerView(availableWidth))
		tempKeyMapState := m.keys; tempKeyMapState.currentState = m.state // Use current state for accurate footer height
		footerHeight := lipgloss.Height(m.footerView(availableWidth, m.help.View(tempKeyMapState)))
		contentAreaHeight := availableHeight - headerHeight - footerHeight
		if contentAreaHeight < 0 { contentAreaHeight = 0 }
		listWidth := availableWidth
		if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
			calcW := availableWidth; if networkListWidthPercent > 0 { calcW = int(float64(availableWidth) * networkListWidthPercent) }
			if networkListFixedWidth > 0 && calcW > networkListFixedWidth { calcW = networkListFixedWidth }; if calcW < 40 { calcW = 40 }; listWidth = calcW
		}
		m.listDisplayWidth = listWidth // Store calculated list width
		m.wifiList.SetSize(m.listDisplayWidth, contentAreaHeight)
		m.knownWifiList.SetSize(m.listDisplayWidth, contentAreaHeight)
		m.activeConnInfoViewport.Width = availableWidth - infoBoxStyle.GetHorizontalFrameSize()
		m.activeConnInfoViewport.Height = contentAreaHeight - infoBoxStyle.GetVerticalFrameSize(); if m.activeConnInfoViewport.Height < 0 {m.activeConnInfoViewport.Height = 0}
		pwInputContentWidth := availableWidth*2/3; if pwInputContentWidth > 60 { pwInputContentWidth = 60 }; if pwInputContentWidth < 40 { pwInputContentWidth = 40 }
		m.passwordInput.Width = pwInputContentWidth - lipgloss.Width(m.passwordInput.Prompt) - passwordInputContainerStyle.GetHorizontalFrameSize()

	case spinner.TickMsg: if m.isLoading { m.spinner, cmd = m.spinner.Update(msg); cmds = append(cmds, cmd) }
	case wifiStatusMsg:
		m.isLoading = false
		if msg.err != nil { if m.state == viewNetworksList { m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error Wi-Fi status: %v", msg.err)) }
		} else {
			m.wifiEnabled = msg.enabled; statusText := "disabled"; if m.wifiEnabled { statusText = "enabled" }
			if m.state == viewNetworksList { m.connectionStatusMsg = fmt.Sprintf("Wi-Fi is %s.", statusText) }
			if m.wifiEnabled { m.isLoading = true; m.wifiList.Title = "Scanning..."; cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
			} else { m.allScannedAps = nil; m.processAndSetWifiList([]wifiAP{}); m.wifiList.Title = "Wi-Fi is Disabled"; m.activeWifiConnection = nil; m.activeWifiDevice = ""
				if m.state == viewNetworksList { m.connectionStatusMsg = "Wi-Fi is disabled." }
			}
		}
	case knownNetworksMsg: m.knownProfiles, m.activeWifiConnection, m.activeWifiDevice = msg.knownProfiles, msg.activeWifiConnection, msg.activeWifiDevice; if m.allScannedAps != nil { m.processAndSetWifiList(m.allScannedAps) }
	case wifiListLoadedMsg:
		m.isLoading = false
		if msg.err != nil { if m.state == viewNetworksList { m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error fetching Wi-Fi: %v", msg.err)) }; m.wifiList.Title = "Error Loading Networks"
		} else { m.allScannedAps = msg.allAps; m.processAndSetWifiList(m.allScannedAps) }
	case connectionAttemptMsg:
		m.isLoading = false
		if msg.success { m.state = viewConnectionResult; m.lastConnectionWasSuccessful = true; m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Connected to %s!", m.selectedAP.StyledTitle()))
		} else {
			if msg.WasKnownAttemptNoPsk && m.selectedAP.getSSIDFromScannedAP() == msg.ssid {
				log.Printf("Known net '%s' connect failed. Prompting for PSK.", msg.ssid); m.state = viewPasswordInput; m.passwordInput.SetValue(""); m.passwordInput.Focus()
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Stored creds for %s failed. Enter password:", m.selectedAP.StyledTitle()))
				cmds = append(cmds, textinput.Blink); return m, tea.Batch(cmds...)
			} else { m.state = viewConnectionResult; m.lastConnectionWasSuccessful = false; errTxt := "Unknown error."; if msg.err != nil { errTxt = msg.err.Error() }; m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Failed to connect to %s: %s", m.selectedAP.StyledTitle(), errTxt)) }
		}
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(false)) // Refresh state after attempt
	case activeConnInfoMsg: /* Same */ 
		m.isLoading = false
		if msg.err != nil { m.activeConnInfoViewport.SetContent(errorStyle.Render(fmt.Sprintf("Error active info: %v", msg.err)))
		} else if msg.details == nil { m.activeConnInfoViewport.SetContent(toggleHiddenStatusMsgStyle.Render("No IP details for active connection."))
		} else {
			info := []string{ fmt.Sprintf("Device: %s (%s)", msg.details.Device, msg.details.Type), fmt.Sprintf("State: %s", msg.details.State), fmt.Sprintf("Connection: %s", msg.details.Connection), fmt.Sprintf("MAC: %s", msg.details.Mac), fmt.Sprintf("IPv4: %s (%s)", msg.details.IPv4, msg.details.NetV4), fmt.Sprintf("Gateway v4: %s", msg.details.GatewayV4), fmt.Sprintf("DNS: %s", strings.Join(msg.details.DNS, ", "))}
			if msg.details.IPv6 != "" { info = append(info, fmt.Sprintf("IPv6: %s (%s)", msg.details.IPv6, msg.details.NetV6), fmt.Sprintf("Gateway v6: %s", msg.details.GatewayV6)) }
			m.activeConnInfoViewport.SetContent(strings.Join(info, "\n"))
		}
	case disconnectResultMsg: /* Same */ 
		m.isLoading = false
		if msg.success { m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Disconnected from %s.", msg.ssid)); m.activeWifiConnection = nil; m.activeWifiDevice = ""
		} else { m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error disconnecting from %s: %v", msg.ssid, msg.err)) }
		m.state = viewNetworksList; cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
	case forgetNetworkResultMsg:
		m.isLoading = false
		if msg.success {
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Network profile for %s forgotten.", msg.ssid))
			delete(m.knownProfiles, msg.ssid)
		} else {
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error forgetting profile for %s: %v", msg.ssid, msg.err))
		}

		// Return to the previous state instead of always going to the main list
		m.state = m.previousState

		// If we came from the profiles list, refresh it. Otherwise, refresh the main list.
		if m.state == viewKnownNetworksList {
			cmds = append(cmds, fetchKnownWifiApsCmd())
		} else {
			cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
		}

	case knownWifiApsListMsg:
		m.isLoading = false
		if msg.err != nil {
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error fetching known profiles: %v", msg.err))
			m.knownWifiList.Title = "Error Loading Profiles"
		} else {
			items := make([]list.Item, len(msg.aps))
			for i, ap := range msg.aps {
				items[i] = ap
			}
			m.knownWifiList.SetItems(items)
			m.knownWifiList.Title = fmt.Sprintf("Known Wi-Fi Profiles (%d found)", len(items))
		}

	case tea.KeyMsg:
		if key.Matches(msg, m.keys.Quit) { return m, tea.Quit }
		if key.Matches(msg, m.keys.Help) {
			if m.state != viewPasswordInput {
				m.help.ShowAll = !m.help.ShowAll
				if m.state == viewNetworksList || m.state == viewActiveConnectionInfo {
					avW := m.width - appStyle.GetHorizontalFrameSize(); hH := lipgloss.Height(m.headerView(avW))
					tk := m.keys; tk.currentState = m.state; fH := lipgloss.Height(m.footerView(avW, m.help.View(tk)))
					appCH := m.height - appStyle.GetVerticalFrameSize(); nCAH := appCH - hH - fH; if nCAH < 0 { nCAH = 0 }
					if m.state == viewNetworksList { m.wifiList.SetSize(m.listDisplayWidth, nCAH)
					} else if m.state == viewActiveConnectionInfo { m.activeConnInfoViewport.Height = nCAH - infoBoxStyle.GetVerticalFrameSize(); if m.activeConnInfoViewport.Height < 0 {m.activeConnInfoViewport.Height = 0} }
					log.Printf("Help toggled, content height for %v set to: %d", m.state, nCAH)
				}
			}
		}

		switch m.state {
		case viewNetworksList:
			// If the list is filtering, it receives all key events.
			if m.wifiList.FilterState() == list.Filtering {
				m.wifiList, cmd = m.wifiList.Update(msg)
				cmds = append(cmds, cmd)

				// If the user just exited the filter, clear the "Filtering..." status message.
				if m.wifiList.FilterState() != list.Filtering {
					m.connectionStatusMsg = ""
				}
				// Return immediately to prevent the keypress from being processed again.
				// This is the fix for the Enter key bug.
				return m, tea.Batch(cmds...)
			}

			// If we're not filtering, handle keypresses as normal.
			if m.isLoading {
				break
			}

			switch {
			case key.Matches(msg, m.keys.ToggleHidden):
				m.showHiddenNetworks = !m.showHiddenNetworks
				m.processAndSetWifiList(m.allScannedAps)
				if m.showHiddenNetworks {m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Showing unnamed.")} else {m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Hiding unnamed.")}
			case key.Matches(msg, m.keys.Filter):
				m.wifiList.FilterInput.Focus()
				m.connectionStatusMsg = "Filtering..."
				cmds = append(cmds, textinput.Blink)
			case key.Matches(msg, m.keys.Refresh):
				m.isLoading = true
				m.connectionStatusMsg = ""
				m.allScannedAps = nil
				m.processAndSetWifiList([]wifiAP{})
				m.wifiList.Title = "Refreshing..."
				cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
			case key.Matches(msg, m.keys.ToggleWifi):
				m.isLoading = true
				act := "OFF"
				if !m.wifiEnabled {act="ON"}
				m.connectionStatusMsg = fmt.Sprintf("Toggling Wi-Fi %s...", act)
				cmds = append(cmds, toggleWifiCmd(!m.wifiEnabled), m.spinner.Tick)
			case key.Matches(msg, m.keys.Disconnect):
				if m.activeWifiConnection != nil {
					sAP := gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection)
					td := make(gonetworkmanager.WifiAccessPoint)
					td[gonetworkmanager.NmcliFieldWifiSSID] = sAP
					m.selectedAP = wifiAP{WifiAccessPoint: td, IsActive: true, IsKnown: true, Interface: m.activeWifiDevice}
					m.state = viewConfirmDisconnect
					m.connectionStatusMsg = ""
				} else {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Not connected.")
				}
			case key.Matches(msg, m.keys.Forget):
				if item, ok := m.wifiList.SelectedItem().(wifiAP); ok && item.IsKnown {
					m.selectedAP = item
					m.previousState = m.state
					m.state = viewConfirmForget
					m.connectionStatusMsg = ""
				} else if ok {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render(fmt.Sprintf("%s not known.", item.StyledTitle()))
				} else {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("No item selected.")
				}
			case key.Matches(msg, m.keys.Info):
				if m.activeWifiConnection != nil && m.activeWifiDevice != "" {
					m.state = viewActiveConnectionInfo
					m.isLoading = true
					m.activeConnInfoViewport.SetContent("Loading...")
					m.activeConnInfoViewport.GotoTop()
					cmds = append(cmds, fetchActiveConnInfoCmd(m.activeWifiDevice), m.spinner.Tick)
					m.connectionStatusMsg = ""
				} else {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("No active connection.")
				}
			case key.Matches(msg, m.keys.Profiles):
				m.state = viewKnownNetworksList
				m.isLoading = true
				m.connectionStatusMsg = "Loading profiles..."
				m.knownWifiList.Title = "Loading..."
				cmds = append(cmds, fetchKnownWifiApsCmd(), m.spinner.Tick)
			case key.Matches(msg, m.keys.Connect):
				if item, ok := m.wifiList.SelectedItem().(wifiAP); ok {
					m.selectedAP = item
					ssid := item.getSSIDFromScannedAP()
					if item.IsActive {
						m.state = viewConfirmDisconnect
						m.connectionStatusMsg = ""
						break
					}
					sec := ""
					if item.WifiAccessPoint != nil {
						sec = strings.ToLower(item.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSecurity])
					}
					isOpen := sec == "" || sec == "open" || sec == "--"
					log.Printf("Connect: SSID '%s', Known: %t, Open: %t", ssid, item.IsKnown, isOpen)
					if isOpen || item.IsKnown {
						m.isLoading = true
						m.state = viewConnecting
						m.connectionStatusMsg = fmt.Sprintf("Connecting to %s...", item.StyledTitle())
						cmds = append(cmds, connectToWifiCmd(ssid, "", item.IsKnown), m.spinner.Tick)
					} else {
						m.state = viewPasswordInput
						m.passwordInput.SetValue("")
						m.passwordInput.Focus()
						m.connectionStatusMsg = ""
						cmds = append(cmds, textinput.Blink)
					}
				}
			default:
				// Pass all other keypresses to the list for navigation.
				m.wifiList, cmd = m.wifiList.Update(msg)
				cmds = append(cmds, cmd)
			}
		case viewPasswordInput: /* Same logic as before */ 
			passthrough := true
			switch {
			case key.Matches(msg, m.keys.Connect): m.isLoading = true; m.state = viewConnecting; m.connectionStatusMsg = fmt.Sprintf("Connecting to %s...", m.selectedAP.StyledTitle()); cmds = append(cmds, connectToWifiCmd(m.selectedAP.getSSIDFromScannedAP(), m.passwordInput.Value(), false), m.spinner.Tick); passthrough = false
			case key.Matches(msg, m.keys.Back): m.state = viewNetworksList; m.passwordInput.Blur(); m.connectionStatusMsg = ""; passthrough = false
			}
			if passthrough { m.passwordInput, cmd = m.passwordInput.Update(msg); cmds = append(cmds, cmd) }
		case viewConnectionResult: if key.Matches(msg, m.keys.Connect) || key.Matches(msg, m.keys.Back) { m.state = viewNetworksList; m.connectionStatusMsg = "" }
		case viewActiveConnectionInfo: if key.Matches(msg, m.keys.Back) { m.state = viewNetworksList; m.connectionStatusMsg = "" } else { m.activeConnInfoViewport, cmd = m.activeConnInfoViewport.Update(msg); cmds = append(cmds, cmd) }
		case viewConfirmDisconnect: /* Same */ 
			switch {
			case key.Matches(msg, m.keys.Connect): 
				m.isLoading = true; ssidD := m.selectedAP.StyledTitle(); m.connectionStatusMsg = fmt.Sprintf("Disconnecting from %s...", ssidD)
				pID := ""; if m.activeWifiConnection != nil { pID = (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID]; if pID == "" { pID = (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionName] }; if pID == "" { pID = gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection) } } else if m.selectedAP.IsActive { log.Printf("Warning: Disconnecting via selectedAP."); pID = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]; if pID == "" { pID = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName] }; if pID == "" { pID = m.selectedAP.getSSIDFromScannedAP() } }
				if pID == "" { m.connectionStatusMsg = errorStyle.Render("Cannot ID profile to disconnect."); m.isLoading = false; m.state = viewNetworksList; break }
				cmds = append(cmds, disconnectWifiCmd(pID), m.spinner.Tick)
			case key.Matches(msg, m.keys.Back): m.state = viewNetworksList; m.connectionStatusMsg = ""
			}
		case viewConfirmForget: /* Same */ 
			switch {
			case key.Matches(msg, m.keys.Connect):
				m.isLoading = true
				ssidForMsg := m.selectedAP.getSSIDFromScannedAP()
				if ssidForMsg == "" || ssidForMsg == "--" {
					ssidForMsg = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName]
				}

				// Get the profile identifier (UUID or Name) directly from the selected item,
				// which is reliable whether we came from the scan list or the profiles list.
				pID := m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]
				if pID == "" {
					pID = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName]
				}

				if pID == "" {
					// Fallback for safety, though it should be rare with the new flow.
					pID = ssidForMsg
					log.Printf("Warning: Forgetting by SSID '%s' as a fallback.", ssidForMsg)
				}

				if pID == "" {
					m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Cannot identify profile to forget for %s.", ssidForMsg))
					m.isLoading = false
					m.state = viewNetworksList
					break
				}

				m.connectionStatusMsg = fmt.Sprintf("Forgetting profile for %s...", ssidForMsg)
				cmds = append(cmds, forgetNetworkCmd(pID, ssidForMsg), m.spinner.Tick)

			case key.Matches(msg, m.keys.Back):
				m.state = viewNetworksList
				m.connectionStatusMsg = ""
			}
		case viewKnownNetworksList:
			if m.isLoading {
				break
			}
			switch {
			case key.Matches(msg, m.keys.Back):
				m.state = viewNetworksList
				m.connectionStatusMsg = ""
			case key.Matches(msg, m.keys.Forget):
				if item, ok := m.knownWifiList.SelectedItem().(wifiAP); ok {
					m.selectedAP = item
					m.previousState = m.state
					m.state = viewConfirmForget
					m.connectionStatusMsg = ""
				}
			default:
				m.knownWifiList, cmd = m.knownWifiList.Update(msg)
				cmds = append(cmds, cmd)
			}
		}
	}
	return m, tea.Batch(cmds...)
}

func (m model) View() string { /* Same as previous version with "Not enough space" logging */
	avW := m.width - appStyle.GetHorizontalFrameSize()
	var mainSb strings.Builder
	hView := m.headerView(avW)
	m.keys.currentState = m.state
	helpR := m.help.View(m.keys)
	fView := m.footerView(avW, helpR)
	hH := lipgloss.Height(hView)
	fH := lipgloss.Height(fView)
	cdh := m.height - appStyle.GetVerticalFrameSize() - hH - fH
	if cdh < 0 {
		cdh = 0
	}
	currMainS := ""
	switch m.state {
	case viewNetworksList:
		if m.isLoading && m.wifiList.FilterState() != list.Filtering {
			currMainS = lipgloss.Place(avW, cdh, lipgloss.Center, lipgloss.Center, m.spinner.Style.Render(lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View()+" ", m.wifiList.Title)))
		} else {
			listR := m.wifiList.View()
			if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
				currMainS = lipgloss.PlaceHorizontal(avW, lipgloss.Center, listR)
			} else {
				currMainS = listR
			}
		}
		if m.connectionStatusMsg != "" && m.state == viewNetworksList {
			statusR := m.connectionStatusMsg
			if !strings.HasPrefix(m.connectionStatusMsg, "\x1b[") {
				style := statusMessageBaseStyle.Copy()
				if strings.Contains(strings.ToLower(m.connectionStatusMsg), "unnamed") {
					style = toggleHiddenStatusMsgStyle
				} else if strings.Contains(m.connectionStatusMsg, "Wi-Fi is") {
					style = style.Foreground(ansTextColor)
				} else {
					style = style.Faint(true)
				}
				statusR = style.Render(m.connectionStatusMsg)
			}
			if (!m.isLoading || m.wifiList.FilterState() == list.Filtering) {
				if lipgloss.Height(currMainS)+lipgloss.Height(statusR) <= cdh {
					currMainS = lipgloss.JoinVertical(lipgloss.Top, currMainS, statusR)
				} else {
					log.Printf("Warn: No vspace for list+status. Status: %s", m.connectionStatusMsg)
				}
			}
		}
	case viewKnownNetworksList:
		if m.isLoading {
			currMainS = lipgloss.Place(avW, cdh, lipgloss.Center, lipgloss.Center, m.spinner.Style.Render(lipgloss.JoinHorizontal(lipgloss.Left, m.spinner.View()+" ", m.knownWifiList.Title)))
		} else {
			listR := m.knownWifiList.View()
			if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
				currMainS = lipgloss.PlaceHorizontal(avW, lipgloss.Center, listR)
			} else {
				currMainS = listR
			}
		}
	case viewPasswordInput:
		promptT := fmt.Sprintf("Password for %s:", m.selectedAP.StyledTitle())
		if m.connectionStatusMsg != "" {
			promptT = m.connectionStatusMsg
		}
		promptCW := m.passwordInput.Width + lipgloss.Width(m.passwordInput.Prompt) + passwordInputContainerStyle.GetHorizontalFrameSize() + 4
		if promptCW > avW*4/5 {
			promptCW = avW * 4 / 5
		}
		if promptCW < 40 {
			promptCW = 40
		}
		cP := lipgloss.NewStyle().Width(promptCW).Align(lipgloss.Center).Render(promptT)
		inputR := m.passwordInput.View()
		pwBlock := lipgloss.JoinVertical(lipgloss.Top, cP, inputR)
		if m.passwordInput.Err != nil {
			pwBlock = lipgloss.JoinVertical(lipgloss.Top, pwBlock, errorStyle.Render(m.passwordInput.Err.Error()))
		}
		currMainS = passwordInputContainerStyle.Render(pwBlock)
	case viewConnecting:
		currMainS = connectingStyle.Render(fmt.Sprintf("\n%s %s\n", m.spinner.View(), m.connectionStatusMsg))
	case viewConnectionResult:
		msgR := m.connectionStatusMsg
		msgBW := avW * 3 / 4
		if msgBW > 80 {
			msgBW = 80
		}
		if msgBW < 40 {
			msgBW = 40
		}
		wrapMsg := lipgloss.NewStyle().Width(msgBW).Align(lipgloss.Center).Render(msgR)
		hint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter/Esc to return)")
		currMainS = lipgloss.JoinVertical(lipgloss.Center, wrapMsg, "", hint)
	case viewActiveConnectionInfo:
		currMainS = m.activeConnInfoViewport.View()
	case viewConfirmDisconnect:
		currMainS = lipgloss.JoinVertical(lipgloss.Center, fmt.Sprintf("Disconnect from %s?", m.selectedAP.StyledTitle()), "\n", lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter to confirm, Esc to cancel)"))
	case viewConfirmForget:
		currMainS = lipgloss.JoinVertical(lipgloss.Center, fmt.Sprintf("Forget profile for\n%s?", m.selectedAP.StyledTitle()), "\n", lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter to confirm, Esc to cancel)"))
	}
	if m.state != viewNetworksList && m.state != viewActiveConnectionInfo && m.state != viewKnownNetworksList {
		currMainS = lipgloss.Place(avW, cdh, lipgloss.Center, lipgloss.Center, currMainS)
	}
	mainSb.WriteString(currMainS)
	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top, hView, mainSb.String(), fView))
}
func (m model) headerView(w int) string { /* Same */ 
	s := "Wi-Fi: "; if m.wifiEnabled { s += wifiStatusStyleEnabled.Render("Enabled ✔") } else { s += wifiStatusStyleDisabled.Render("Disabled ✘") }; t := titleStyle.Render("Go Network Manager TUI")
	sp := w - lipgloss.Width(t) - lipgloss.Width(s); if sp < 1 { sp = 1 }; return lipgloss.JoinHorizontal(lipgloss.Left, t, strings.Repeat(" ", sp), s)
}
func (m model) footerView(w int, h string) string { /* Same */ return lipgloss.PlaceHorizontal(w, lipgloss.Center, helpGlobalStyle.Render(h)) }

func main() { /* Same log setup */ 
	logOut := io.Discard; var logFH *os.File
	if os.Getenv("DEBUG_TEA") != "" { var err error; logFH, err = os.OpenFile(debugLogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil { fmt.Fprintf(os.Stderr, "Err open log %s: %v\n", debugLogFile, err) } else { logOut = logFH; defer func() { log.Println("--- NMTUI Log End ---"); if err := logFH.Close(); err != nil { fmt.Fprintf(os.Stderr, "Err close log: %v\n", err) } }() }
	}
	log.SetOutput(logOut); log.SetFlags(log.Ltime | log.Lshortfile); if os.Getenv("DEBUG_TEA") != "" && logOut != io.Discard { log.Println("--- NMTUI Log Start ---") }
	im := initialModel(); p := tea.NewProgram(im, tea.WithAltScreen(), tea.WithMouseCellMotion())
	fm, err := p.Run()
	if err != nil { log.Printf("Err run TUI: %v", err); if fmm, ok := fm.(model); ok { log.Printf("Final model on err: %+v", fmm) }; if logOut == io.Discard { fmt.Fprintf(os.Stderr, "Err run TUI: %v\n", err) }; os.Exit(1) }
}

