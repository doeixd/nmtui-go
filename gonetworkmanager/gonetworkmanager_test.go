package gonetworkmanager

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParseNmcliMultilineOutput(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantLen int
		wantErr bool
	}{
		{name: "empty", input: "", wantLen: 0},
		{name: "single record", input: "SSID: home\nSIGNAL: 80", wantLen: 1},
		{name: "two records by repeated first key", input: "SSID: one\nSIGNAL: 30\nSSID: two\nSIGNAL: 60", wantLen: 2},
		{name: "malformed line", input: "SSID: one\nBADLINE", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseNmcliMultilineOutput(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("expected %d records, got %d", tc.wantLen, len(got))
			}
		})
	}
}

func TestParseDeviceState(t *testing.T) {
	tests := map[string]string{
		"":                 "unknown",
		"100":              "activated",
		"100 (connected)":  "connected",
		"300":              "Unknown code (300)",
		"already-readable": "already-readable",
	}

	for input, want := range tests {
		got := parseDeviceState(input)
		if got != want {
			t.Fatalf("parseDeviceState(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRedactNmcliArgs(t *testing.T) {
	in := []string{"device", "wifi", "connect", "myssid", "password", "hunter2", "wifi-sec.psk=abc123", "pin", "0000"}
	want := []string{"device", "wifi", "connect", "myssid", "password", "<redacted>", "wifi-sec.psk=<redacted>", "pin", "<redacted>"}

	got := redactNmcliArgs(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redactNmcliArgs mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestRedactSensitiveValues(t *testing.T) {
	args := []string{"device", "wifi", "connect", "myssid", "password", "hunter2", "wifi-sec.psk=abc123", "pin", "0000"}
	msg := "failed with hunter2 and abc123 and 0000"
	got := redactSensitiveValues(msg, args)
	if got != "failed with <redacted> and <redacted> and <redacted>" {
		t.Fatalf("unexpected redaction result: %q", got)
	}
}

func TestRunNmcliAndClibInternalWithMockBinary(t *testing.T) {
	setupMockNmcli(t)

	out, err := runNmcli("ok")
	if err != nil {
		t.Fatalf("runNmcli(ok) unexpected error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("runNmcli(ok) output = %q, want %q", out, "ok")
	}

	_, err = runNmcli("fail", "password", "supersecret")
	if err == nil {
		t.Fatalf("expected error for runNmcli(fail)")
	}
	errText := err.Error()
	if containsAny(errText, []string{"supersecret"}) {
		t.Fatalf("error leaked secret: %q", errText)
	}
	if !containsAny(errText, []string{"<redacted>"}) {
		t.Fatalf("error should include redaction marker: %q", errText)
	}

	records, err := clibInternal("multi")
	if err != nil {
		t.Fatalf("clibInternal(multi) unexpected error: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 multiline records, got %d", len(records))
	}
}

func TestClibInternalErrorIsRedacted(t *testing.T) {
	setupMockNmcli(t)
	_, err := clibInternal("fail", "password", "supersecret")
	if err == nil {
		t.Fatalf("expected clibInternal error")
	}
	msg := err.Error()
	if strings.Contains(msg, "supersecret") {
		t.Fatalf("clibInternal error leaked secret: %q", msg)
	}
}

func TestCreateWifiProfileValidation(t *testing.T) {
	_, err := CreateWifiProfile(WifiProfileSpec{})
	if err == nil {
		t.Fatalf("expected validation error for empty profile spec")
	}

	_, err = CreateWifiProfile(WifiProfileSpec{Name: "x", SSID: "y", Security: "wpa-psk"})
	if err == nil {
		t.Fatalf("expected validation error for missing WPA password")
	}
}

func TestUpdateWifiProfileValidation(t *testing.T) {
	_, err := UpdateWifiProfile("", WifiProfileSpec{Name: "x", SSID: "y"}, false, false)
	if err == nil {
		t.Fatalf("expected validation error for empty profile id")
	}
}

func TestCreateAndUpdateWifiProfileWithMockBinary(t *testing.T) {
	setupMockNmcli(t)

	_, err := CreateWifiProfile(WifiProfileSpec{Name: "Office", SSID: "Office", Security: "open", Autoconnect: true})
	if err != nil {
		t.Fatalf("CreateWifiProfile unexpected error: %v", err)
	}

	_, err = UpdateWifiProfile("uuid-1", WifiProfileSpec{Name: "Office", SSID: "Office", Security: "open", Autoconnect: true}, false, false)
	if err != nil {
		t.Fatalf("UpdateWifiProfile unexpected error: %v", err)
	}
}

func TestGetConnectionProfileByIDWithMockBinary(t *testing.T) {
	setupMockNmcli(t)

	p, err := GetConnectionProfileByID("uuid-1")
	if err != nil {
		t.Fatalf("GetConnectionProfileByID unexpected error: %v", err)
	}
	if p == nil {
		t.Fatalf("expected non-nil profile")
	}
	if p[NmcliFieldConnectionUUID] != "uuid-1" {
		t.Fatalf("expected UUID uuid-1, got %q", p[NmcliFieldConnectionUUID])
	}
}

func TestNormalizeWifiSecurityMode(t *testing.T) {
	if got := normalizeWifiSecurityMode("open"); got != "open" {
		t.Fatalf("expected open, got %q", got)
	}
	if got := normalizeWifiSecurityMode("none"); got != "open" {
		t.Fatalf("expected none->open, got %q", got)
	}
	if got := normalizeWifiSecurityMode("wpa2"); got != "wpa-psk" {
		t.Fatalf("expected wpa2->wpa-psk, got %q", got)
	}
}

func setupMockNmcli(t *testing.T) {
	t.Helper()
	dir := t.TempDir()

	var scriptPath string
	var scriptBody string
	if runtime.GOOS == "windows" {
		scriptPath = filepath.Join(dir, "nmcli.bat")
		scriptBody = "@echo off\r\n" +
			"if \"%1\"==\"ok\" (\r\n" +
			"  echo ok\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"if \"%1\"==\"multi\" (\r\n" +
			"  echo NAME: one\r\n" +
			"  echo TYPE: wifi\r\n" +
			"  echo NAME: two\r\n" +
			"  echo TYPE: wifi\r\n" +
			"  exit /b 0\r\n" +
			")\r\n" +
			"if \"%1\"==\"fail\" (\r\n" +
			"  echo command failed for %2 %3 1>&2\r\n" +
			"  exit /b 7\r\n" +
			")\r\n" +
			"if \"%1\"==\"connection\" (\r\n" +
			"  if \"%2\"==\"add\" (\r\n" +
			"    echo profile added\r\n" +
			"    exit /b 0\r\n" +
			"  )\r\n" +
			"  if \"%2\"==\"modify\" (\r\n" +
			"    echo profile modified\r\n" +
			"    exit /b 0\r\n" +
			"  )\r\n" +
			")\r\n" +
			"if \"%1\"==\"-m\" (\r\n" +
			"  if \"%2\"==\"multiline\" if \"%3\"==\"connection\" if \"%4\"==\"show\" (\r\n" +
			"    echo NAME: office\r\n" +
			"    echo UUID: uuid-1\r\n" +
			"    echo TYPE: wifi\r\n" +
			"    echo 802-11-wireless.ssid: office\r\n" +
			"    exit /b 0\r\n" +
			"  )\r\n" +
			")\r\n" +
			"echo unknown\r\n" +
			"exit /b 9\r\n"
	} else {
		scriptPath = filepath.Join(dir, "nmcli")
		scriptBody = "#!/bin/sh\n" +
			"if [ \"$1\" = \"ok\" ]; then\n" +
			"  echo ok\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = \"multi\" ]; then\n" +
			"  printf 'NAME: one\\nTYPE: wifi\\nNAME: two\\nTYPE: wifi\\n'\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = \"fail\" ]; then\n" +
			"  echo \"command failed for $2 $3\" 1>&2\n" +
			"  exit 7\n" +
			"fi\n" +
			"if [ \"$1\" = \"connection\" ] && [ \"$2\" = \"add\" ]; then\n" +
			"  echo profile added\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = \"connection\" ] && [ \"$2\" = \"modify\" ]; then\n" +
			"  echo profile modified\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = \"-m\" ] && [ \"$2\" = \"multiline\" ] && [ \"$3\" = \"connection\" ] && [ \"$4\" = \"show\" ]; then\n" +
			"  printf 'NAME: office\\nUUID: uuid-1\\nTYPE: wifi\\n802-11-wireless.ssid: office\\n'\n" +
			"  exit 0\n" +
			"fi\n" +
			"echo unknown\n" +
			"exit 9\n"
	}

	if err := os.WriteFile(scriptPath, []byte(scriptBody), 0700); err != nil {
		t.Fatalf("failed to write mock nmcli: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(scriptPath, 0700); err != nil {
			t.Fatalf("failed to chmod mock nmcli: %v", err)
		}
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}
