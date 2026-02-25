package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"nmtui/gonetworkmanager"
)

func windowedModel(t *testing.T) model {
	t.Helper()
	m := initialModel()
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return updated.(model)
}

func TestQDoesNotQuitInPasswordInput(t *testing.T) {
	m := initialModel()
	m.state = viewPasswordInput
	m.passwordInput.SetValue("")
	m.passwordInput.Focus()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := updated.(model)

	if m2.state != viewPasswordInput {
		t.Fatalf("expected to stay in password input, got state %v", m2.state)
	}
	if m2.passwordInput.Value() != "q" {
		t.Fatalf("expected password input to capture q, got %q", m2.passwordInput.Value())
	}
}

func TestZeroResultScanStopsLoading(t *testing.T) {
	m := initialModel()
	m.isLoading = true
	m.isScanning = true

	updated, _ := m.Update(wifiListLoadedMsg{allAps: []wifiAP{}, err: nil})
	m2 := updated.(model)

	if m2.isLoading {
		t.Fatalf("expected isLoading=false")
	}
	if m2.isScanning {
		t.Fatalf("expected isScanning=false")
	}
}

func TestKnownNetworksErrorPreservesCurrentState(t *testing.T) {
	m := initialModel()
	m.knownProfiles = map[string]gonetworkmanager.ConnectionProfile{
		"home": {gonetworkmanager.NmcliFieldConnectionName: "home"},
	}

	updated, _ := m.Update(knownNetworksMsg{err: errors.New("boom")})
	m2 := updated.(model)

	if len(m2.knownProfiles) != 1 {
		t.Fatalf("expected knownProfiles to be preserved, got %d", len(m2.knownProfiles))
	}
	if _, ok := m2.knownProfiles["home"]; !ok {
		t.Fatalf("expected existing known profile to remain")
	}
}

func TestHiddenSSIDCannotConnectFromScanList(t *testing.T) {
	m := initialModel()
	m.state = viewNetworksList
	m.isLoading = false

	hidden := wifiAP{WifiAccessPoint: gonetworkmanager.WifiAccessPoint{
		gonetworkmanager.NmcliFieldWifiSSID:     "--",
		gonetworkmanager.NmcliFieldWifiSecurity: "open",
	}}
	m.wifiList.SetItems([]list.Item{hidden})
	m.wifiList.Select(0)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m2 := updated.(model)

	if m2.state != viewNetworksList {
		t.Fatalf("expected to remain in networks list, got %v", m2.state)
	}
	if !strings.Contains(strings.ToLower(m2.connectionStatusMsg), "hidden ssid") {
		t.Fatalf("expected hidden-ssid warning, got %q", m2.connectionStatusMsg)
	}
}

func TestProfilesShortcutWhileLoading(t *testing.T) {
	m := initialModel()
	m.state = viewNetworksList
	m.isLoading = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	m2 := updated.(model)

	if m2.state != viewKnownNetworksList {
		t.Fatalf("expected known profiles view, got %v", m2.state)
	}
	if !m2.isLoading {
		t.Fatalf("expected loading=true when opening profiles")
	}
	if m2.knownWifiList.Title != "Loading Profiles..." {
		t.Fatalf("unexpected profiles title: %q", m2.knownWifiList.Title)
	}
}

func TestHelpKeyIgnoredWhileFilterTyping(t *testing.T) {
	m := initialModel()
	m.state = viewNetworksList
	m.isFiltering = true
	m.filterInput.SetValue("")
	m.filterInput.Focus()
	m.help.ShowAll = false

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m2 := updated.(model)

	if m2.help.ShowAll {
		t.Fatalf("help should not toggle while text input is active")
	}
	if m2.filterInput.Value() != "?" {
		t.Fatalf("expected filter input to capture '?', got %q", m2.filterInput.Value())
	}
}

func TestForgetResultReturnsToProfilesView(t *testing.T) {
	m := initialModel()
	m.state = viewConfirmForget
	m.previousState = viewKnownNetworksList
	m.knownProfiles = map[string]gonetworkmanager.ConnectionProfile{
		"home": {gonetworkmanager.NmcliFieldConnectionName: "home"},
	}

	updated, _ := m.Update(forgetNetworkResultMsg{ssid: "home", success: true})
	m2 := updated.(model)

	if m2.state != viewKnownNetworksList {
		t.Fatalf("expected to return to profiles view, got %v", m2.state)
	}
	if m2.previousState != viewNetworksList {
		t.Fatalf("expected previousState reset, got %v", m2.previousState)
	}
}

func TestPrintUsageIncludesCoreSections(t *testing.T) {
	var b bytes.Buffer
	printUsage(&b)
	out := b.String()

	for _, section := range []string{
		"Overview:",
		"Usage:",
		"Options:",
		"Features:",
		"Runtime keybindings",
		"Requirements:",
		"Troubleshooting:",
		"Debug logging:",
	} {
		if !strings.Contains(out, section) {
			t.Fatalf("help output missing section %q", section)
		}
	}
}

func TestHandleCLIFlagsUnknownOption(t *testing.T) {
	exitNow, code := handleCLIFlags([]string{"--not-a-real-flag"})
	if !exitNow {
		t.Fatalf("expected unknown flag to request exit")
	}
	if code != 2 {
		t.Fatalf("expected exit code 2 for unknown flag, got %d", code)
	}
}

func TestViewNetworksListRendersCoreUI(t *testing.T) {
	m := initialModel()
	m.state = viewNetworksList
	m.wifiEnabled = true
	m.isLoading = false
	m.isScanning = false
	m.processAndSetWifiList([]wifiAP{{WifiAccessPoint: gonetworkmanager.WifiAccessPoint{
		gonetworkmanager.NmcliFieldWifiSSID:     "CafeWiFi",
		gonetworkmanager.NmcliFieldWifiSignal:   "72",
		gonetworkmanager.NmcliFieldWifiSecurity: "WPA2",
	}}})

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m2 := updated.(model)
	v := m2.View()

	if !strings.Contains(v, effectiveAppName()) {
		t.Fatalf("view missing app name")
	}
	if !strings.Contains(v, "Wi-Fi:") {
		t.Fatalf("view missing wifi status section")
	}
	if !strings.Contains(v, "CafeWiFi") {
		t.Fatalf("view missing network entry")
	}
}

func TestViewKnownProfilesRendersList(t *testing.T) {
	m := initialModel()
	m.state = viewKnownNetworksList
	m.isLoading = false
	m.knownWifiList.Title = "Known Wi-Fi Profiles (1)"
	m.knownWifiList.SetItems([]list.Item{wifiAP{WifiAccessPoint: gonetworkmanager.WifiAccessPoint{
		gonetworkmanager.NmcliFieldWifiSSID:       "HomeNet",
		gonetworkmanager.NmcliFieldConnectionName: "HomeNet",
		gonetworkmanager.NmcliFieldWifiSecurity:   "--",
		gonetworkmanager.NmcliFieldWifiSignal:     "0",
	}, IsKnown: true}})

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m2 := updated.(model)
	v := m2.View()

	if !strings.Contains(v, "Known Wi-Fi Profiles") {
		t.Fatalf("profiles view missing title")
	}
	if !strings.Contains(v, "HomeNet") {
		t.Fatalf("profiles view missing item")
	}
}

func TestViewPasswordInputRendersPrompt(t *testing.T) {
	m := initialModel()
	m.state = viewPasswordInput
	m.selectedAP = wifiAP{WifiAccessPoint: gonetworkmanager.WifiAccessPoint{gonetworkmanager.NmcliFieldWifiSSID: "Office"}}

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m2 := updated.(model)
	v := m2.View()

	if !strings.Contains(v, "Password for") {
		t.Fatalf("password view missing prompt label")
	}
	if !strings.Contains(v, "Office") {
		t.Fatalf("password view missing selected SSID")
	}
}

func TestNewProfileShortcutOpensCreateForm(t *testing.T) {
	m := initialModel()
	m.state = viewKnownNetworksList
	m.isLoading = false

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	m2 := updated.(model)

	if m2.state != viewProfileCreate {
		t.Fatalf("expected create profile view, got %v", m2.state)
	}
	if m2.profileForm.focusIndex != 0 {
		t.Fatalf("expected form focus at first field, got %d", m2.profileForm.focusIndex)
	}
}

func TestQDoesNotQuitInProfileForm(t *testing.T) {
	m := initialModel()
	m.state = viewProfileCreate
	m.focusProfileInput(profileFieldName)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := updated.(model)

	if m2.state != viewProfileCreate {
		t.Fatalf("expected to stay in profile create view, got %v", m2.state)
	}
	if m2.profileForm.inputs[profileFieldName].Value() != "q" {
		t.Fatalf("expected typed q in form field, got %q", m2.profileForm.inputs[profileFieldName].Value())
	}
}

func TestProfileLoadedTransitionsToDetails(t *testing.T) {
	m := initialModel()
	m.state = viewKnownNetworksList
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(model)

	profile := gonetworkmanager.ConnectionProfile{
		gonetworkmanager.NmcliFieldConnectionName: "Office",
		gonetworkmanager.NmcliFieldConnectionUUID: "uuid-1",
		"802-11-wireless.ssid":                    "Office",
		"connection.autoconnect":                  "yes",
	}

	updated, _ = m.Update(profileLoadedMsg{profile: profile, forEdit: false})
	m2 := updated.(model)

	if m2.state != viewProfileDetails {
		t.Fatalf("expected profile details view, got %v", m2.state)
	}
	if m2.activeConnInfoViewport.TotalLineCount() == 0 {
		t.Fatalf("details view content should not be empty")
	}
}

func TestProfileLoadedForEditTransitionsToEditForm(t *testing.T) {
	m := windowedModel(t)
	m.state = viewKnownNetworksList

	profile := gonetworkmanager.ConnectionProfile{
		gonetworkmanager.NmcliFieldConnectionName: "Home",
		gonetworkmanager.NmcliFieldConnectionUUID: "uuid-home",
		"802-11-wireless.ssid":                    "Home",
	}

	updated, _ := m.Update(profileLoadedMsg{profile: profile, forEdit: true})
	m2 := updated.(model)

	if m2.state != viewProfileEdit {
		t.Fatalf("expected profile edit view, got %v", m2.state)
	}
	if m2.profileForm.profileID != "uuid-home" {
		t.Fatalf("expected edit form bound to UUID, got %q", m2.profileForm.profileID)
	}
}

func TestProfileFormValidationCreateRequiresPasswordForWPA(t *testing.T) {
	m := windowedModel(t)
	m.initProfileForm(profileFormCreate, nil)
	m.profileForm.inputs[profileFieldName].SetValue("Office")
	m.profileForm.inputs[profileFieldSSID].SetValue("Office")
	m.profileForm.inputs[profileFieldSecurity].SetValue("wpa-psk")
	m.profileForm.inputs[profileFieldPassword].SetValue("")

	_, _, _, err := m.validateProfileForm()
	if err == nil {
		t.Fatalf("expected validation error for missing WPA password")
	}
}

func TestProfileFormValidationEditAllowsEmptyPasswordAsUnchanged(t *testing.T) {
	m := windowedModel(t)
	m.initProfileForm(profileFormEdit, gonetworkmanager.ConnectionProfile{
		gonetworkmanager.NmcliFieldConnectionUUID: "uuid-1",
		gonetworkmanager.NmcliFieldConnectionName: "Home",
		"802-11-wireless.ssid":                    "Home",
	})
	m.profileForm.inputs[profileFieldSecurity].SetValue("wpa-psk")
	m.profileForm.inputs[profileFieldPassword].SetValue("")

	_, passwordProvided, _, err := m.validateProfileForm()
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if passwordProvided {
		t.Fatalf("expected empty edit password to be treated as unchanged")
	}
}

func TestProfileFormValidationPriorityMustBeInteger(t *testing.T) {
	m := windowedModel(t)
	m.initProfileForm(profileFormCreate, nil)
	m.profileForm.inputs[profileFieldName].SetValue("Lab")
	m.profileForm.inputs[profileFieldSSID].SetValue("Lab")
	m.profileForm.inputs[profileFieldSecurity].SetValue("open")
	m.profileForm.inputs[profileFieldPriority].SetValue("high")

	_, _, _, err := m.validateProfileForm()
	if err == nil {
		t.Fatalf("expected integer validation error for priority")
	}
}

func TestProfileFormDiscardConfirmationFlow(t *testing.T) {
	m := windowedModel(t)
	m.state = viewProfileCreate
	m.initProfileForm(profileFormCreate, nil)
	m.profileForm.inputs[profileFieldName].SetValue("Changed")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m2 := updated.(model)
	if m2.state != viewProfileCreate {
		t.Fatalf("first esc should keep user in form, got %v", m2.state)
	}
	if !strings.Contains(strings.ToLower(m2.profileForm.statusMsg), "unsaved") {
		t.Fatalf("expected unsaved warning, got %q", m2.profileForm.statusMsg)
	}

	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m3 := updated.(model)
	if m3.state != viewKnownNetworksList {
		t.Fatalf("second esc should discard and return to list, got %v", m3.state)
	}
}

func TestProfileFormClearPasswordFlagResetsOnTyping(t *testing.T) {
	m := windowedModel(t)
	m.state = viewProfileEdit
	m.initProfileForm(profileFormEdit, gonetworkmanager.ConnectionProfile{
		gonetworkmanager.NmcliFieldConnectionUUID: "uuid-1",
		gonetworkmanager.NmcliFieldConnectionName: "Home",
		"802-11-wireless.ssid":                    "Home",
	})
	m.focusProfileInput(profileFieldPassword)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m2 := updated.(model)
	if !m2.profileForm.clearPassword {
		t.Fatalf("expected clear password flag to be set")
	}

	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	m3 := updated.(model)
	if m3.profileForm.clearPassword {
		t.Fatalf("typing password should clear clearPassword flag")
	}
}

func TestProfileSaveSuccessReturnsToProfilesWithStatus(t *testing.T) {
	m := windowedModel(t)
	m.state = viewProfileCreate

	updated, _ := m.Update(profileSaveResultMsg{success: true, action: "created", profileRef: "Office"})
	m2 := updated.(model)

	if m2.state != viewKnownNetworksList {
		t.Fatalf("expected to return to profiles list, got %v", m2.state)
	}
	if !strings.Contains(strings.ToLower(m2.connectionStatusMsg), "created") {
		t.Fatalf("expected success status mentioning created, got %q", m2.connectionStatusMsg)
	}
}

func TestProfileSaveFailureStaysInEditWithError(t *testing.T) {
	m := windowedModel(t)
	m.state = viewProfileEdit
	m.profileForm.mode = profileFormEdit

	updated, _ := m.Update(profileSaveResultMsg{success: false, err: errors.New("boom")})
	m2 := updated.(model)

	if m2.state != viewProfileEdit {
		t.Fatalf("expected to stay in edit view on save failure, got %v", m2.state)
	}
	if !strings.Contains(strings.ToLower(m2.profileForm.statusMsg), "failed") {
		t.Fatalf("expected form failure status, got %q", m2.profileForm.statusMsg)
	}
}

func TestParseYesNoVariants(t *testing.T) {
	trueVals := []string{"yes", "Y", "true", "1", "on"}
	for _, v := range trueVals {
		b, err := parseYesNo(v)
		if err != nil || !b {
			t.Fatalf("expected %q to parse true", v)
		}
	}
	falseVals := []string{"no", "N", "false", "0", "off"}
	for _, v := range falseVals {
		b, err := parseYesNo(v)
		if err != nil || b {
			t.Fatalf("expected %q to parse false", v)
		}
	}
	if _, err := parseYesNo("maybe"); err == nil {
		t.Fatalf("expected invalid yes/no to error")
	}
}

func TestNormalizeSecurity(t *testing.T) {
	if got := normalizeSecurity("open"); got != "open" {
		t.Fatalf("expected open, got %q", got)
	}
	if got := normalizeSecurity("none"); got != "open" {
		t.Fatalf("expected none->open, got %q", got)
	}
	if got := normalizeSecurity("WPA2"); got != "wpa-psk" {
		t.Fatalf("expected WPA2->wpa-psk, got %q", got)
	}
}

func TestProfileFormPriorityParsed(t *testing.T) {
	m := windowedModel(t)
	m.initProfileForm(profileFormCreate, nil)
	m.profileForm.inputs[profileFieldName].SetValue("X")
	m.profileForm.inputs[profileFieldSSID].SetValue("X")
	m.profileForm.inputs[profileFieldSecurity].SetValue("open")
	m.profileForm.inputs[profileFieldPriority].SetValue("42")

	spec, _, pri, err := m.validateProfileForm()
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if spec.Priority == nil || pri == nil || *pri != 42 {
		t.Fatalf("expected parsed priority 42, got %v / %v", spec.Priority, pri)
	}
	if spec.Priority != nil && strconv.Itoa(*spec.Priority) != "42" {
		t.Fatalf("unexpected priority in spec")
	}
}
