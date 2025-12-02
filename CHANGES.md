# nmtui-go Changes Summary

This document outlines all the changes made to the nmtui-go project during development.

## Unified Network List

### Unified Network List

- Combined separate known networks and scanned networks into a single unified list
- Networks are now displayed together with indicators for known (★) and active (✔) status
- Simplified the UI by removing separate views for known vs. scanned networks
- Updated sorting to prioritize: Active networks → Known networks → Available networks by signal strength
- **Note**: Out-of-range known networks only display if they are currently active (not showing inactive known networks that are out of range)

## Initial Feature Implementation (Caching & Scanning Indicator)

### Caching System

- Added `cacheFile` constant (`/tmp/nmtui-cache.json`) for storing scanned networks
- Created `loadCachedNetworks()` function to load cached networks on application startup
- Created `saveCachedNetworks()` function to save networks after successful scans
- Modified `initialModel()` to load cached networks at startup for instant display

### Scanning Indicator

- Modified `headerView()` to display a spinner between "Network Manager" and "Wi-Fi:<status>" when scanning

## Bug Fixes (Network Disappearing During Scan)

### Network Persistence During Scanning

- Removed conditional hiding of network list when `isLoading=true` in the `View()` function
- Modified `wifiListLoadedMsg` handler to ignore empty scan results and keep `isScanning=true`
- Modified `knownNetworksMsg` handler to preserve cached networks during scanning state
- Removed `m.processAndSetWifiList([])` call that was clearing networks on refresh

## Code Cleanup & Quality Improvements

### Removed Unused Code

- Deleted `knownWifiApsListMsg` type
- Removed `connectionProfileToWifiAP()` function
- Removed `fetchKnownWifiApsCmd()` function

### Fixed Lint Warnings

- Removed redundant nil checks
- Eliminated redundant state condition checks in `View()` switch statement

### UI Fixes

- Fixed signal strength display from double percent signs (`%%`) to single (`%`)

## Additional Features

### Network Deduplication

- Implemented deduplication of networks by SSID, keeping the one with the strongest signal
- Modified `getAllWifiItems()` to consolidate duplicate access points
- Updated filtering logic to work with deduplicated list

### Bug Fixes

- **Fixed filtering during scanning/refreshing**: Custom filtering logic now works properly when pressing `/` during scanning or refreshing operations, instead of triggering Bubble Tea's built-in list filtering
- **Fixed disconnect dialog showing wrong network name**: Pressing 'D' to disconnect now correctly shows the actual network name instead of "<Hidden Network>" by using the active network from the list when available

## Technical Improvements

- **Unified list**: Single network list combining known and scanned networks with clear status indicators
- **Network caching**: Instant startup with previously scanned networks
- **Persistent display**: Networks remain visible during the entire scan cycle (15-second process)
- **Deduplication**: Networks with same SSID show as one entry with strongest signal
- **Clean codebase**: No lint warnings, no unused code
- **Better UX**: Scanning indicator, correct signal display, build-from-source focus
- **Custom filtering**: Real-time SSID filtering with filtered count display

All changes were validated through compilation and testing. The project now provides a robust TUI for WiFi management with caching, persistent display during scans, and clean code.
