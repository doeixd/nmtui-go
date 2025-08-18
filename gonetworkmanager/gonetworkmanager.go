// nmtui/gonetworkmanager/gonetworkmanager.go
package gonetworkmanager

import (
	"bufio"   // For DeviceStatus parsing
	"bytes"
	"context" // For ActivityMonitor
	"fmt"
	"io"      // For ActivityMonitor
	"log"
	"net"     // For GetIPv4
	"os/exec"
	"strconv" // For parseDeviceState and others
	"strings"
	"syscall" // For ActivityMonitor signal handling
)

// --- Constants for nmcli field names ---
const (
	NmcliFieldGeneralDevice      = "GENERAL.DEVICE"
	NmcliFieldGeneralType        = "GENERAL.TYPE"
	NmcliFieldGeneralState       = "GENERAL.STATE"
	NmcliFieldGeneralConnection  = "GENERAL.CONNECTION"
	NmcliFieldGeneralHwAddr      = "GENERAL.HWADDR"
	NmcliFieldIP4Address1        = "IP4.ADDRESS[1]"
	NmcliFieldIP4Gateway         = "IP4.GATEWAY"
	NmcliFieldDns1               = "IP4.DNS[1]"
	NmcliFieldDns2               = "IP4.DNS[2]"
	NmcliFieldIP6Address1        = "IP6.ADDRESS[1]"
	NmcliFieldIP6Gateway         = "IP6.GATEWAY"
	NmcliFieldConnectionName     = "NAME"
	NmcliFieldConnectionUUID     = "UUID"
	NmcliFieldConnectionType     = "TYPE"
	NmcliFieldConnectionDevice   = "DEVICE"
	NmcliFieldConnectionState    = "STATE"
	NmcliFieldWifiSSID           = "SSID"
	NmcliFieldWifiBSSID          = "BSSID"
	NmcliFieldWifiSignal         = "SIGNAL"
	NmcliFieldWifiSecurity       = "SECURITY"
	NmcliFieldWifiInUse          = "IN-USE"
	NmcliFieldDeviceStatusDevice = "DEVICE"
	NmcliFieldDeviceStatusType   = "TYPE"
	NmcliFieldDeviceStatusState  = "STATE"
	NmcliFieldDeviceStatusConn   = "CONNECTION"

	wifiSecKeyMgmt       = "wifi-sec.key-mgmt"
	wifiSecPSK           = "wifi-sec.psk"
	ConnectionTypeWifi   = "wifi"
	// connectionTypeEth    = "ethernet" // Already have this effectively with NmcliFieldConnectionType
	keyMgmtWPAPSK        = "wpa-psk"
	eightZeroTwo11SSID   = "802-11-wireless.ssid"
	// eightZeroTwo11SecKM  = "802-11-wireless-security.key-mgmt" // Covered by wifiSecKeyMgmt
	// eightZeroTwo11SecPSK = "802-11-wireless-security.psk" // Covered by wifiSecPSK
)

// --- Type Definitions ---
type IPv4Info struct {
	InterfaceName string `json:"interfaceName"`
	Address       string `json:"address"`
	Netmask       string `json:"netmask"`
	Mac           string `json:"mac"`
}

type DeviceOverallStatus struct {
	Device     string `json:"device"`
	Type       string `json:"type"`
	State      string `json:"state"`
	Connection string `json:"connection,omitempty"`
}

type DeviceIPDetail struct {
	Device     string   `json:"device,omitempty"`
	Type       string   `json:"type,omitempty"`
	State      string   `json:"state"`
	Connection string   `json:"connection,omitempty"`
	Mac        string   `json:"mac,omitempty"`
	IPv4       string   `json:"ipV4,omitempty"`
	NetV4      string   `json:"netV4,omitempty"`
	GatewayV4  string   `json:"gatewayV4,omitempty"`
	DNS        []string `json:"dns,omitempty"`
	IPv6       string   `json:"ipV6,omitempty"`
	NetV6      string   `json:"netV6,omitempty"`
	GatewayV6  string   `json:"gatewayV6,omitempty"`
}

type ConnectionProfile map[string]string
type WifiAccessPoint map[string]string
type WifiCredentialsType map[string]string
type StopActivityMonitorFn func() error

// --- Core nmcli Interaction ---
func parseNmcliMultilineOutput(output string) ([]map[string]string, error) {
	output = strings.TrimSpace(output)
	if output == "" { return []map[string]string{}, nil }
	lines := strings.Split(output, "\n")
	if len(lines) == 0 { return []map[string]string{}, nil }
	var records []map[string]string
	var currentRecord map[string]string
	var firstKeyOfRecord string
	for i, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" { continue }
		parts := strings.SplitN(trimmedLine, ":", 2)
		if len(parts) != 2 {
			if i == 0 && !strings.Contains(trimmedLine, ":") { continue }
			return nil, fmt.Errorf("malformed line in multiline output: \"%s\"", trimmedLine)
		}
		key := strings.TrimSpace(parts[0])
		// Only trim leading whitespace from the value to preserve trailing spaces in SSIDs.
		value := strings.TrimLeft(parts[1], " \t")
		if key == "" { return nil, fmt.Errorf("empty key for value: \"%s\"", value) }
		if currentRecord == nil { currentRecord = make(map[string]string); firstKeyOfRecord = key
		} else if key == firstKeyOfRecord && len(currentRecord) > 0 {
			records = append(records, currentRecord); currentRecord = make(map[string]string)
		}
		currentRecord[key] = value
	}
	if len(currentRecord) > 0 { records = append(records, currentRecord) }
	return records, nil
}

func runNmcli(args ...string) (string, error) {
	cmd := exec.Command("nmcli", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout; cmd.Stderr = &stderr
	log.Printf("Executing nmcli command: %v", cmd.Args)
	err := cmd.Run()
	stderrStr := strings.TrimSpace(stderr.String()); stdoutStr := strings.TrimSpace(stdout.String())
	if err != nil {
		if stderrStr != "" {
			log.Printf("nmcli command '%s' stderr: %s", strings.Join(args, " "), stderrStr)
			return stdoutStr, fmt.Errorf("nmcli command '%s' failed: %s (underlying error: %w)", strings.Join(args, " "), stderrStr, err)
		}
		return stdoutStr, fmt.Errorf("nmcli command '%s' failed: %w", strings.Join(args, " "), err)
	}
	if stderrStr != "" {
		log.Printf("nmcli command '%s' succeeded but produced stderr (warning): %s", strings.Join(args, " "), stderrStr)
	}
	return stdoutStr, nil
}
func cliInternal(args ...string) (string, error) { return runNmcli(args...) }
func clibInternal(args ...string) ([]map[string]string, error) {
	output, err := runNmcli(args...); if err != nil { return nil, fmt.Errorf("nmcli for multiline failed (args: %v): %w", args, err) }
	return parseNmcliMultilineOutput(output)
}

// --- Public API Functions ---

// GetIPv4 retrieves IPv4 network interface details using Go's native `net` package.
func GetIPv4() ([]IPv4Info, error) {
	ifaces, err := net.Interfaces()
	if err != nil { return nil, fmt.Errorf("failed to get network interfaces: %w", err) }
	var results []IPv4Info
	for _, i := range ifaces {
		if (i.Flags&net.FlagLoopback) != 0 || (i.Flags&net.FlagUp) == 0 || i.HardwareAddr == nil { continue }
		addrs, err := i.Addrs(); if err != nil { continue }
		var ipv4Info IPv4Info
		ipv4Info.InterfaceName = i.Name; ipv4Info.Mac = i.HardwareAddr.String(); foundIPv4 := false
		for _, addr := range addrs {
			var ip net.IP; var mask net.IPMask
			switch v := addr.(type) {
			case *net.IPNet: ip = v.IP; mask = v.Mask
			case *net.IPAddr: ip = v.IP
			}
			if ip == nil || ip.IsLoopback() { continue }
			ipv4 := ip.To4()
			if ipv4 != nil {
				ipv4Info.Address = ipv4.String()
				if mask != nil {
					if len(mask) == net.IPv4len { ipv4Info.Netmask = fmt.Sprintf("%d.%d.%d.%d", mask[0], mask[1], mask[2], mask[3])
					} else if len(mask) == net.IPv6len && isIPv4Mask(mask) { ipv4Info.Netmask = fmt.Sprintf("%d.%d.%d.%d", mask[12], mask[13], mask[14], mask[15]) }
				}
				foundIPv4 = true; break
			}
		}
		if foundIPv4 && ipv4Info.Address != "" { results = append(results, ipv4Info) }
	}
	return results, nil
}

func isIPv4Mask(mask net.IPMask) bool {
	for i := 0; i < 10; i++ { if mask[i] != 0 { return false } }
	return mask[10] == 0xff && mask[11] == 0xff
}

// ActivityMonitor monitors NetworkManager activity.
func ActivityMonitor(ctx context.Context, writer io.Writer) (StopActivityMonitorFn, error) {
	monitorCtx, cancelMonitorCmd := context.WithCancel(ctx)
	cmd := exec.CommandContext(monitorCtx, "nmcli", "monitor")
	cmd.Stdout = writer; cmd.Stderr = writer
	if err := cmd.Start(); err != nil { cancelMonitorCmd(); return nil, fmt.Errorf("failed to start 'nmcli monitor': %w", err) }
	stopFn := func() error {
		cancelMonitorCmd(); err := cmd.Wait()
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				if status.Signaled() && (status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGINT) { return nil }
			}
		}
		return err
	}
	go func() { _ = cmd.Wait(); cancelMonitorCmd() }()
	return stopFn, nil
}

// GetHostName gets the current system hostname.
func GetHostName() (string, error) { return cliInternal("general", "hostname") }

// SetHostName sets the system hostname.
func SetHostName(newHostName string) (string, error) {
	if strings.TrimSpace(newHostName) == "" { return "", fmt.Errorf("new hostname cannot be empty") }
	return cliInternal("general", "hostname", newHostName)
}

// EnableNetworking enables all networking.
func EnableNetworking() (string, error) { return cliInternal("networking", "on") }

// DisableNetworking disables all networking.
func DisableNetworking() (string, error) { return cliInternal("networking", "off") }

// GetNetworkConnectivityState gets overall network connectivity state.
func GetNetworkConnectivityState(recheck bool) (string, error) {
	args := []string{"networking", "connectivity"}; if recheck { args = append(args, "check") }
	return cliInternal(args...)
}

// ConnectionUp activates a connection profile.
func ConnectionUp(profileIdentifier string) (string, error) {
	if strings.TrimSpace(profileIdentifier) == "" { return "", fmt.Errorf("profile identifier cannot be empty") }
	return cliInternal("connection", "up", profileIdentifier)
}

// ConnectionDown deactivates a connection profile.
func ConnectionDown(profileIdentifier string) (string, error) {
	if strings.TrimSpace(profileIdentifier) == "" { return "", fmt.Errorf("profile identifier cannot be empty") }
	return cliInternal("connection", "down", profileIdentifier)
}

// ConnectionDelete deletes a connection profile.
func ConnectionDelete(profileIdentifier string) (string, error) {
	if strings.TrimSpace(profileIdentifier) == "" { return "", fmt.Errorf("profile identifier cannot be empty") }
	return cliInternal("connection", "delete", profileIdentifier)
}

// GetConnectionProfilesList lists connection profiles.
func GetConnectionProfilesList(activeOnly bool) ([]ConnectionProfile, error) {
	args := []string{"-m", "multiline", "connection", "show", "--order", "name"}; if activeOnly { args = append(args, "--active") }
	rawProfiles, err := clibInternal(args...); if err != nil { return nil, err }
	profiles := make([]ConnectionProfile, len(rawProfiles))
	for i, rp := range rawProfiles { profiles[i] = ConnectionProfile(rp) }
	return profiles, nil
}

// ChangeDnsConnection modifies DNS servers for a connection profile.
func ChangeDnsConnection(profileIdentifier string, dnsServers string) (string, error) {
	if strings.TrimSpace(profileIdentifier) == "" { return "", fmt.Errorf("profile identifier cannot be empty") }
	return cliInternal("connection", "modify", profileIdentifier, "ipv4.dns", dnsServers)
}

// AddEthernetConnection adds an Ethernet connection profile with static IP.
func AddEthernetConnection(connectionName, interfaceName, ipv4Address, gateway string, cidrPrefix int) (string, error) {
	if strings.TrimSpace(connectionName) == "" { return "", fmt.Errorf("connection name cannot be empty") }
	if strings.TrimSpace(ipv4Address) == "" { return "", fmt.Errorf("IPv4 address cannot be empty") }
	if interfaceName == "" { interfaceName = "enp0s3" }
	if cidrPrefix <= 0 || cidrPrefix > 32 { cidrPrefix = 24 }
	return cliInternal("connection", "add", "type", "ethernet", "con-name", connectionName, "ifname", interfaceName,
		"ipv4.method", "manual", "ipv4.addresses", fmt.Sprintf("%s/%d", ipv4Address, cidrPrefix), "gw4", gateway)
}

// AddGsmConnection adds a GSM connection profile.
func AddGsmConnection(connectionName, interfaceName, apn, username, password, pin string) (string, error) {
	if strings.TrimSpace(connectionName) == "" { return "", fmt.Errorf("connection name cannot be empty") }
	if interfaceName == "" { interfaceName = "*" }
	args := []string{"connection", "add", "type", "gsm", "con-name", connectionName, "ifname", interfaceName}
	if apn != "" { args = append(args, "apn", apn) }
	if username != "" { args = append(args, "username", username) }
	if password != "" { args = append(args, "password", password) }
	if pin != "" { args = append(args, "pin", pin) }
	return cliInternal(args...)
}

// DeviceConnect connects a network device.
func DeviceConnect(deviceInterface string) (string, error) {
	if strings.TrimSpace(deviceInterface) == "" { return "", fmt.Errorf("device interface cannot be empty") }
	return cliInternal("device", "connect", deviceInterface)
}

// DeviceDisconnect disconnects a network device.
func DeviceDisconnect(deviceInterface string) (string, error) {
	if strings.TrimSpace(deviceInterface) == "" { return "", fmt.Errorf("device interface cannot be empty") }
	return cliInternal("device", "disconnect", deviceInterface)
}

var deviceStateMap = map[int]string{
	0:"unknown",10:"unmanaged",20:"unavailable",30:"disconnected",40:"prepare",50:"config",
	60:"need-auth",70:"ip-config",80:"ip-check",90:"secondaries",100:"activated",
	110:"deactivating",120:"failed",
}

// parseDeviceState is now defined here.
func parseDeviceState(stateStr string) string {
	stateStr = strings.TrimSpace(stateStr)
	if stateStr == "" { return "unknown" }
	if strings.Contains(stateStr, "(") && strings.HasSuffix(stateStr, ")") {
		openParenIndex := strings.Index(stateStr, "(")
		if openParenIndex > 0 {
			potentialCodeStr := strings.TrimSpace(stateStr[:openParenIndex])
			if _, err := strconv.Atoi(potentialCodeStr); err == nil {
				descPart := strings.TrimSuffix(stateStr[openParenIndex+1:], ")")
				return strings.TrimSpace(descPart)
			}
		}
	}
	if code, err := strconv.Atoi(stateStr); err == nil {
		if desc, ok := deviceStateMap[code]; ok { return desc }
		return fmt.Sprintf("Unknown code (%d)", code)
	}
	return stateStr
}

// DeviceStatus gets the status of all network devices.
func DeviceStatus() ([]DeviceOverallStatus, error) {
	output, err := cliInternal("-t", "-f", fmt.Sprintf("%s,%s,%s,%s", NmcliFieldDeviceStatusDevice, NmcliFieldDeviceStatusType, NmcliFieldDeviceStatusState, NmcliFieldDeviceStatusConn), "device")
	if err != nil { return nil, fmt.Errorf("failed to get device status: %w", err) }
	var statuses []DeviceOverallStatus
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text(); parts := strings.Split(line, ":")
		if len(parts) < 3 { continue }
		status := DeviceOverallStatus{
			Device:strings.TrimSpace(parts[0]), Type:strings.TrimSpace(parts[1]), State:parseDeviceState(strings.TrimSpace(parts[2])),
		}
		if len(parts) > 3 { connection := strings.TrimSpace(parts[3]); if connection != "" && connection != "--" { status.Connection = connection } }
		statuses = append(statuses, status)
	}
	if err := scanner.Err(); err != nil { return nil, fmt.Errorf("error reading device status output: %w", err) }
	return statuses, nil
}

// GetDeviceInfoIPDetail gets detailed IP config for a specific device.
func GetDeviceInfoIPDetail(deviceName string) (*DeviceIPDetail, error) {
	if strings.TrimSpace(deviceName) == "" { return nil, fmt.Errorf("device name cannot be empty") }
	data, err := clibInternal("-m", "multiline", "device", "show", deviceName); if err != nil { return nil, err }
	if len(data) == 0 { return nil, nil } // Device not found
	item := data[0]; stateStr := item[NmcliFieldGeneralState]
	detail := &DeviceIPDetail{
		Device:item[NmcliFieldGeneralDevice], Type:item[NmcliFieldGeneralType], State:parseDeviceState(stateStr),
		Connection:item[NmcliFieldGeneralConnection], Mac:item[NmcliFieldGeneralHwAddr],
		NetV4:item[NmcliFieldIP4Address1], GatewayV4:item[NmcliFieldIP4Gateway],
		NetV6:item[NmcliFieldIP6Address1], GatewayV6:item[NmcliFieldIP6Gateway], DNS:[]string{},
	}
	if dns1, ok := item[NmcliFieldDns1]; ok && dns1 != "" { detail.DNS = append(detail.DNS, strings.Fields(dns1)[0]) }
	if dns2, ok := item[NmcliFieldDns2]; ok && dns2 != "" { detail.DNS = append(detail.DNS, strings.Fields(dns2)[0]) }
	if detail.Connection == "--" { detail.Connection = "" }
	if detail.NetV4 != "" { if parts := strings.SplitN(detail.NetV4, "/", 2); len(parts) > 0 { detail.IPv4 = parts[0] } }
	if detail.NetV6 != "" { if parts := strings.SplitN(detail.NetV6, "/", 2); len(parts) > 0 { detail.IPv6 = parts[0] } }
	return detail, nil
}

// GetAllDeviceInfoIPDetail gets detailed IP config for all devices.
func GetAllDeviceInfoIPDetail() ([]DeviceIPDetail, error) {
	data, err := clibInternal("-m", "multiline", "device", "show"); if err != nil { return nil, err }
	var details []DeviceIPDetail
	for _, item := range data {
		stateStr := item[NmcliFieldGeneralState]
		detail := DeviceIPDetail{
			Device:item[NmcliFieldGeneralDevice], Type:item[NmcliFieldGeneralType], State:parseDeviceState(stateStr),
			Connection:item[NmcliFieldGeneralConnection], Mac:item[NmcliFieldGeneralHwAddr],
			NetV4:item[NmcliFieldIP4Address1], GatewayV4:item[NmcliFieldIP4Gateway],
			NetV6:item[NmcliFieldIP6Address1], GatewayV6:item[NmcliFieldIP6Gateway], DNS:[]string{},
		}
		if dns1, ok := item[NmcliFieldDns1]; ok && dns1 != "" { detail.DNS = append(detail.DNS, strings.Fields(dns1)[0]) }
		if dns2, ok := item[NmcliFieldDns2]; ok && dns2 != "" { detail.DNS = append(detail.DNS, strings.Fields(dns2)[0]) }
		if detail.Connection == "--" { detail.Connection = "" }
		if detail.NetV4 != "" { if parts := strings.SplitN(detail.NetV4, "/", 2); len(parts) > 0 { detail.IPv4 = parts[0] } }
		if detail.NetV6 != "" { if parts := strings.SplitN(detail.NetV6, "/", 2); len(parts) > 0 { detail.IPv6 = parts[0] } }
		details = append(details, detail)
	}
	return details, nil
}

// --- Wi-Fi ---
func WifiEnable() (string, error) { return cliInternal("radio", "wifi", "on") }
func WifiDisable() (string, error) { return cliInternal("radio", "wifi", "off") }
func GetWifiStatus() (string, error) { return cliInternal("radio", "wifi") }

func WifiHotspot(interfaceName, ssid, password string) ([]map[string]string, error) {
	if strings.TrimSpace(interfaceName) == "" { return nil, fmt.Errorf("hotspot interface name empty") }
	if strings.TrimSpace(ssid) == "" { return nil, fmt.Errorf("hotspot SSID empty") }
	if len(password) < 8 || len(password) > 63 { return nil, fmt.Errorf("hotspot password must be 8-63 chars") }
	return clibInternal("device", "wifi", "hotspot", "ifname", interfaceName, "ssid", ssid, "password", password)
}

func WifiCredentials(interfaceName string) (WifiCredentialsType, error) {
	if strings.TrimSpace(interfaceName) == "" { return nil, fmt.Errorf("wifi creds ifname empty") }
	data, err := clibInternal("-m", "multiline", "device", "wifi", "show-password", "ifname", interfaceName)
	if err != nil { return nil, err }
	if len(data) == 0 { return WifiCredentialsType{}, nil }
	return WifiCredentialsType(data[0]), nil
}

func GetWifiList(rescan bool) ([]WifiAccessPoint, error) {
	rescanArg := "no"; if rescan { rescanArg = "yes" }
	args := []string{"-m", "multiline", "device", "wifi", "list", "--rescan", rescanArg}
	rawData, err := clibInternal(args...); if err != nil { return nil, err }
	var wifiList []WifiAccessPoint
	for _, item := range rawData {
		ap := WifiAccessPoint(item)
		if inUse, ok := ap[NmcliFieldWifiInUse]; ok && inUse == "*" { ap["inUseBoolean"] = "true"
		} else { ap["inUseBoolean"] = "false" }
		wifiList = append(wifiList, ap)
	}
	return wifiList, nil
}

func WifiConnect(ssid string, password string, hidden bool) (string, error) {
	if strings.TrimSpace(ssid) == "" { return "", fmt.Errorf("SSID empty for Wi-Fi connect") }
	args := []string{"device", "wifi", "connect", ssid}
	if password != "" { args = append(args, "password", password) }
	if hidden { args = append(args, "hidden", "yes") }
	return cliInternal(args...)
}

func AddWifiConnectionPSK(profileName, ifname, ssid, password string) (string, error) {
	if strings.TrimSpace(profileName) == "" { return "", fmt.Errorf("profile name empty") }
	if strings.TrimSpace(ssid) == "" { return "", fmt.Errorf("SSID empty") }
	if strings.TrimSpace(password) == "" { return "", fmt.Errorf("password empty for WPA-PSK") }
	// ifname is typically "*" when called from ConnectToWifiRobustly

	profiles, err := GetConnectionProfilesList(false)
	if err != nil { return "", fmt.Errorf("could not list profiles to check for existing: %w", err) }

	var existingProfile ConnectionProfile
	var existingProfileIdentifier string // Will hold NAME or UUID for deletion/modification

	for _, p := range profiles {
		profileSSID := GetSSIDFromProfile(p)
		// Match by profile name OR by SSID if profile name is different but SSID is the same (common scenario)
		if p[NmcliFieldConnectionName] == profileName || (profileSSID == ssid && p[NmcliFieldConnectionType] == ConnectionTypeWifi) {
			existingProfile = p
			existingProfileIdentifier = p[NmcliFieldConnectionName] // Prefer name for operations
			if existingProfileIdentifier == "" {
				existingProfileIdentifier = p[NmcliFieldConnectionUUID] // Fallback to UUID
			}
			break
		}
	}

	var args []string
	if existingProfile != nil && existingProfileIdentifier != "" {
		log.Printf("Existing Wi-Fi profile '%s' found for SSID '%s'. Deleting and re-adding for a clean configuration.", existingProfileIdentifier, ssid)
		
		// Attempt to delete the existing profile
		_, delErr := ConnectionDelete(existingProfileIdentifier)
		if delErr != nil {
			log.Printf("Failed to delete existing profile '%s': %v. Proceeding to add new.", existingProfileIdentifier, delErr)
			// Non-fatal, nmcli add might still work or overwrite, but good to log.
		}

		// Proceed to add as a new profile
		log.Printf("Adding new Wi-Fi profile: %s for SSID: %s, ifname: %s", profileName, ssid, ifname)
		args = []string{
			"connection", "add", "type", ConnectionTypeWifi,
			"con-name", profileName, // Use the intended profile name
			"ifname", ifname, // This sets connection.interface-name, should be "*"
			"ssid", ssid,
			"wifi-sec.key-mgmt", keyMgmtWPAPSK,
			"wifi-sec.psk", password,
		}
	} else {
		log.Printf("No existing conflicting profile found. Adding new Wi-Fi profile: %s for SSID: %s, ifname: %s", profileName, ssid, ifname)
		args = []string{
			"connection", "add", "type", ConnectionTypeWifi,
			"con-name", profileName,
			"ifname", ifname, // Should be "*"
			"ssid", ssid,
			"wifi-sec.key-mgmt", keyMgmtWPAPSK,
			"wifi-sec.psk", password,
		}
	}
	return cliInternal(args...)
}
// func AddWifiConnectionPSK(profileName, ifname, ssid, password string) (string, error) {
	// if strings.TrimSpace(profileName) == "" { return "", fmt.Errorf("profile name empty") }
	// if strings.TrimSpace(ssid) == "" { return "", fmt.Errorf("SSID empty") }
	// if strings.TrimSpace(password) == "" { return "", fmt.Errorf("password empty for WPA-PSK") }
	// if ifname == "" { ifname = "*" }
	//
	// profiles, err := GetConnectionProfilesList(false)
	// if err != nil { return "", fmt.Errorf("could not list profiles: %w", err) }
	// var existingProfile ConnectionProfile
	// for _, p := range profiles {
	// 	if p[NmcliFieldConnectionName] == profileName { existingProfile = p; break }
	// 	profileSSID := GetSSIDFromProfile(p)
	// 	if profileSSID == ssid && p[NmcliFieldConnectionType] == ConnectionTypeWifi { existingProfile = p; break }
	// }
	//
	// var args []string
	// if existingProfile != nil {
	// 	connIDToModify := existingProfile[NmcliFieldConnectionName]
	// 	if connIDToModify == "" { connIDToModify = existingProfile[NmcliFieldConnectionUUID] }
	// 	log.Printf("Modifying existing Wi-Fi profile: %s", connIDToModify)
	// 	args = []string{ "connection", "modify", connIDToModify, wifiSecKeyMgmt, keyMgmtWPAPSK, wifiSecPSK, password, "ssid", ssid, "connection.interface-name", ifname }
	// } else {
	// 	log.Printf("Adding new Wi-Fi profile: %s for SSID: %s", profileName, ssid)
	// 	args = []string{ "connection", "add", "type", ConnectionTypeWifi, "con-name", profileName, "ifname", ifname, "ssid", ssid, wifiSecKeyMgmt, keyMgmtWPAPSK, wifiSecPSK, password }
	// }
	// return cliInternal(args...)
// }

func ConnectToWifiRobustly(profileNameBase, ifname, ssid, password string, hidden bool) (string, error) {
	log.Printf("Robust connect attempt for SSID: %s", ssid)
	output, err := WifiConnect(ssid, password, hidden)
	if err != nil {
		if password != "" && (strings.Contains(err.Error(), "802-11-wireless-security.key-mgmt: property is missing") || strings.Contains(err.Error(), "secrets were required")) {
			log.Printf("Simple connect for '%s' failed (key-mgmt/secrets). Attempting explicit profile.", ssid)
			profileName := profileNameBase; if profileName == "" { profileName = ssid }
			
			profileOutput, addErr := AddWifiConnectionPSK(profileName, ifname, ssid, password)
			if addErr != nil {
				log.Printf("Failed to add/modify profile '%s' for SSID '%s': %v", profileName, ssid, addErr)
				return output, fmt.Errorf("simple connect failed (%w), and explicit profile config also failed (%v)", err, addErr)
			}
			log.Printf("Successfully added/modified profile '%s'. Output: %s. Attempting activation.", profileName, profileOutput)
			upOutput, upErr := ConnectionUp(profileName)
			if upErr != nil {
				log.Printf("Failed to bring up profile '%s': %v", profileName, upErr)
				return upOutput, fmt.Errorf("profile '%s' configured but activation failed: %w", profileName, upErr)
			}
			log.Printf("Successfully activated profile '%s'. Output: %s", profileName, upOutput)
			return upOutput, nil
		}
		return output, err
	}
	return output, nil
}


// GetSSIDFromProfile extracts the SSID from a connection profile map.
// NetworkManager might store SSID under different keys depending on context.
func GetSSIDFromProfile(profile ConnectionProfile) string {
	if profile == nil { return "" }
	ssid := profile[NmcliFieldWifiSSID]
	if ssid == "" {
		ssid = profile[eightZeroTwo11SSID]
	}
	return ssid
}
