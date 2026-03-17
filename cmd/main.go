// Package main implements a Terminal User Interface (TUI) for managing NetworkManager Wi-Fi connections.
// It allows users to scan for networks, connect to secured and open networks, view connection details,
// manage known profiles, and toggle Wi-Fi radio status.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

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
	appName                 = "Network Manager"
	cacheFileName           = "nmtui-cache.json"
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

	titleStyle            = lipgloss.NewStyle().Bold(true).Foreground(ansPrimaryColor).Padding(0, 1).MarginBottom(1)
	listTitleStyle        = lipgloss.NewStyle().Foreground(ansSecondaryColor).Padding(0, 1).Bold(true)
	listItemStyle         = lipgloss.NewStyle().PaddingLeft(2).Foreground(ansTextColor)
	listSelectedItemStyle = lipgloss.NewStyle().PaddingLeft(1).Foreground(ansPrimaryColor).Bold(true)
	listDescStyle         = lipgloss.NewStyle().PaddingLeft(2).Foreground(ansFaintTextColor)
	listSelectedDescStyle = lipgloss.NewStyle().PaddingLeft(1).Foreground(ansPrimaryColor)
	listNoItemsStyle      = lipgloss.NewStyle().Faint(true).Margin(1, 0).Align(lipgloss.Center).Foreground(ansFaintTextColor)

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
	viewProfileDetails
	viewProfileCreate
	viewProfileEdit
	viewUpdating
)

type itemDelegate struct{}

func (d itemDelegate) Height() int                             { return 2 }
func (d itemDelegate) Spacing() int                            { return 1 }
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
		indicator += lipgloss.NewStyle().Foreground(ansSuccessColor).Render(" ")
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

	signalVal, _ := strconv.Atoi(signalStr)

	// If this is a known network with no signal, it's out of range
	if ap.IsKnown && signalVal == 0 {
		descParts = append(descParts, labelStyle.Render("Known (Out of Range)"))
	} else if signalStr != "" {
		var sStyle lipgloss.Style
		switch {
		case signalVal > 70:
			sStyle = lipgloss.NewStyle().Foreground(ansSuccessColor)
		case signalVal > 40:
			sStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		default:
			sStyle = lipgloss.NewStyle().Foreground(ansErrorColor)
		}
		descParts = append(descParts, fmt.Sprintf("%s %s", labelStyle.Render("Signal:"), sStyle.Render(signalStr+"%")))
	}

	if security == "" || security == "--" {
		security = "Open"
	}
	descParts = append(descParts, fmt.Sprintf("%s %s", labelStyle.Render("Security:"), labelStyle.Render(security)))
	return strings.Join(descParts, labelStyle.Render(" | "))
}
func (ap wifiAP) FilterValue() string {
	ssid := ap.getSSIDFromScannedAP()
	if ssid == "" || ssid == "--" {
		return "<Hidden Network>"
	}
	return ssid
}

type wifiListLoadedMsg struct {
	allAps []wifiAP
	err    error
}
type connectionAttemptMsg struct {
	ssid                 string
	success              bool
	err                  error
	WasKnownAttemptNoPsk bool
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
	success bool
	err     error
	ssid    string
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

type profileLoadedMsg struct {
	profile gonetworkmanager.ConnectionProfile
	err     error
	forEdit bool
}

type profileSaveResultMsg struct {
	success    bool
	err        error
	action     string
	profileRef string
}

type updateCheckMsg struct {
	latestVersion string
	updateAvail   bool
	err           error // always swallowed in TUI context
}

type updateProgressMsg struct {
	step    UpdateStep
	message string
}

type updateCompleteMsg struct {
	newVersion string
	err        error
}

type keyMap struct {
	Connect, Refresh, Quit, Back, Help, Filter, ToggleWifi, Disconnect, Info, ToggleHidden, Forget, Profiles, NewProfile, EditProfile, ClearSecret, Update key.Binding
	currentState                                                                                                                                          viewState
}

func (k keyMap) ShortHelp() []key.Binding {
	b := []key.Binding{k.Help}
	switch k.currentState {
	case viewNetworksList:
		b = append(b, k.Connect, k.Refresh, k.Filter, k.ToggleWifi, k.Update)
	case viewPasswordInput, viewConnectionResult, viewConfirmDisconnect, viewConfirmForget:
		b = append(b, k.Connect, k.Back)
	case viewKnownNetworksList:
		b = append(b, k.Connect, k.NewProfile, k.EditProfile, k.Forget)
	case viewActiveConnectionInfo:
		b = append(b, k.Back)
	case viewProfileDetails:
		b = append(b, k.Back, k.EditProfile, k.Forget)
	case viewProfileCreate, viewProfileEdit:
		b = append(b, k.Connect, k.Back, k.ClearSecret)
	}
	return append(b, k.Quit)
}
func (k keyMap) FullHelp() [][]key.Binding {
	switch k.currentState {
	default: // viewNetworksList
		return [][]key.Binding{
			{k.Help, k.Connect, k.Back, k.Quit},
			{k.Refresh, k.Filter, k.ToggleHidden, k.ToggleWifi},
			{k.Disconnect, k.Forget, k.Info, k.Profiles, k.Update},
		}
	case viewKnownNetworksList:
		return [][]key.Binding{{k.Connect, k.NewProfile, k.EditProfile, k.Forget}, {k.Refresh, k.Back, k.Quit}}
	case viewProfileDetails:
		return [][]key.Binding{{k.Back, k.EditProfile, k.Forget, k.Quit}}
	case viewProfileCreate, viewProfileEdit:
		return [][]key.Binding{{k.Connect, k.Back, k.ClearSecret, k.Quit}}
	}
}

var defaultKeyBindings = keyMap{
	Connect:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter/l", "select/conn/confirm")),
	Refresh:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
	Quit:         key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Back:         key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc/h", "back/cancel")),
	Help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	Filter:       key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	ToggleWifi:   key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "toggle Wi-Fi")),
	Disconnect:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "disconnect")),
	Forget:       key.NewBinding(key.WithKeys("ctrl+f"), key.WithHelp("ctrl+f", "forget")),
	Info:         key.NewBinding(key.WithKeys("i"), key.WithHelp("i", "info")),
	ToggleHidden: key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "unnamed nets")),
	Profiles:     key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "profiles")),
	NewProfile:   key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new profile")),
	EditProfile:  key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit profile")),
	ClearSecret:  key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "clear password")),
	Update:       key.NewBinding(key.WithKeys("U"), key.WithHelp("U", "update")),
}

type model struct {
	state                       viewState
	previousState               viewState
	wifiList                    list.Model
	knownWifiList               list.Model
	passwordInput               textinput.Model
	filterInput                 textinput.Model
	spinner                     spinner.Model
	activeConnInfoViewport      viewport.Model
	selectedAP                  wifiAP
	connectionStatusMsg         string
	lastConnectionWasSuccessful bool
	wifiEnabled                 bool
	knownProfiles               map[string]gonetworkmanager.ConnectionProfile
	activeWifiConnection        *gonetworkmanager.ConnectionProfile
	activeWifiDevice            string
	allScannedAps               []wifiAP
	showHiddenNetworks          bool
	isLoading                   bool
	isScanning                  bool
	isFiltering                 bool
	filterQuery                 string
	width, height               int
	listDisplayWidth            int
	keys                        keyMap
	help                        help.Model
	profileForm                 profileFormState
	profileDetailsID            string
	updateAvailable             bool
	updateLatestVersion         string
	updateStatusMsg             string
	updateError                 error
	updateNewVersion            string
	isUpdating                  bool
	wantsRestart                bool
	allowPrerelease             bool
	updateCancelFn              context.CancelFunc
}

type profileFormMode int

const (
	profileFormCreate profileFormMode = iota
	profileFormEdit
)

const (
	profileFieldName = iota
	profileFieldSSID
	profileFieldSecurity
	profileFieldPassword
	profileFieldAutoconnect
	profileFieldHidden
	profileFieldPriority
	profileFieldCount
)

var profileFieldLabels = []string{"Name", "SSID", "Security (open|wpa-psk)", "Password", "Autoconnect (yes|no)", "Hidden (yes|no)", "Priority (blank or integer)"}

type profileFormState struct {
	mode          profileFormMode
	profileID     string
	inputs        []textinput.Model
	initialValues []string
	focusIndex    int
	statusMsg     string
	clearPassword bool
	discardArmed  bool
}

func initialModel() model {
	delegate := itemDelegate{}
	l := list.New([]list.Item{}, delegate, 0, 0)
	l.Title = "Scanning for Wi-Fi Networks..."
	l.Styles.Title = listTitleStyle
	l.SetShowStatusBar(true)
	l.SetStatusBarItemName("network", "networks")
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	l.Styles.NoItems = listNoItemsStyle.Copy().SetString("No Wi-Fi. Try (r)efresh, (t)oggle Wi-Fi, (u)nnamed.")
	l.Styles.FilterPrompt = lipgloss.NewStyle().Foreground(ansPrimaryColor)
	l.Styles.FilterCursor = lipgloss.NewStyle().Foreground(ansPrimaryColor)
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{defaultKeyBindings.Filter, defaultKeyBindings.Refresh, defaultKeyBindings.ToggleHidden}
	}
	l.AdditionalFullHelpKeys = l.AdditionalShortHelpKeys

	ti := textinput.New()
	ti.Placeholder = "Network Password"
	ti.EchoMode = textinput.EchoPassword
	ti.CharLimit = 63
	ti.Prompt = passwordPromptStyle.Render("🔑 Password: ")
	ti.EchoCharacter = '•'
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(ansAccentColor)

	fi := textinput.New()
	fi.Placeholder = "Type to filter..."
	fi.CharLimit = 100
	fi.Prompt = "/ "
	fi.Cursor.Style = lipgloss.NewStyle().Foreground(ansPrimaryColor)

	s := spinner.New()
	s.Spinner = spinner.Globe
	s.Style = connectingStyle
	vp := viewport.New(0, 0)
	vp.Style = infoBoxStyle.Copy()
	h := help.New()
	h.ShowAll = false
	subtleHelp := lipgloss.NewStyle().Foreground(ansFaintTextColor)
	h.Styles = help.Styles{ShortKey: subtleHelp, ShortDesc: subtleHelp, FullKey: subtleHelp, FullDesc: subtleHelp, Ellipsis: subtleHelp.Copy()}
	pl := list.New([]list.Item{}, delegate, 0, 0)
	pl.Title = "Known Wi-Fi Profiles"
	pl.Styles.Title = listTitleStyle
	pl.SetShowStatusBar(false)
	pl.SetShowHelp(false)
	pl.DisableQuitKeybindings()
	pl.Styles.NoItems = listNoItemsStyle.Copy().SetString("No known Wi-Fi profiles found.")

	profileInputs := make([]textinput.Model, profileFieldCount)
	for i := range profileInputs {
		inp := textinput.New()
		inp.Prompt = ""
		inp.CharLimit = 256
		inp.Cursor.Style = lipgloss.NewStyle().Foreground(ansAccentColor)
		profileInputs[i] = inp
	}
	profileInputs[profileFieldSecurity].SetValue("wpa-psk")
	profileInputs[profileFieldAutoconnect].SetValue("yes")
	profileInputs[profileFieldHidden].SetValue("no")
	profileInputs[profileFieldPassword].Placeholder = "leave blank"
	profileInputs[profileFieldPassword].EchoMode = textinput.EchoPassword
	profileInputs[profileFieldPassword].EchoCharacter = '•'
	profileInputs[profileFieldPassword].CharLimit = 63

	m := model{
		state:                  viewNetworksList,
		wifiList:               l,
		knownWifiList:          pl,
		passwordInput:          ti,
		filterInput:            fi,
		spinner:                s,
		activeConnInfoViewport: vp,
		isLoading:              true,
		isScanning:             true,
		isFiltering:            false,
		filterQuery:            "",
		keys:                   defaultKeyBindings,
		help:                   h,
		profileForm: profileFormState{
			mode:       profileFormCreate,
			inputs:     profileInputs,
			focusIndex: 0,
		},
		knownProfiles:      make(map[string]gonetworkmanager.ConnectionProfile),
		showHiddenNetworks: false,
		allowPrerelease:    getAllowPrereleaseConfig(),
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
	return tea.Batch(getWifiStatusInternalCmd(), fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true), m.spinner.Tick, checkForUpdateCmd())
}

func checkForUpdateCmd() tea.Cmd {
	return func() tea.Msg {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("Cmd: Update check panicked: %v", r)
			}
		}()
		result, err := checkForUpdate()
		if err != nil || result == nil {
			if err != nil {
				log.Printf("Cmd: Update check error: %v", err)
			}
			return updateCheckMsg{}
		}
		return updateCheckMsg{
			latestVersion: result.LatestVersion,
			updateAvail:   result.UpdateAvail,
		}
	}
}

var tuiProgram *tea.Program

func performTUIUpdateCmd(ctx context.Context, keepBackup bool, allowPrerelease bool) tea.Cmd {
	return func() tea.Msg {
		newVersion, err := performSelfUpdateCore(UpdateOptions{
			Ctx:             ctx,
			KeepBackup:      keepBackup,
			AllowPrerelease: allowPrerelease,
			ProgressFn: func(p UpdateProgress) {
				if tuiProgram != nil {
					tuiProgram.Send(updateProgressMsg{step: p.Step, message: p.Message})
				}
			},
		})
		return updateCompleteMsg{newVersion: newVersion, err: err}
	}
}

func cacheFilePath() string {
	return filepath.Join(os.TempDir(), cacheFileName)
}

func loadCachedNetworks() []wifiAP {
	data, err := os.ReadFile(cacheFilePath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("Cache read failed: %v", err)
		}
		return nil
	}
	var cached []wifiAP
	if err := json.Unmarshal(data, &cached); err != nil {
		log.Printf("Cache decode failed: %v", err)
		return nil
	}
	return cached
}

func saveCachedNetworks(aps []wifiAP) {
	data, err := json.Marshal(aps)
	if err != nil {
		log.Printf("Cache encode failed: %v", err)
		return
	}
	if err := os.WriteFile(cacheFilePath(), data, 0600); err != nil {
		log.Printf("Cache write failed: %v", err)
	}
}

func cloneWifiAps(src []wifiAP) []wifiAP {
	dst := make([]wifiAP, len(src))
	for i := range src {
		dst[i] = src[i]
		if src[i].WifiAccessPoint != nil {
			mapCopy := make(gonetworkmanager.WifiAccessPoint, len(src[i].WifiAccessPoint))
			for k, v := range src[i].WifiAccessPoint {
				mapCopy[k] = v
			}
			dst[i].WifiAccessPoint = mapCopy
		}
	}
	return dst
}

func parseYesNo(value string) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "yes", "y", "true", "1", "on":
		return true, nil
	case "no", "n", "false", "0", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected yes/no")
	}
}

func normalizeSecurity(value string) string {
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" || v == "open" || v == "none" {
		return "open"
	}
	return "wpa-psk"
}

func (m *model) blurProfileInputs() {
	for i := range m.profileForm.inputs {
		m.profileForm.inputs[i].Blur()
	}
}

func (m *model) focusProfileInput(index int) {
	if len(m.profileForm.inputs) == 0 {
		return
	}
	if index < 0 {
		index = len(m.profileForm.inputs) - 1
	}
	if index >= len(m.profileForm.inputs) {
		index = 0
	}
	m.profileForm.focusIndex = index
	m.blurProfileInputs()
	m.profileForm.inputs[m.profileForm.focusIndex].Focus()
}

func (m *model) initProfileForm(mode profileFormMode, p gonetworkmanager.ConnectionProfile) {
	m.profileForm.mode = mode
	m.profileForm.statusMsg = ""
	m.profileForm.clearPassword = false
	m.profileForm.discardArmed = false
	m.profileForm.profileID = ""
	for i := range m.profileForm.inputs {
		m.profileForm.inputs[i].SetValue("")
	}
	m.profileForm.inputs[profileFieldSecurity].SetValue("wpa-psk")
	m.profileForm.inputs[profileFieldAutoconnect].SetValue("yes")
	m.profileForm.inputs[profileFieldHidden].SetValue("no")
	m.profileForm.inputs[profileFieldPriority].SetValue("")

	if p != nil {
		m.profileForm.profileID = p[gonetworkmanager.NmcliFieldConnectionUUID]
		name := p[gonetworkmanager.NmcliFieldConnectionName]
		ssid := gonetworkmanager.GetSSIDFromProfile(p)
		if ssid == "" {
			ssid = name
		}
		m.profileForm.inputs[profileFieldName].SetValue(name)
		m.profileForm.inputs[profileFieldSSID].SetValue(ssid)
		sec := normalizeSecurity(p[gonetworkmanager.NmcliFieldWifiSecurity])
		if km := strings.ToLower(strings.TrimSpace(p["802-11-wireless-security.key-mgmt"])); km == "wpa-psk" {
			sec = "wpa-psk"
		}
		m.profileForm.inputs[profileFieldSecurity].SetValue(sec)
		m.profileForm.inputs[profileFieldPassword].SetValue("")
		if ac, ok := p["connection.autoconnect"]; ok && strings.TrimSpace(ac) != "" {
			if b, err := parseYesNo(ac); err == nil {
				if b {
					m.profileForm.inputs[profileFieldAutoconnect].SetValue("yes")
				} else {
					m.profileForm.inputs[profileFieldAutoconnect].SetValue("no")
				}
			}
		}
		if h, ok := p["802-11-wireless.hidden"]; ok && strings.TrimSpace(h) != "" {
			if b, err := parseYesNo(h); err == nil {
				if b {
					m.profileForm.inputs[profileFieldHidden].SetValue("yes")
				} else {
					m.profileForm.inputs[profileFieldHidden].SetValue("no")
				}
			}
		}
		if pri, ok := p["connection.autoconnect-priority"]; ok {
			m.profileForm.inputs[profileFieldPriority].SetValue(strings.TrimSpace(pri))
		}
	}

	m.focusProfileInput(0)
	m.profileForm.initialValues = make([]string, len(m.profileForm.inputs))
	for i := range m.profileForm.inputs {
		m.profileForm.initialValues[i] = m.profileForm.inputs[i].Value()
	}
}

func (m *model) profileFormHasUnsavedChanges() bool {
	if m.profileForm.clearPassword {
		return true
	}
	if len(m.profileForm.initialValues) != len(m.profileForm.inputs) {
		return true
	}
	for i := range m.profileForm.inputs {
		if m.profileForm.inputs[i].Value() != m.profileForm.initialValues[i] {
			return true
		}
	}
	return false
}

func (m *model) validateProfileForm() (gonetworkmanager.WifiProfileSpec, bool, *int, error) {
	name := strings.TrimSpace(m.profileForm.inputs[profileFieldName].Value())
	ssid := strings.TrimSpace(m.profileForm.inputs[profileFieldSSID].Value())
	security := normalizeSecurity(m.profileForm.inputs[profileFieldSecurity].Value())
	password := m.profileForm.inputs[profileFieldPassword].Value()
	autoconnect, err := parseYesNo(m.profileForm.inputs[profileFieldAutoconnect].Value())
	if err != nil {
		return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("autoconnect must be yes/no")
	}
	hidden, err := parseYesNo(m.profileForm.inputs[profileFieldHidden].Value())
	if err != nil {
		return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("hidden must be yes/no")
	}
	if name == "" {
		return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("profile name is required")
	}
	if ssid == "" {
		return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("ssid is required")
	}
	passwordProvided := strings.TrimSpace(password) != ""
	if m.profileForm.mode == profileFormCreate && security == "wpa-psk" && !passwordProvided {
		return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("password is required for wpa-psk profiles")
	}
	if security == "wpa-psk" && passwordProvided {
		if len(password) < 8 || len(password) > 63 {
			return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("password must be 8-63 characters")
		}
	}

	var priorityPtr *int
	priorityRaw := strings.TrimSpace(m.profileForm.inputs[profileFieldPriority].Value())
	if priorityRaw != "" {
		priorityVal, err := strconv.Atoi(priorityRaw)
		if err != nil {
			return gonetworkmanager.WifiProfileSpec{}, false, nil, fmt.Errorf("priority must be an integer")
		}
		priorityPtr = &priorityVal
	}

	spec := gonetworkmanager.WifiProfileSpec{
		Name:        name,
		SSID:        ssid,
		Security:    security,
		Password:    password,
		Hidden:      hidden,
		Autoconnect: autoconnect,
		Priority:    priorityPtr,
	}
	return spec, passwordProvided, priorityPtr, nil
}

func fetchProfileByIDCmd(profileID string, forEdit bool) tea.Cmd {
	return func() tea.Msg {
		p, err := gonetworkmanager.GetConnectionProfileByID(profileID)
		return profileLoadedMsg{profile: p, err: err, forEdit: forEdit}
	}
}

func createProfileCmd(spec gonetworkmanager.WifiProfileSpec) tea.Cmd {
	return func() tea.Msg {
		_, err := gonetworkmanager.CreateWifiProfile(spec)
		return profileSaveResultMsg{success: err == nil, err: err, action: "created", profileRef: spec.Name}
	}
}

func updateProfileCmd(profileID string, spec gonetworkmanager.WifiProfileSpec, passwordProvided bool, clearPassword bool) tea.Cmd {
	return func() tea.Msg {
		_, err := gonetworkmanager.UpdateWifiProfile(profileID, spec, passwordProvided, clearPassword)
		return profileSaveResultMsg{success: err == nil, err: err, action: "updated", profileRef: spec.Name}
	}
}

func fetchWifiNetworksCmd(rescan bool) tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Fetching Wi-Fi networks (rescan: %t)...", rescan)
		apsRaw, err := gonetworkmanager.GetWifiList(rescan)
		var aps []wifiAP
		if err == nil {
			aps = make([]wifiAP, len(apsRaw))
			for i, r := range apsRaw {
				aps[i] = wifiAP{WifiAccessPoint: r}
			}
			log.Printf("Cmd: Fetched %d Wi-Fi networks.", len(apsRaw))
		} else {
			log.Printf("Cmd: Error fetching Wi-Fi list: %v", err)
		}
		return wifiListLoadedMsg{allAps: aps, err: err}
	}
}
func connectToWifiCmd(ssid, pw string, knownNoPsk bool) tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Connect to SSID: '%s', WasKnownNoPsk: %t", ssid, knownNoPsk)
		_, err := gonetworkmanager.ConnectToWifiRobustly(ssid, "*", ssid, pw, false)
		if err != nil {
			log.Printf("Cmd: Connect error for '%s': %v", ssid, err)
		} else {
			log.Printf("Cmd: Connect for '%s' appears successful.", ssid)
		}
		return connectionAttemptMsg{ssid: ssid, success: err == nil, err: err, WasKnownAttemptNoPsk: knownNoPsk}
	}
}
func getWifiStatusInternalCmd() tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Getting Wi-Fi status...")
		st, err := gonetworkmanager.GetWifiStatus()
		enabled := false
		if err == nil && st == "enabled" {
			enabled = true
		}
		if err != nil {
			log.Printf("Cmd: Error getting Wi-Fi status: %v", err)
		}
		return wifiStatusMsg{enabled: enabled, err: err}
	}
}
func toggleWifiCmd(enable bool) tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Toggling Wi-Fi to %t...", enable)
		var err error
		if enable {
			_, err = gonetworkmanager.WifiEnable()
		} else {
			_, err = gonetworkmanager.WifiDisable()
		}
		if err != nil {
			log.Printf("Cmd: Error toggling Wi-Fi: %v", err)
			return wifiStatusMsg{enabled: !enable, err: err}
		}
		return wifiStatusMsg{enabled: enable, err: nil}
	}
}
func fetchKnownNetworksCmd() tea.Cmd {
	return func() tea.Msg {
		log.Printf("Cmd: Fetching known networks...")
		profiles, err := gonetworkmanager.GetConnectionProfilesList(false)
		if err != nil {
			log.Printf("Cmd: Error fetching known profiles: %v", err)
			return knownNetworksMsg{err: err}
		}

		log.Printf("Cmd: Got %d total profiles", len(profiles))

		known := make(map[string]gonetworkmanager.ConnectionProfile)
		var activeConn *gonetworkmanager.ConnectionProfile
		var activeDev string

		activeDevProfiles, activeErr := gonetworkmanager.GetConnectionProfilesList(true)
		if activeErr != nil {
			log.Printf("Cmd: Error fetching active profiles: %v", activeErr)
		}
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

func fetchKnownWifiApsCmd() tea.Cmd {
	return func() tea.Msg {
		profiles, err := gonetworkmanager.GetConnectionProfilesList(false)
		if err != nil {
			return knownWifiApsListMsg{err: err}
		}

		var aps []wifiAP
		for _, p := range profiles {
			if p[gonetworkmanager.NmcliFieldConnectionType] == gonetworkmanager.ConnectionTypeWifi {
				aps = append(aps, connectionProfileToWifiAP(p))
			}
		}
		// Sort alphabetically
		sort.Slice(aps, func(i, j int) bool {
			return strings.ToLower(aps[i].getSSIDFromScannedAP()) < strings.ToLower(aps[j].getSSIDFromScannedAP())
		})
		return knownWifiApsListMsg{aps: aps, err: nil}
	}
}

func connectionProfileToWifiAP(p gonetworkmanager.ConnectionProfile) wifiAP {
	apMap := make(gonetworkmanager.WifiAccessPoint)
	ssid := gonetworkmanager.GetSSIDFromProfile(p)
	if ssid == "" {
		ssid = p[gonetworkmanager.NmcliFieldConnectionName]
	}
	apMap[gonetworkmanager.NmcliFieldWifiSSID] = ssid
	apMap[gonetworkmanager.NmcliFieldConnectionName] = p[gonetworkmanager.NmcliFieldConnectionName]
	apMap[gonetworkmanager.NmcliFieldConnectionUUID] = p[gonetworkmanager.NmcliFieldConnectionUUID]
	apMap[gonetworkmanager.NmcliFieldWifiSignal] = "0" // No signal for just a profile
	apMap[gonetworkmanager.NmcliFieldWifiSecurity] = "--"

	return wifiAP{
		WifiAccessPoint: apMap,
		IsKnown:         true,
		IsActive:        false, // Will be updated if needed, but for list view it's just a profile
	}
}
func fetchActiveConnInfoCmd(devName string) tea.Cmd { /* Same */
	return func() tea.Msg {
		if devName == "" {
			log.Printf("Cmd: fetchActiveConnInfo called with no device.")
			return activeConnInfoMsg{nil, fmt.Errorf("no active Wi-Fi device")}
		}
		log.Printf("Cmd: Fetching IP details for device: %s", devName)
		details, err := gonetworkmanager.GetDeviceInfoIPDetail(devName)
		if err != nil {
			log.Printf("Cmd: Error fetching IP details for %s: %v", devName, err)
		}
		return activeConnInfoMsg{details: details, err: err}
	}
}
func disconnectWifiCmd(profileID string) tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Attempting to disconnect profile: %s", profileID)
		_, err := gonetworkmanager.ConnectionDown(profileID)
		if err != nil {
			log.Printf("Cmd: Error disconnecting %s: %v", profileID, err)
		}
		return disconnectResultMsg{success: err == nil, err: err, ssid: profileID}
	}
}
func forgetNetworkCmd(profileID, ssidForMsg string) tea.Cmd { /* Same */
	return func() tea.Msg {
		log.Printf("Cmd: Attempting to forget profile ID: '%s' (SSID: '%s')", profileID, ssidForMsg)
		_, err := gonetworkmanager.ConnectionDelete(profileID)
		if err != nil {
			log.Printf("Cmd: Error forgetting profile '%s': %v", profileID, err)
		}
		return forgetNetworkResultMsg{ssid: ssidForMsg, success: err == nil, err: err}
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
			key := ssid + "|" + bssid
			deduplicatedAps[key] = ap
		} else {
			// For named networks, keep the one with the strongest signal
			if existing, ok := deduplicatedAps[ssid]; ok {
				existingSignal, _ := strconv.Atoi(existing.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
				newSignal, _ := strconv.Atoi(ap.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
				if newSignal > existingSignal {
					deduplicatedAps[ssid] = ap
					log.Printf("GetAllWifiItems: Keeping stronger signal for '%s': %d > %d", ssid, newSignal, existingSignal)
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
			// Create a wifiAP entry for this known profile
			apMap := make(gonetworkmanager.WifiAccessPoint)
			apMap[gonetworkmanager.NmcliFieldWifiSSID] = ssid
			apMap[gonetworkmanager.NmcliFieldConnectionName] = profile[gonetworkmanager.NmcliFieldConnectionName]
			apMap[gonetworkmanager.NmcliFieldConnectionUUID] = profile[gonetworkmanager.NmcliFieldConnectionUUID]
			apMap[gonetworkmanager.NmcliFieldWifiSignal] = "0" // No signal since not in range
			apMap[gonetworkmanager.NmcliFieldWifiSecurity] = "--"

			isActive := false
			if m.activeWifiConnection != nil && profile[gonetworkmanager.NmcliFieldConnectionUUID] == (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID] {
				isActive = true
			}

			knownAP := wifiAP{
				WifiAccessPoint: apMap,
				IsKnown:         true,
				IsActive:        isActive,
				Interface:       profile[gonetworkmanager.NmcliFieldConnectionDevice],
			}
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

	// Enrich scanned APs with known/active status
	enrichedAps := make([]list.Item, 0)
	foundActive := false

	for _, ap := range filteredAps {
		pAP := ap
		ssid := pAP.getSSIDFromScannedAP()
		pAP.IsKnown, pAP.IsActive = false, false

		if ssid != "" && ssid != "--" {
			if profile, ok := m.knownProfiles[ssid]; ok {
				pAP.IsKnown = true
				// Store profile info in the AP for later use (e.g., forgetting)
				pAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID] = profile[gonetworkmanager.NmcliFieldConnectionUUID]
				pAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName] = profile[gonetworkmanager.NmcliFieldConnectionName]

				if m.activeWifiConnection != nil && profile[gonetworkmanager.NmcliFieldConnectionUUID] == (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID] {
					pAP.IsActive = true
					pAP.Interface = profile[gonetworkmanager.NmcliFieldConnectionDevice]
					foundActive = true
				}
			}
		}
		enrichedAps = append(enrichedAps, pAP)
	}

	// Add known networks that aren't in scan range (only if they're active)
	for _, knownAP := range knownNetworksNotInScan {
		// Only show out-of-range known networks if they're currently active
		if knownAP.IsActive {
			enrichedAps = append(enrichedAps, knownAP)
			foundActive = true
		}
	}

	if !foundActive && m.activeWifiConnection != nil {
		activeSSID := gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection)
		log.Printf("GetAllWifiItems: WARNING - Active conn '%s' not found in enriched list!", activeSSID)
	}

	// Sort: Active first, then Known (in-range), then Known (out-of-range), then by signal
	sort.SliceStable(enrichedAps, func(i, j int) bool {
		itemI, itemJ := enrichedAps[i].(wifiAP), enrichedAps[j].(wifiAP)

		// Active networks always first
		if itemI.IsActive != itemJ.IsActive {
			return itemI.IsActive
		}

		// Then known networks
		if itemI.IsKnown != itemJ.IsKnown {
			return itemI.IsKnown
		}

		// Among known networks, show those in range (signal > 0) before those out of range
		sigi, _ := strconv.Atoi(itemI.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])
		sigj, _ := strconv.Atoi(itemJ.WifiAccessPoint[gonetworkmanager.NmcliFieldWifiSignal])

		if itemI.IsKnown && itemJ.IsKnown {
			inRangeI := sigi > 0
			inRangeJ := sigj > 0
			if inRangeI != inRangeJ {
				return inRangeI
			}
		}

		// Sort by signal strength
		if sigi != sigj {
			return sigi > sigj
		}

		// Finally sort by SSID alphabetically
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

func (m *model) clearStatus() {
	m.connectionStatusMsg = ""
}

func (m *model) setStatus(msg string, style lipgloss.Style) {
	m.connectionStatusMsg = style.Render(msg)
}

func (m *model) resizeComponents() {
	// Re-trigger window size calculation by sending the current size
	// This is a bit of a hack, but it ensures consistency with the Update loop
	// In a real refactor, we'd extract the layout logic.
	// For now, we'll just manually update the list sizes which is the most important part
	availableWidth := m.width - appStyle.GetHorizontalFrameSize()
	availableHeight := m.height - appStyle.GetVerticalFrameSize()

	headerHeight := lipgloss.Height(m.headerView(availableWidth))
	// We need a temporary keymap with current state for accurate footer height
	tempKeyMapState := m.keys
	tempKeyMapState.currentState = m.state
	footerHeight := lipgloss.Height(m.footerView(availableWidth, m.help.View(tempKeyMapState)))

	contentAreaHeight := availableHeight - headerHeight - footerHeight
	if contentAreaHeight < 0 {
		contentAreaHeight = 0
	}

	listContentHeight := contentAreaHeight
	if m.isFiltering {
		listContentHeight -= 4
		if listContentHeight < 5 {
			listContentHeight = 5
		}
	}

	listWidth := availableWidth
	if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
		calcW := availableWidth
		if networkListWidthPercent > 0 {
			calcW = int(float64(availableWidth) * networkListWidthPercent)
		}
		if networkListFixedWidth > 0 && calcW > networkListFixedWidth {
			calcW = networkListFixedWidth
		}
		if calcW < 40 {
			calcW = 40
		}
		listWidth = calcW
	}

	m.listDisplayWidth = listWidth
	m.wifiList.SetSize(m.listDisplayWidth, listContentHeight)
	m.knownWifiList.SetSize(m.listDisplayWidth, listContentHeight)
	m.activeConnInfoViewport.Width = availableWidth - infoBoxStyle.GetHorizontalFrameSize()
	m.activeConnInfoViewport.Height = contentAreaHeight - infoBoxStyle.GetVerticalFrameSize()
}

func (m *model) handleKnownNetworksListKeys(msg tea.KeyMsg) []tea.Cmd {
	var cmds []tea.Cmd
	if m.isLoading {
		switch {
		case key.Matches(msg, m.keys.Back) || msg.String() == "h":
			m.state = viewNetworksList
			m.clearStatus()
			m.resizeComponents()
			return nil
		case key.Matches(msg, m.keys.Quit):
			return []tea.Cmd{tea.Quit}
		default:
			return nil
		}
	}
	switch {
	case key.Matches(msg, m.keys.Back) || msg.String() == "h":
		m.state = viewNetworksList
		m.clearStatus()
		m.resizeComponents()
		return nil
	case key.Matches(msg, m.keys.Connect) || msg.String() == "l":
		if i, ok := m.knownWifiList.SelectedItem().(wifiAP); ok {
			profileID := i.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]
			if profileID == "" {
				m.connectionStatusMsg = errorStyle.Render("Selected profile has no UUID.")
				return nil
			}
			m.selectedAP = i
			m.profileDetailsID = profileID
			m.state = viewProfileDetails
			m.isLoading = true
			m.activeConnInfoViewport.SetContent("Loading profile details...")
			return []tea.Cmd{fetchProfileByIDCmd(profileID, false), m.spinner.Tick}
		}
		return nil
	case key.Matches(msg, m.keys.NewProfile):
		m.initProfileForm(profileFormCreate, nil)
		m.state = viewProfileCreate
		m.clearStatus()
		return []tea.Cmd{textinput.Blink}
	case key.Matches(msg, m.keys.EditProfile):
		if i, ok := m.knownWifiList.SelectedItem().(wifiAP); ok {
			profileID := i.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]
			if profileID == "" {
				m.connectionStatusMsg = errorStyle.Render("Selected profile has no UUID.")
				return nil
			}
			m.selectedAP = i
			m.profileDetailsID = profileID
			m.isLoading = true
			m.clearStatus()
			return []tea.Cmd{fetchProfileByIDCmd(profileID, true), m.spinner.Tick}
		}
		m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("No profile selected.")
		return nil
	case key.Matches(msg, m.keys.Refresh):
		m.isLoading = true
		m.knownWifiList.Title = "Loading Profiles..."
		m.clearStatus()
		return []tea.Cmd{fetchKnownWifiApsCmd(), m.spinner.Tick}
	case key.Matches(msg, m.keys.Quit):
		return []tea.Cmd{tea.Quit}
	case key.Matches(msg, m.keys.Forget):
		if i, ok := m.knownWifiList.SelectedItem().(wifiAP); ok {
			m.selectedAP = i
			m.previousState = m.state
			m.state = viewConfirmForget
			m.clearStatus()
		}
		return nil
	}
	var cmd tea.Cmd
	m.knownWifiList, cmd = m.knownWifiList.Update(msg)
	cmds = append(cmds, cmd)
	return cmds
}

func (m model) isTextInputActive() bool {
	return m.state == viewPasswordInput || m.state == viewProfileCreate || m.state == viewProfileEdit || (m.state == viewNetworksList && m.isFiltering)
}

// remapVimKeys converts j/k to down/up arrow keys when not in a text input context.
func (m model) remapVimKeys(msg tea.KeyMsg) tea.KeyMsg {
	if m.isTextInputActive() {
		return msg
	}
	switch msg.String() {
	case "j":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "k":
		return tea.KeyMsg{Type: tea.KeyUp}
	}
	return msg
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var cmd tea.Cmd
	m.keys.currentState = m.state

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		appStyleHorizontalFrame := appStyle.GetHorizontalFrameSize()
		appStyleVerticalFrame := appStyle.GetVerticalFrameSize()
		availableWidth := m.width - appStyleHorizontalFrame
		availableHeight := m.height - appStyleVerticalFrame
		desiredHelpWidth := int(float64(availableWidth) * helpBarWidthPercent)
		if desiredHelpWidth > helpBarMaxWidth {
			desiredHelpWidth = helpBarMaxWidth
		}
		if desiredHelpWidth < 20 {
			desiredHelpWidth = 20
		}
		m.help.Width = desiredHelpWidth
		headerHeight := lipgloss.Height(m.headerView(availableWidth))
		tempKeyMapState := m.keys
		tempKeyMapState.currentState = m.state // Use current state for accurate footer height
		footerHeight := lipgloss.Height(m.footerView(availableWidth, m.help.View(tempKeyMapState)))
		contentAreaHeight := availableHeight - headerHeight - footerHeight
		if contentAreaHeight < 0 {
			contentAreaHeight = 0
		}

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
			calcW := availableWidth
			if networkListWidthPercent > 0 {
				calcW = int(float64(availableWidth) * networkListWidthPercent)
			}
			if networkListFixedWidth > 0 && calcW > networkListFixedWidth {
				calcW = networkListFixedWidth
			}
			if calcW < 40 {
				calcW = 40
			}
			listWidth = calcW
		}
		m.listDisplayWidth = listWidth // Store calculated list width
		m.wifiList.SetSize(m.listDisplayWidth, listContentHeight)
		m.knownWifiList.SetSize(m.listDisplayWidth, listContentHeight)
		m.activeConnInfoViewport.Width = availableWidth - infoBoxStyle.GetHorizontalFrameSize()
		m.activeConnInfoViewport.Height = contentAreaHeight - infoBoxStyle.GetVerticalFrameSize()
		if m.activeConnInfoViewport.Height < 0 {
			m.activeConnInfoViewport.Height = 0
		}
		pwInputContentWidth := availableWidth * 2 / 3
		if pwInputContentWidth > 60 {
			pwInputContentWidth = 60
		}
		if pwInputContentWidth < 40 {
			pwInputContentWidth = 40
		}
		m.passwordInput.Width = pwInputContentWidth - lipgloss.Width(m.passwordInput.Prompt) - passwordInputContainerStyle.GetHorizontalFrameSize()
		profileInputWidth := availableWidth - 24
		if profileInputWidth < 20 {
			profileInputWidth = 20
		}
		for i := range m.profileForm.inputs {
			m.profileForm.inputs[i].Width = profileInputWidth
		}

	case spinner.TickMsg:
		if m.isLoading || m.isUpdating {
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
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
		if msg.err != nil {
			if m.state == viewNetworksList {
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error fetching known profiles: %v", msg.err))
			}
			break
		}
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
			m.isLoading = false
			m.isScanning = false
			m.allScannedAps = msg.allAps
			if len(msg.allAps) > 0 {
				m.processAndSetWifiList(m.allScannedAps)
				saveCachedNetworks(cloneWifiAps(msg.allAps))
			} else {
				log.Printf("Scan returned 0 results")
				m.processAndSetWifiList([]wifiAP{})
				if m.state == viewNetworksList {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("No networks found.")
				}
			}
		}
	case connectionAttemptMsg:
		m.isLoading = false
		if msg.success {
			m.state = viewConnectionResult
			m.lastConnectionWasSuccessful = true
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Connected to %s!", m.selectedAP.StyledTitle()))
		} else {
			if msg.WasKnownAttemptNoPsk && m.selectedAP.getSSIDFromScannedAP() == msg.ssid {
				log.Printf("Known net '%s' connect failed. Prompting for PSK.", msg.ssid)
				m.state = viewPasswordInput
				m.passwordInput.SetValue("")
				m.passwordInput.Focus()
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Stored creds for %s failed. Enter password:", m.selectedAP.StyledTitle()))
				cmds = append(cmds, textinput.Blink)
				return m, tea.Batch(cmds...)
			} else {
				m.state = viewConnectionResult
				m.lastConnectionWasSuccessful = false
				errTxt := "Unknown error."
				if msg.err != nil {
					errTxt = msg.err.Error()
				}
				m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Failed to connect to %s: %s", m.selectedAP.StyledTitle(), errTxt))
			}
		}
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(false)) // Refresh state after attempt
	case activeConnInfoMsg: /* Same */
		m.isLoading = false
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
	case disconnectResultMsg: /* Same */
		m.isLoading = false
		if msg.success {
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Disconnected from %s.", msg.ssid))
			m.activeWifiConnection = nil
			m.activeWifiDevice = ""
		} else {
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error disconnecting from %s: %v", msg.ssid, msg.err))
		}
		m.state = viewNetworksList
		cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
	case forgetNetworkResultMsg:
		m.isLoading = false
		if msg.success {
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Network profile for %s forgotten.", msg.ssid))
			delete(m.knownProfiles, msg.ssid)
		} else {
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error forgetting profile for %s: %v", msg.ssid, msg.err))
		}

		if m.previousState == viewKnownNetworksList {
			m.state = viewKnownNetworksList
			cmds = append(cmds, fetchKnownNetworksCmd(), fetchKnownWifiApsCmd())
		} else {
			m.state = viewNetworksList
			cmds = append(cmds, fetchKnownNetworksCmd(), fetchWifiNetworksCmd(true))
		}
		m.previousState = viewNetworksList

	case knownWifiApsListMsg:
		m.isLoading = false
		if msg.err != nil {
			m.knownWifiList.Title = "Error fetching profiles"
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error fetching profiles: %v", msg.err))
		} else {
			items := make([]list.Item, len(msg.aps))
			for i, ap := range msg.aps {
				items[i] = ap
			}
			m.knownWifiList.SetItems(items)
			m.knownWifiList.Title = fmt.Sprintf("Known Wi-Fi Profiles (%d)", len(msg.aps))
			m.clearStatus()
		}

	case profileLoadedMsg:
		m.isLoading = false
		if msg.err != nil {
			m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Error loading profile: %v", msg.err))
			m.state = viewKnownNetworksList
			break
		}
		if msg.profile == nil {
			m.connectionStatusMsg = errorStyle.Render("Profile no longer exists.")
			m.state = viewKnownNetworksList
			break
		}

		if msg.forEdit {
			m.initProfileForm(profileFormEdit, msg.profile)
			m.state = viewProfileEdit
			cmds = append(cmds, textinput.Blink)
		} else {
			name := msg.profile[gonetworkmanager.NmcliFieldConnectionName]
			uuid := msg.profile[gonetworkmanager.NmcliFieldConnectionUUID]
			ssid := gonetworkmanager.GetSSIDFromProfile(msg.profile)
			if ssid == "" {
				ssid = name
			}
			security := msg.profile["802-11-wireless-security.key-mgmt"]
			if security == "" {
				security = "open"
			}
			autoconnect := msg.profile["connection.autoconnect"]
			if autoconnect == "" {
				autoconnect = "yes"
			}
			hidden := msg.profile["802-11-wireless.hidden"]
			if hidden == "" {
				hidden = "no"
			}
			priority := msg.profile["connection.autoconnect-priority"]
			if strings.TrimSpace(priority) == "" {
				priority = "(default)"
			}
			details := []string{
				fmt.Sprintf("Name: %s", name),
				fmt.Sprintf("UUID: %s", uuid),
				fmt.Sprintf("SSID: %s", ssid),
				fmt.Sprintf("Security: %s", security),
				fmt.Sprintf("Autoconnect: %s", autoconnect),
				fmt.Sprintf("Hidden: %s", hidden),
				fmt.Sprintf("Priority: %s", priority),
			}
			m.activeConnInfoViewport.SetContent(strings.Join(details, "\n"))
			m.activeConnInfoViewport.GotoTop()
			m.state = viewProfileDetails
		}

	case profileSaveResultMsg:
		m.isLoading = false
		if msg.success {
			m.state = viewKnownNetworksList
			m.connectionStatusMsg = successStyle.Render(fmt.Sprintf("Profile %s %s.", msg.profileRef, msg.action))
			cmds = append(cmds, fetchKnownNetworksCmd(), fetchKnownWifiApsCmd())
		} else {
			m.profileForm.statusMsg = errorStyle.Render(fmt.Sprintf("Failed to save profile: %v", msg.err))
			if m.profileForm.mode == profileFormCreate {
				m.state = viewProfileCreate
			} else {
				m.state = viewProfileEdit
			}
			cmds = append(cmds, textinput.Blink)
		}

	case updateCheckMsg:
		if msg.updateAvail && msg.latestVersion != "" {
			m.updateAvailable = true
			m.updateLatestVersion = msg.latestVersion
		}

	case updateProgressMsg:
		m.updateStatusMsg = msg.message

	case updateCompleteMsg:
		m.isUpdating = false
		m.isLoading = false
		m.updateCancelFn = nil
		if msg.err != nil {
			if errors.Is(msg.err, context.Canceled) {
				m.updateError = nil
				m.updateStatusMsg = "Update cancelled."
			} else {
				m.updateError = msg.err
				m.updateStatusMsg = ""
			}
		} else {
			if msg.newVersion != "" {
				m.updateNewVersion = msg.newVersion
				m.updateAvailable = false
			} else {
				// Already up to date
				m.updateStatusMsg = "Already up to date."
			}
			m.updateError = nil
		}

	case tea.KeyMsg:
		// Remap vim j/k to arrow keys when not typing in a text field
		msg = m.remapVimKeys(msg)

		if key.Matches(msg, m.keys.Quit) && !(msg.String() == "q" && m.isTextInputActive()) {
			if m.updateCancelFn != nil {
				m.updateCancelFn()
				m.updateCancelFn = nil
			}
			return m, tea.Quit
		}
		if key.Matches(msg, m.keys.Help) {
			if !m.isTextInputActive() {
				m.help.ShowAll = !m.help.ShowAll
				if m.state == viewNetworksList || m.state == viewActiveConnectionInfo || m.state == viewProfileDetails {
					avW := m.width - appStyle.GetHorizontalFrameSize()
					hH := lipgloss.Height(m.headerView(avW))
					tk := m.keys
					tk.currentState = m.state
					fH := lipgloss.Height(m.footerView(avW, m.help.View(tk)))
					appCH := m.height - appStyle.GetVerticalFrameSize()
					nCAH := appCH - hH - fH
					if nCAH < 0 {
						nCAH = 0
					}
					switch m.state {
					case viewNetworksList:
						m.wifiList.SetSize(m.listDisplayWidth, nCAH)
					case viewActiveConnectionInfo, viewProfileDetails:
						m.activeConnInfoViewport.Height = nCAH - infoBoxStyle.GetVerticalFrameSize()
						if m.activeConnInfoViewport.Height < 0 {
							m.activeConnInfoViewport.Height = 0
						}
					}
					log.Printf("Help toggled, content height for %v set to: %d", m.state, nCAH)
				}
			}
		}

		// Shift+U: trigger in-TUI update
		if msg.String() == "U" && m.updateAvailable && !m.isTextInputActive() && !m.isUpdating {
			if m.state != viewConnecting && m.state != viewUpdating {
				ctx, cancel := context.WithCancel(context.Background())
				m.updateCancelFn = cancel
				m.previousState = m.state
				m.state = viewUpdating
				m.isUpdating = true
				m.isLoading = true
				m.updateStatusMsg = "Starting update..."
				m.updateError = nil
				m.updateNewVersion = ""
				return m, tea.Batch(
					performTUIUpdateCmd(ctx, getKeepBackupConfig(), m.allowPrerelease),
					m.spinner.Tick,
				)
			}
		}

		switch m.state {
		case viewKnownNetworksList:
			cmds = append(cmds, m.handleKnownNetworksListKeys(msg)...)
		case viewProfileDetails:
			switch {
			case key.Matches(msg, m.keys.Back) || msg.String() == "h":
				m.state = viewKnownNetworksList
				m.clearStatus()
			case key.Matches(msg, m.keys.EditProfile):
				if m.profileDetailsID == "" {
					m.connectionStatusMsg = errorStyle.Render("Cannot edit profile without UUID.")
					break
				}
				m.isLoading = true
				cmds = append(cmds, fetchProfileByIDCmd(m.profileDetailsID, true), m.spinner.Tick)
			case key.Matches(msg, m.keys.Forget):
				if m.selectedAP.WifiAccessPoint != nil {
					m.previousState = viewKnownNetworksList
					m.state = viewConfirmForget
				}
			default:
				m.activeConnInfoViewport, cmd = m.activeConnInfoViewport.Update(msg)
				cmds = append(cmds, cmd)
			}
		case viewProfileCreate, viewProfileEdit:
			switch {
			case key.Matches(msg, m.keys.Back):
				if m.profileFormHasUnsavedChanges() && !m.profileForm.discardArmed {
					m.profileForm.discardArmed = true
					m.profileForm.statusMsg = toggleHiddenStatusMsgStyle.Render("Unsaved changes. Press Esc again to discard.")
					break
				}
				m.blurProfileInputs()
				m.state = viewKnownNetworksList
				m.profileForm.statusMsg = ""
				m.profileForm.discardArmed = false
			case key.Matches(msg, m.keys.ClearSecret):
				m.profileForm.inputs[profileFieldPassword].SetValue("")
				m.profileForm.clearPassword = true
				m.profileForm.discardArmed = false
				m.profileForm.statusMsg = toggleHiddenStatusMsgStyle.Render("Password will be cleared on save.")
			case key.Matches(msg, m.keys.Connect):
				spec, passwordProvided, _, err := m.validateProfileForm()
				if err != nil {
					m.profileForm.discardArmed = false
					m.profileForm.statusMsg = errorStyle.Render(err.Error())
					break
				}
				m.profileForm.discardArmed = false
				m.profileForm.statusMsg = ""
				m.isLoading = true
				if m.state == viewProfileCreate {
					cmds = append(cmds, createProfileCmd(spec), m.spinner.Tick)
				} else {
					cmds = append(cmds, updateProfileCmd(m.profileForm.profileID, spec, passwordProvided, m.profileForm.clearPassword), m.spinner.Tick)
				}
			case msg.String() == "tab" || msg.String() == "down":
				m.profileForm.discardArmed = false
				m.focusProfileInput(m.profileForm.focusIndex + 1)
			case msg.String() == "shift+tab" || msg.String() == "up":
				m.profileForm.discardArmed = false
				m.focusProfileInput(m.profileForm.focusIndex - 1)
			default:
				if m.profileForm.focusIndex >= 0 && m.profileForm.focusIndex < len(m.profileForm.inputs) {
					m.profileForm.inputs[m.profileForm.focusIndex], cmd = m.profileForm.inputs[m.profileForm.focusIndex].Update(msg)
					cmds = append(cmds, cmd)
					m.profileForm.discardArmed = false
					if m.profileForm.focusIndex == profileFieldPassword {
						if strings.TrimSpace(m.profileForm.inputs[profileFieldPassword].Value()) != "" {
							m.profileForm.clearPassword = false
						}
					}
				}
			}
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

			// Check for Filter key binding even when loading
			if key.Matches(msg, m.keys.Filter) {
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
				return m, tea.Batch(cmds...)
			}

			// Allow opening known profiles even while scanning/loading.
			if key.Matches(msg, m.keys.Profiles) {
				m.state = viewKnownNetworksList
				m.isLoading = true
				m.knownWifiList.Title = "Loading Profiles..."
				m.clearStatus()
				m.resizeComponents()
				cmds = append(cmds, fetchKnownWifiApsCmd(), m.spinner.Tick)
				return m, tea.Batch(cmds...)
			}

			// Not filtering - handle normal keys
			if m.isLoading {
				// While loading, still allow list navigation (but Filter key is handled above)
				m.wifiList, cmd = m.wifiList.Update(msg)
				cmds = append(cmds, cmd)
				break
			}

			// Handle custom key bindings
			switch {
			case key.Matches(msg, m.keys.Back) || msg.String() == "esc" || msg.String() == "h":
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
				if m.showHiddenNetworks {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Showing unnamed.")
				} else {
					m.connectionStatusMsg = toggleHiddenStatusMsgStyle.Render("Hiding unnamed.")
				}

			case key.Matches(msg, m.keys.Filter):
				// This should not be reached since Filter is handled above, but keeping for completeness
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
				if !m.wifiEnabled {
					act = "ON"
				}
				m.connectionStatusMsg = fmt.Sprintf("Toggling Wi-Fi %s...", act)
				cmds = append(cmds, toggleWifiCmd(!m.wifiEnabled), m.spinner.Tick)

			case key.Matches(msg, m.keys.Disconnect):
				if m.activeWifiConnection != nil {
					// Try to find the active network in the current list first
					foundActive := false
					if items := m.wifiList.Items(); len(items) > 0 {
						for _, item := range items {
							if ap, ok := item.(wifiAP); ok && ap.IsActive {
								m.selectedAP = ap
								foundActive = true
								break
							}
						}
					}

					// If not found in list, create a minimal wifiAP from the profile
					if !foundActive {
						sAP := gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection)
						td := make(gonetworkmanager.WifiAccessPoint)
						td[gonetworkmanager.NmcliFieldWifiSSID] = sAP
						m.selectedAP = wifiAP{WifiAccessPoint: td, IsActive: true, IsKnown: true, Interface: m.activeWifiDevice}
					}

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

			case key.Matches(msg, m.keys.Profiles):
				m.state = viewKnownNetworksList
				m.isLoading = true
				m.knownWifiList.Title = "Loading Profiles..."
				m.clearStatus()
				m.resizeComponents()
				cmds = append(cmds, fetchKnownWifiApsCmd(), m.spinner.Tick)

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
			case key.Matches(msg, m.keys.Connect) || msg.String() == "l":
				if item, ok := m.wifiList.SelectedItem().(wifiAP); ok {
					m.selectedAP = item
					ssid := item.getSSIDFromScannedAP()
					if ssid == "" || ssid == "--" {
						m.connectionStatusMsg = errorStyle.Render("Cannot connect to hidden SSID from scan list.")
						break
					}
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
			case key.Matches(msg, m.keys.Connect):
				m.isLoading = true
				m.state = viewConnecting
				m.connectionStatusMsg = fmt.Sprintf("Connecting to %s...", m.selectedAP.StyledTitle())
				cmds = append(cmds, connectToWifiCmd(m.selectedAP.getSSIDFromScannedAP(), m.passwordInput.Value(), false), m.spinner.Tick)
				passthrough = false
			case key.Matches(msg, m.keys.Back):
				m.state = viewNetworksList
				m.passwordInput.Blur()
				m.connectionStatusMsg = ""
				passthrough = false
			}
			if passthrough {
				m.passwordInput, cmd = m.passwordInput.Update(msg)
				cmds = append(cmds, cmd)
			}
		case viewConnectionResult:
			if key.Matches(msg, m.keys.Connect) || key.Matches(msg, m.keys.Back) {
				m.state = viewNetworksList
				m.connectionStatusMsg = ""
			}
		case viewActiveConnectionInfo:
			if key.Matches(msg, m.keys.Back) || msg.String() == "h" {
				m.state = viewNetworksList
				m.connectionStatusMsg = ""
			} else {
				m.activeConnInfoViewport, cmd = m.activeConnInfoViewport.Update(msg)
				cmds = append(cmds, cmd)
			}
		case viewConfirmDisconnect: /* Same */
			switch {
			case key.Matches(msg, m.keys.Connect):
				m.isLoading = true
				ssidD := m.selectedAP.StyledTitle()
				m.connectionStatusMsg = fmt.Sprintf("Disconnecting from %s...", ssidD)
				pID := ""
				if m.activeWifiConnection != nil {
					pID = (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionUUID]
					if pID == "" {
						pID = (*m.activeWifiConnection)[gonetworkmanager.NmcliFieldConnectionName]
					}
					if pID == "" {
						pID = gonetworkmanager.GetSSIDFromProfile(*m.activeWifiConnection)
					}
				} else if m.selectedAP.IsActive {
					log.Printf("Warning: Disconnecting via selectedAP.")
					pID = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionUUID]
					if pID == "" {
						pID = m.selectedAP.WifiAccessPoint[gonetworkmanager.NmcliFieldConnectionName]
					}
					if pID == "" {
						pID = m.selectedAP.getSSIDFromScannedAP()
					}
				}
				if pID == "" {
					m.connectionStatusMsg = errorStyle.Render("Cannot ID profile to disconnect.")
					m.isLoading = false
					m.state = viewNetworksList
					break
				}
				cmds = append(cmds, disconnectWifiCmd(pID), m.spinner.Tick)
			case key.Matches(msg, m.keys.Back):
				m.state = viewNetworksList
				m.connectionStatusMsg = ""
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
					m.connectionStatusMsg = errorStyle.Render(fmt.Sprintf("Cannot identify profile UUID to forget for %s.", ssidForMsg))
					m.isLoading = false
					m.state = m.previousState
					break
				}

				m.connectionStatusMsg = fmt.Sprintf("Forgetting profile for %s...", ssidForMsg)
				cmds = append(cmds, forgetNetworkCmd(pID, ssidForMsg), m.spinner.Tick)

			case key.Matches(msg, m.keys.Back):
				m.state = m.previousState
				m.connectionStatusMsg = ""
			}
		case viewUpdating:
			switch {
			case key.Matches(msg, m.keys.Back):
				if m.isUpdating {
					if m.updateCancelFn != nil {
						m.updateCancelFn()
						m.updateCancelFn = nil
					}
				} else {
					m.state = m.previousState
					m.updateError = nil
					m.updateStatusMsg = ""
					m.updateCancelFn = nil
				}
			case key.Matches(msg, m.keys.Connect):
				if !m.isUpdating && m.updateNewVersion != "" {
					m.wantsRestart = true
					return m, tea.Quit
				}
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
	m.keys.Update.SetEnabled(m.updateAvailable && !m.isUpdating)
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
		if m.connectionStatusMsg != "" {
			statusR := m.connectionStatusMsg
			if !strings.HasPrefix(m.connectionStatusMsg, "\x1b[") {
				style := statusMessageBaseStyle
				if strings.Contains(strings.ToLower(m.connectionStatusMsg), "unnamed") {
					style = toggleHiddenStatusMsgStyle
				} else if strings.Contains(m.connectionStatusMsg, "Wi-Fi is") {
					style = style.Foreground(ansTextColor)
				} else {
					style = style.Faint(true)
				}
				statusR = style.Render(m.connectionStatusMsg)
			}
			if !m.isLoading || m.isFiltering {
				if lipgloss.Height(currMainS)+lipgloss.Height(statusR) <= cdh {
					currMainS = lipgloss.JoinVertical(lipgloss.Top, currMainS, statusR)
				} else {
					log.Printf("Warn: No vspace for list+status. Status: %s", m.connectionStatusMsg)
				}
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
	case viewProfileDetails:
		currMainS = m.activeConnInfoViewport.View()
	case viewConfirmDisconnect:
		currMainS = lipgloss.JoinVertical(lipgloss.Center, fmt.Sprintf("Disconnect from %s ?", m.selectedAP.StyledTitle()), "\n", lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter to confirm, Esc to cancel)"))
	case viewConfirmForget:
		currMainS = lipgloss.JoinVertical(lipgloss.Center, fmt.Sprintf("Forget profile for\n%s ?", m.selectedAP.StyledTitle()), "\n", lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter to confirm, Esc to cancel)"))
	case viewUpdating:
		msgW := avW * 3 / 4
		if msgW > 80 {
			msgW = 80
		}
		if msgW < 40 {
			msgW = 40
		}
		wrapStyle := lipgloss.NewStyle().Width(msgW).Align(lipgloss.Center)
		if m.updateError != nil {
			errMsg := wrapStyle.Render(errorStyle.Render(fmt.Sprintf("Update failed: %v", m.updateError)))
			hint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Esc to go back)")
			currMainS = lipgloss.JoinVertical(lipgloss.Center, "", errMsg, "", hint)
		} else if m.updateNewVersion != "" {
			successMsg := successStyle.Render(fmt.Sprintf("Updated to %s!", m.updateNewVersion))
			hint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Enter to restart, Esc to continue)")
			currMainS = lipgloss.JoinVertical(lipgloss.Center, "", successMsg, "", hint)
		} else if m.isUpdating {
			progress := connectingStyle.Render(fmt.Sprintf("%s %s", m.spinner.View(), m.updateStatusMsg))
			hint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Esc to cancel)")
			currMainS = lipgloss.JoinVertical(lipgloss.Center, "", progress, "", hint)
		} else {
			statusMsg := toggleHiddenStatusMsgStyle.Render(m.updateStatusMsg)
			hint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("(Esc to go back)")
			currMainS = lipgloss.JoinVertical(lipgloss.Center, "", statusMsg, "", hint)
		}
	case viewKnownNetworksList:
		listR := m.knownWifiList.View()
		if networkListWidthPercent > 0 || networkListFixedWidth > 0 {
			currMainS = lipgloss.PlaceHorizontal(avW, lipgloss.Center, listR)
		} else {
			currMainS = listR
		}
	case viewProfileCreate, viewProfileEdit:
		title := "Create Wi-Fi Profile"
		if m.state == viewProfileEdit {
			title = "Edit Wi-Fi Profile"
		}
		var lines []string
		lines = append(lines, titleStyle.Render(title))
		for i := range m.profileForm.inputs {
			fieldLine := m.profileForm.inputs[i].View()
			if i != m.profileForm.focusIndex {
				v := m.profileForm.inputs[i].Value()
				if i == profileFieldPassword {
					if v != "" {
						v = strings.Repeat("*", len(v))
					} else if m.state == viewProfileEdit {
						v = "(unchanged)"
					}
				}
				fieldLine = v
			}
			prefix := "  "
			if i == m.profileForm.focusIndex {
				prefix = "▸ "
			}
			lines = append(lines, fmt.Sprintf("%s%s: %s", prefix, profileFieldLabels[i], fieldLine))
		}
		hint := lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("Tab/Up/Down: move  Enter: save  Esc: cancel  Ctrl+X: clear password")
		lines = append(lines, "", hint)
		if m.profileForm.statusMsg != "" {
			lines = append(lines, "", m.profileForm.statusMsg)
		}
		currMainS = infoBoxStyle.Render(strings.Join(lines, "\n"))
	}
	if m.state != viewNetworksList && m.state != viewActiveConnectionInfo && m.state != viewProfileDetails {
		currMainS = lipgloss.Place(avW, cdh, lipgloss.Center, lipgloss.Center, currMainS)
	}
	mainSb.WriteString(currMainS)
	return appStyle.Render(lipgloss.JoinVertical(lipgloss.Top, hView, mainSb.String(), fView))
}
func (m model) headerView(w int) string {
	t := titleStyle.Render(effectiveAppName())

	// Scanning indicator
	scanIndicator := ""
	if m.isScanning {
		scanIndicator = connectingStyle.Render(" " + m.spinner.View() + " Scanning...")
	}

	s := "Wi-Fi: "
	if m.wifiEnabled {
		s += wifiStatusStyleEnabled.Render("Enabled 󰄬")
	} else {
		s += wifiStatusStyleDisabled.Render("Disabled ✘")
	}

	// Update hint (dim, non-intrusive)
	updateHint := ""
	if m.updateAvailable {
		updateHint = " " + lipgloss.NewStyle().Foreground(ansFaintTextColor).Render("("+m.updateLatestVersion+" avail, U to update)")
	}

	// Calculate spacing
	fixedWidth := lipgloss.Width(t) + lipgloss.Width(s) + lipgloss.Width(updateHint)
	scanWidth := lipgloss.Width(scanIndicator)
	totalWidth := fixedWidth + scanWidth

	if totalWidth >= w {
		// Not enough space — drop update hint, just show title and status
		sp := w - lipgloss.Width(t) - lipgloss.Width(s)
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

	return lipgloss.JoinHorizontal(lipgloss.Left, t, strings.Repeat(" ", leftSpace), scanIndicator, strings.Repeat(" ", rightSpace), s, updateHint)
}
func (m model) footerView(w int, h string) string { /* Same */
	return lipgloss.PlaceHorizontal(w, lipgloss.Center, helpGlobalStyle.Render(h))
}

func effectiveAppName() string {
	if strings.TrimSpace(AppName) != "" {
		return AppName
	}
	return appName
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `%s

Overview:
  A terminal UI for managing NetworkManager Wi-Fi connections on Linux.
  It uses nmcli under the hood and provides a keyboard-driven interface.

Usage:
  nmtui-go
  nmtui-go [--help] [--version]
  nmtui-go [--update] [--update-prerelease] [--no-backup]
  nmtui-go [--check-update]

Options:
  -h, --help            Show this help and exit
  -v, --version         Show version/build metadata and exit
  --update              Self-update to the latest GitHub release
  --check-update        Check if a newer version is available

  Modifiers for --update:
  --update-prerelease   Include pre-release versions when updating
  --no-backup           Don't keep backup of old binary after update

Features:
  - Scan for Wi-Fi networks (rescan on demand)
  - Connect to open and WPA/WPA2 PSK networks
  - Reuse existing NetworkManager profiles when available
  - Unified list with active and known indicators
  - Toggle Wi-Fi radio on/off
  - Show active connection details (IP, gateway, DNS, etc.)
  - Manage known Wi-Fi profiles (view/details/create/edit/forget)
  - Disconnect active Wi-Fi connection
  - Filter network list by SSID

Runtime keybindings (inside TUI):
  Arrow Up/Down   Navigate list
  Enter           Select/connect/confirm
  Esc             Back/cancel
  r               Refresh scan
  /               Start filter input
  u               Toggle unnamed/hidden networks
  t               Toggle Wi-Fi radio
  d               Disconnect active Wi-Fi
  i               Active connection info
  p               Known profiles view
  n               New profile (in profiles view)
  e               Edit selected profile
  Ctrl+f          Forget selected known profile
  Ctrl+x          Clear password in profile form
  U (Shift+U)     Update to latest version (when update available)
  ?               Toggle extended in-app help
  q / Ctrl+c      Quit (plain q is ignored while typing in text inputs)

Requirements:
  - Linux
  - NetworkManager service installed and running
  - nmcli available in PATH

Troubleshooting:
  - "nmcli: command not found": install/start NetworkManager and ensure nmcli is in PATH.
  - Wrong interface/profile mismatch: remove stale profile with:
      nmcli con delete "<Profile Name or UUID>"
  - Wi-Fi won't toggle: check hardware switch/rfkill blocks.
  - Authentication failures: verify password and inspect debug logs.
  - Hidden SSIDs: unnamed entries can be shown/hidden with 'u'; direct hidden SSID entry is not supported.

Environment variables:
  NMTUI_NO_UPDATE_CHECK=1       Disable automatic update check on startup
  NMTUI_UPDATE_PRERELEASE=1     Include pre-release versions in update checks
  NMTUI_UPDATE_KEEP_BACKUP=0    Don't keep .old backup after update
  GITHUB_TOKEN=<token>          Increase GitHub API rate limit (60->5000/hr)

Debug logging:
  Run with:
      DEBUG_TEA=1 nmtui-go
  This writes nmtui-debug.log in the current directory.
  Sensitive nmcli arguments (passwords, pins, psk) are redacted.

Project:
  GitHub: https://github.com/doeixd/nmtui-go
`, effectiveAppName())
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "%s\n", effectiveAppName())
	fmt.Fprintf(w, "Version: %s\n", Version)
	fmt.Fprintf(w, "Commit: %s\n", Commit)
	fmt.Fprintf(w, "BuildDate: %s\n", BuildDate)
}

func handleCLIFlags(args []string) (exitNow bool, exitCode int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "-h", "--help":
		printUsage(os.Stdout)
		return true, 0
	case "-v", "--version":
		printVersion(os.Stdout)
		return true, 0
	case "--update":
		for _, a := range args[1:] {
			switch a {
			case "--update-prerelease":
				os.Setenv("NMTUI_UPDATE_PRERELEASE", "1")
			case "--no-backup":
				os.Setenv("NMTUI_UPDATE_KEEP_BACKUP", "0")
			}
		}
		exitCode := performSelfUpdateCLI()
		return true, exitCode
	case "--check-update":
		exitCode := performCheckUpdateCLI()
		return true, exitCode
	default:
		if strings.HasPrefix(args[0], "-") {
			fmt.Fprintf(os.Stderr, "Unknown option: %s\n\n", args[0])
			printUsage(os.Stderr)
			return true, 2
		}
	}
	return false, 0
}

func main() { /* Same log setup */
	if exitNow, exitCode := handleCLIFlags(os.Args[1:]); exitNow {
		os.Exit(exitCode)
	}

	logOut := io.Discard
	var logFH *os.File
	if os.Getenv("DEBUG_TEA") != "" {
		var err error
		logFH, err = os.OpenFile(debugLogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Err open log %s: %v\n", debugLogFile, err)
		} else {
			logOut = logFH
			defer func() {
				log.Println("--- NMTUI Log End ---")
				if err := logFH.Close(); err != nil {
					fmt.Fprintf(os.Stderr, "Err close log: %v\n", err)
				}
			}()
		}
	}
	log.SetOutput(logOut)
	log.SetFlags(log.Ltime | log.Lshortfile)
	if os.Getenv("DEBUG_TEA") != "" && logOut != io.Discard {
		log.Println("--- NMTUI Log Start ---")
	}
	im := initialModel()
	p := tea.NewProgram(im, tea.WithAltScreen(), tea.WithMouseCellMotion())
	tuiProgram = p
	fm, err := p.Run()
	if err != nil {
		log.Printf("Err run TUI: %v", err)
		if fmm, ok := fm.(model); ok {
			log.Printf("Final model on err: %+v", fmm)
		}
		if logOut == io.Discard {
			fmt.Fprintf(os.Stderr, "Err run TUI: %v\n", err)
		}
		os.Exit(1)
	}

	// Check if app wants to restart after update
	if fmm, ok := fm.(model); ok && fmm.wantsRestart {
		binPath, err := os.Executable()
		if err == nil {
			binPath, _ = filepath.EvalSymlinks(binPath)
			execErr := syscall.Exec(binPath, os.Args, os.Environ())
			if execErr != nil {
				fmt.Fprintf(os.Stderr, "Updated successfully but restart failed: %v\nPlease restart nmtui-go manually.\n", execErr)
			}
		} else {
			fmt.Fprintf(os.Stderr, "Updated successfully. Please restart nmtui-go manually.\n")
		}
		os.Exit(0)
	}
}
