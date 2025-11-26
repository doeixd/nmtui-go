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
	cacheFile               = "/tmp/nmtui-cache.json"
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

	signalVal, _ := strconv.Atoi(signalStr)

	// If this is a known network with no signal, it's out of range
	if ap.IsKnown && signalVal == 0 {
		descParts = append(descParts, labelStyle.Render("Known (Out of Range)"))
	} else if signalStr != "" {
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
	filterInput            textinput.Model
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
	isScanning             bool
	isFiltering            bool
	filterQuery            string
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
	fi := textinput.New(); fi.Placeholder = "Type to filter..."; fi.CharLimit = 100
	fi.Prompt = "/ "; fi.Cursor.Style = lipgloss.NewStyle().Foreground(ansPrimaryColor)
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
		filterInput:   fi,
		spinner:       s,
		activeConnInfoViewport: vp,
		isLoading:              true,
		isScanning:             true,
		isFiltering:            false,
		filterQuery:            "",
		keys:                   defaultKeyBindings,
		help:                   h,
		knownProfiles:          make(map[string]gonetworkmanager.ConnectionProfile),
		showHiddenNetworks:     false,
	}
	m.keys.currentState = m.state

	// Load cached networks if available
	if cachedAps := loadCachedNetworks(); cachedAps != nil {
		m.allScannedAps = cachedAps
		m.processAndSetWifiList(cachedAps)
	}

	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(getWifiStatusInternalCmd(), fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
}

func loadCachedNetworks() []wifiAP {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil
	}
	var cached []wifiAP
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil
	}
	return cached
}

func saveCachedNetworks(aps []wifiAP) {
	data, err := json.Marshal(aps)
	if err != nil {
		return
	}
	os.WriteFile(cacheFile, data, 0644)
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
		if err != nil { 
			log.Printf("Cmd: Error fetching known profiles: %v", err)
			return knownNetworksMsg{} 
		}

		log.Printf("Cmd: Got %d total profiles", len(profiles))

		known := make(map[string]gonetworkmanager.ConnectionProfile)
		var activeConn *gonetworkmanager.ConnectionProfile
		var activeDev string
		
		activeDevProfiles, _ := gonetworkmanager.GetConnectionProfilesList(true)
		log.Printf("Cmd: Got %d active profiles", len(activeDevProfiles))
		
		activeUUIDs := make(map[string]struct{})
		for _, adp := range activeDevProfiles {
			connType := adp[gonetworkmanager.NmcliFieldConnectionType]
			log.Printf("Cmd: Active profile type: '%s', UUID: %s", connType, adp[gonetworkmanager.NmcliFieldConnectionUUID])
			if connType == gonetworkmanager.ConnectionTypeWifi {
				activeUUIDs[adp[gonetworkmanager.NmcliFieldConnectionUUID]] = struct{}{}
			}
		}

		for _, p := range profiles {
			connType := p[gonetworkmanager.NmcliFieldConnectionType]
			log.Printf("Cmd: Profile '%s' type: '%s'", p[gonetworkmanager.NmcliFieldConnectionName], connType)

			if connType == gonetworkmanager.ConnectionTypeWifi {
				ssid := gonetworkmanager.GetSSIDFromProfile(p)
				log.Printf("Cmd: WiFi profile SSID from fields: '%s'", ssid)

				// If SSID is not in the profile (which happens with 'nmcli connection show --order name'),
				// use the connection name as the SSID for WiFi connections
				if ssid == "" {
					ssid = p[gonetworkmanager.NmcliFieldConnectionName]
					log.Printf("Cmd: Using connection name as SSID: '%s'", ssid)
				}

				if ssid != "" {
					known[ssid] = p
					if _, isActive := activeUUIDs[p[gonetworkmanager.NmcliFieldConnectionUUID]]; isActive {
						pCopy := make(gonetworkmanager.ConnectionProfile)
						for k, v := range p {
							pCopy[k] = v
						}
						activeConn = &pCopy
						activeDev = p[gonetworkmanager.NmcliFieldConnectionDevice]
						log.Printf("Cmd: Found active WiFi connection: %s (device: %s)", ssid, activeDev)
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

func (m *model) applyFilterAndUpdateList() {
	// Get all items (known + scanned)
	allItems := m.getAllWifiItems()

	// Apply filter if query is not empty
	var filteredItems []list.Item
	if m.filterQuery != "" {
		query := strings.ToLower(m.filterQuery)
		for _, item := range allItems {
			ap := item.(wifiAP)
			ssid := strings.ToLower(ap.getSSIDFromScannedAP())
			if strings.Contains(ssid, query) {
				filteredItems = append(filteredItems, item)
			}
		}
	} else {
		filteredItems = allItems
	}

	m.wifiList.SetItems(filteredItems)

	// Update title
	knownCount := 0
	availableCount := 0
	for _, item := range filteredItems {
		ap := item.(wifiAP)
		if ap.IsKnown {
			knownCount++
		} else {
			availableCount++
		}
	}

	hiddenStatus := ""
	if !m.showHiddenNetworks {
		hiddenStatus = listTitleHiddenStatusStyle.Render(" (hiding unnamed)")
	}
	filterStatus := ""
	if m.filterQuery != "" {
		filterStatus = lipgloss.NewStyle().Foreground(ansPrimaryColor).Render(fmt.Sprintf(" [filtered: %d/%d]", len(filteredItems), len(allItems)))
	}
	m.wifiList.Title = fmt.Sprintf("Wi-Fi Networks: %d Known, %d Available%s%s", knownCount, availableCount, hiddenStatus, filterStatus)
}

func (m *model) getAllWifiItems() []list.Item {
	log.Printf("GetAllWifiItems: Processing %d scanned APs, %d known profiles, active conn: %v",
		len(m.allScannedAps), len(m.knownProfiles), m.activeWifiConnection != nil)

	// Deduplicate scanned APs by SSID, keeping the one with the strongest signal
	deduplicatedAps := make(map[string]wifiAP)
	for _, ap := range m.allScannedAps {
		ssid := ap.getSSIDFromScannedAP()
		if ssid == "" || ssid == "--" {
			// For hidden networks, each one is unique, so add them all
			// Use a unique key combining SSID and BSSID if available
			bssid := ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiBSSID]
			key := "hidden|" + bssid
			// If BSSID is missing, use a random unique key or just append
			if bssid == "" {
				key = fmt.Sprintf("hidden|%d", len(deduplicatedAps))
			}
			deduplicatedAps[key] = ap
		} else {
			// For named networks, keep the one with the strongest signal
			newSignal, _ := strconv.Atoi(ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
			if existing, ok := deduplicatedAps[ssid]; ok {
				existingSignal, _ := strconv.Atoi(existing.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
				if newSignal > existingSignal {
					deduplicatedAps[ssid] = ap
					log.Printf("ProcessList: Keeping stronger signal for '%s': %d > %d", ssid, newSignal, existingSignal)
				}
			} else {
				deduplicatedAps[ssid] = ap
			}
		}
	}

	// Convert deduplicated map back to slice
	var deduplicatedSlice []wifiAP
	for _, ap := range deduplicatedAps {
		deduplicatedSlice = append(deduplicatedSlice, ap)
	}

	// First, collect all known profiles that aren't in the scanned list
	knownNetworksNotInScan := make(map[string]wifiAP)
	for ssid, profile := range m.knownProfiles {
		// Check if this known network is in the deduplicated scanned list
		found := false
		for _, ap := range deduplicatedSlice {
			if ap.getSSIDFromScannedAP() == ssid {
				found = true
				break
			}
		}
		if !found {
			// This known profile wasn't scanned, add it to the list
			knownAP := connectionProfileToWifiAP(profile, m.activeWifiConnection)
			knownNetworksNotInScan[ssid] = knownAP
		}
	}

	// Filter deduplicated APs based on hidden network settings
	var filteredAps []wifiAP
	for _, ap := range deduplicatedSlice {
		ssid := ap.getSSIDFromScannedAP()
		isUnnamed := ssid == "" || ssid == "--"
		if m.showHiddenNetworks || !isUnnamed {
			filteredAps = append(filteredAps, ap)
		}
	}

	// Add known networks that weren't in the scan to the filtered list
	for _, knownAP := range knownNetworksNotInScan {
		filteredAps = append(filteredAps, knownAP)
	}

	enrichedAps := make([]list.Item, len(filteredAps))
	foundActive := false
	for i, ap := range filteredAps {
		pAP := ap
		ssid := pAP.getSSIDFromScannedAP()
		pAP.IsKnown, pAP.IsActive = false, false
		if ssid != "" && ssid != "--" {
			if profile, ok := m.knownProfiles[ssid]; ok {
				pAP.IsKnown = true
				if m.activeWifiConnection != nil && profile[gonetworkmanager.NmcliFieldConnectionUUID] == (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID] {
					pAP.IsActive = true
					pAP.Interface = profile[gonetworkmanager.NmcliFieldConnectionDevice]
					foundActive = true
				}
			}
		}
		enrichedAps[i] = pAP
	}
	if !foundActive && m.activeWifiConnection != nil {
		log.Println("ProcessList: Active conn (", gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection), ") not in scan.")
	}
	sort.SliceStable(enrichedAps, func(i, j int) bool {
		itemI, itemJ := enrichedAps[i].(wifiAP), enrichedAps[j].(wifiAP)
		if itemI.IsActive != itemJ.IsActive {
			return itemI.IsActive
		}
		if itemI.IsKnown != itemJ.IsKnown {
			return itemI.IsKnown
		}
		sigi, errI := strconv.Atoi(itemI.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
		if errI != nil { sigi = -1 }
		sigj, errJ := strconv.Atoi(itemJ.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
		if errJ != nil { sigj = -1 }

		// Among known networks, show those in range (signal > 0) before those out of range
		if itemI.IsKnown && itemJ.IsKnown {
			inRangeI := sigi > 0
			inRangeJ := sigj > 0
			if inRangeI != inRangeJ {
				return inRangeI
			}
		}

		if sigi != sigj {
			return sigi > sigj
		}
		ssidi, ssidj := strings.ToLower(itemI.getSSIDFromScannedAP()), strings.ToLower(itemJ.getSSIDFromScannedAP())
		isIUn := ssidi == "" || ssidi == "--"
		isJUn := ssidj == "" || ssidj == "--"
		if isIUn && !isJUn {
			return false
		}
		if !isIUn && isJUn {
			return true
		}
		return ssidi < ssidj
	})

	return enrichedAps
}

func (m *model) processAndSetWifiList(apsToProcess []wifiAP) {
	m.allScannedAps = apsToProcess
	m.applyFilterAndUpdateList()
}


func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd; var cmd tea.Cmd
	m.keys.currentState = m.state

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.width == 0 || m.height == 0 { return m, nil }
		appStyleHorizontalFrame := appStyle.GetHorizontalFrameSize(); appStyleVerticalFrame := appStyle.GetVerticalFrameSize()
		availableWidth := m.width - appStyleHorizontalFrame; availableHeight := m.height - appStyleVerticalFrame
		desiredHelpWidth := int(float64(availableWidth) * helpBarWidthPercent); if desiredHelpWidth > helpBarMaxWidth { desiredHelpWidth = helpBarMaxWidth }; if desiredHelpWidth < 20 { desiredHelpWidth = 20 }
		m.help.Width = desiredHelpWidth
		headerHeight := lipgloss.Height(m.headerView(availableWidth))
		tempKeyMapState := m.keys; tempKeyMapState.currentState = m.state // Use current state for accurate footer height
		footerHeight := lipgloss.Height(m.footerView(availableWidth, m.help.View(tempKeyMapState)))
		contentAreaHeight := availableHeight - headerHeight - footerHeight
		if contentAreaHeight < 0 { contentAreaHeight = 0 }

		// Reserve space for filter input if filtering (border + padding + content = ~3 lines)
		listContentHeight := contentAreaHeight
		if m.isFiltering {
			listContentHeight -= 4 // Reserve space for filter input
			if listContentHeight < 5 {
				listContentHeight = 5 // Minimum height for list
			}
		}

		listWidth := availableWidth
		if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
			calcW := availableWidth; if networkListWidthPercent > 0 { calcW = int(float64(availableWidth) * networkListWidthPercent) }
			if networkListFixedWidth > 0 && calcW > networkListFixedWidth { calcW = networkListFixedWidth }; if calcW < 40 { calcW = 40 }; listWidth = calcW
		}
		m.listDisplayWidth = listWidth // Store calculated list width
		m.wifiList.SetSize(m.listDisplayWidth, listContentHeight)
		m.knownWifiList.SetSize(m.listDisplayWidth, listContentHeight)
		m.activeConnInfoViewport.Width = availableWidth - infoBoxStyle.GetHorizontalFrameSize()
		m.activeConnInfoViewport.Height = contentAreaHeight - infoBoxStyle.GetVerticalFrameSize(); if m.activeConnInfoViewport.Height < 0 {m.activeConnInfoViewport.Height = 0}
		pwInputContentWidth := availableWidth*2/3; if pwInputContentWidth > 60 { pwInputContentWidth = 60 }; if pwInputContentWidth < 40 { pwInputContentWidth = 40 }
		m.passwordInput.Width = pwInputContentWidth - lipgloss.Width(m.passwordInput.Prompt) - passwordInputContainerStyle.GetHorizontalFrameSize()

	case spinner.TickMsg: if m.isLoading { m.spinner, cmd = m.spinner.Update(msg); cmds = append(cmds, cmd) }
	case wifiStatusMsg:
		m.isLoading = false
		if msg.err != nil { 
			if m.state == viewNetworksList { 
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error Wi-Fi status: %v", msg.err)) 
			}
		} else {
			m.wifiEnabled = msg.enabled
			statusText := "disabled"
			if m.wifiEnabled { 
				statusText = "enabled" 
			}
			if m.state == viewNetworksList { 
				m.connectionStatusMsg = fmt.Sprintf("Wi-Fi is %s.", statusText) 
			}
			if m.wifiEnabled { 
				m.isLoading = true
				m.isScanning = true
				// Keep existing cached networks visible while scanning
				if len(m.allScannedAps) > 0 {
					m.wifiList.Title = "Scanning..."
				} else {
					m.wifiList.Title = "Scanning..."
				}
				cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick)
			} else { 
				m.allScannedAps = nil
				m.isScanning = false
				m.processAndSetWifiList([]wifiAP{})
				m.wifiList.Title = "Wi-Fi is Disabled"
				m.activeWifiConnection = nil
				m.activeWifiDevice = ""
				if m.state == viewNetworksList { 
					m.connectionStatusMsg = "Wi-Fi is disabled." 
				}
			}
		}
	case knownNetworksMsg: 
		m.knownProfiles, m.activeWifiConnection, m.activeWifiDevice = msg.knownProfiles, msg.activeWifiConnection, msg.activeWifiDevice
		// Always reprocess the list when known networks are updated
		if len(m.allScannedAps) > 0 {
			// We have scanned APs, reprocess with updated known profiles
			m.processAndSetWifiList(m.allScannedAps)
		} else if m.isLoading || m.isScanning {
			// Still scanning - keep any cached networks visible or just update title
			if len(m.allScannedAps) > 0 {
				m.processAndSetWifiList(m.allScannedAps)
			}
			totalKnown := len(m.knownProfiles)
			if totalKnown > 0 {
				m.wifiList.Title = fmt.Sprintf("Scanning for networks... (%d known)", totalKnown)
			} else {
				m.wifiList.Title = "Scanning..."
			}
		}
	case wifiListLoadedMsg:
		if msg.err != nil { 
			m.isLoading = false
			m.isScanning = false
			if m.state == viewNetworksList { 
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error fetching Wi-Fi: %v", msg.err)) 
			}
			m.wifiList.Title = "Error Loading Networks"
		} else { 
			// Only update if we have new results
			if len(msg.allAps) > 0 {
				m.isLoading = false
				m.isScanning = false
				m.allScannedAps = msg.allAps
				m.processAndSetWifiList(m.allScannedAps)
				// Cache the networks for next startup
				go saveCachedNetworks(msg.allAps)
			} else {
				// Empty results - this can happen during scanning, so just ignore
				// Keep displaying cached networks and keep scanning indicator active
				log.Printf("Scan returned 0 results, keeping cached networks visible")
			}
		}
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
			// If we're filtering, handle filter input
			if m.isFiltering {
				switch {
				case key.Matches(msg, m.keys.Back) || msg.String() == "esc":
					// Cancel filtering and clear filter - return to default view
					m.isFiltering = false
					m.filterQuery = ""
					m.filterInput.SetValue("")
					m.filterInput.Blur()
					m.connectionStatusMsg = ""
					m.applyFilterAndUpdateList() // This will show all networks since filterQuery is empty
					// Trigger resize to restore list height
					cmds = append(cmds, func() tea.Msg {
						return tea.WindowSizeMsg{Width: m.width, Height: m.height}
					})
					return m, tea.Batch(cmds...)

				case msg.String() == "enter":
					// Accept filter - keep current filter but stop editing
					m.isFiltering = false
					m.filterInput.Blur()
					m.connectionStatusMsg = ""
					// Keep the filter query active, just stop showing the input
					// Trigger resize to restore list height
					cmds = append(cmds, func() tea.Msg {
						return tea.WindowSizeMsg{Width: m.width, Height: m.height}
					})
					return m, tea.Batch(cmds...)

				default:
					// Update filter input
					m.filterInput, cmd = m.filterInput.Update(msg)
					cmds = append(cmds, cmd)
					m.filterQuery = m.filterInput.Value()
					m.applyFilterAndUpdateList()
				}
				return m, tea.Batch(cmds...)
			}

			// Not filtering - handle normal keys
			if m.isLoading {
				break
			}

			switch {
			case key.Matches(msg, m.keys.Back) || msg.String() == "esc":
				// If a filter is active (but not currently editing), clear it
				if m.filterQuery != "" {
					m.filterQuery = ""
					m.filterInput.SetValue("")
					m.connectionStatusMsg = ""
					m.applyFilterAndUpdateList()
					break
				}
				// Otherwise, let it fall through to default behavior
				m.wifiList, cmd = m.wifiList.Update(msg)
				cmds = append(cmds, cmd)

			case key.Matches(msg, m.keys.ToggleHidden):
				m.showHiddenNetworks = !m.showHiddenNetworks
				m.applyFilterAndUpdateList()
				if m.showHiddenNetworks {m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Showing unnamed.")} else {m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Hiding unnamed.")}
			case key.Matches(msg, m.keys.Filter):
				// Start filtering
				m.isFiltering = true
				m.filterInput.SetValue(m.filterQuery)
				m.filterInput.Focus()
				m.connectionStatusMsg = "Type to filter networks, ESC to cancel..."
				cmds = append(cmds, textinput.Blink)
				// Trigger resize to adjust list height
				cmds = append(cmds, func() tea.Msg {
					return tea.WindowSizeMsg{Width: m.width, Height: m.height}
				})
			case key.Matches(msg, m.keys.Refresh):
				m.isLoading = true
				m.isScanning = true
				m.connectionStatusMsg = ""
				m.filterQuery = ""
				m.isFiltering = false
				m.filterInput.SetValue("")
				// Don't clear the list - keep showing cached networks while scanning
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
		// Always show the list, even when loading
		listR := m.wifiList.View()

		// Render filter input if filtering
		if m.isFiltering {
			filterStyle := lipgloss.NewStyle().
				BorderStyle(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("62")).
				Padding(0, 1)
			filterR := filterStyle.Render(m.filterInput.View())

			// Combine list and filter vertically
			combined := lipgloss.JoinVertical(lipgloss.Top, listR, "", filterR)

			if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
				currMainS = lipgloss.PlaceHorizontal(avW, lipgloss.Center, combined)
			} else {
				currMainS = combined
			}
		} else {
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
			if (!m.isLoading || m.isFiltering) {
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
func (m model) headerView(w int) string {
	t := titleStyle.Render("Go Network Manager TUI")

	// Scanning indicator
	scanIndicator := ""
	if m.isScanning {
		scanIndicator = connectingStyle.Render(" " + m.spinner.View() + " Scanning...")
	}

	s := "Wi-Fi: "
	if m.wifiEnabled {
		s += wifiStatusStyleEnabled.Render("Enabled ✔")
	} else {
		s += wifiStatusStyleDisabled.Render("Disabled ✘")
	}

	// Calculate spacing
	fixedWidth := lipgloss.Width(t) + lipgloss.Width(s)
	scanWidth := lipgloss.Width(scanIndicator)
	totalWidth := fixedWidth + scanWidth

	if totalWidth >= w {
		// Not enough space, just show title and status
		sp := w - fixedWidth
		if sp < 1 {
			sp = 1
		}
		return lipgloss.JoinHorizontal(lipgloss.Left, t, strings.Repeat(" ", sp), s)
	}

	// Distribute remaining space
	remainingSpace := w - totalWidth
	leftSpace := remainingSpace / 2
	rightSpace := remainingSpace - leftSpace

	if leftSpace < 1 {
		leftSpace = 1
	}
	if rightSpace < 1 {
		rightSpace = 1
	}

	return lipgloss.JoinHorizontal(lipgloss.Left, t, strings.Repeat(" ", leftSpace), scanIndicator, strings.Repeat(" ", rightSpace), s)
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

