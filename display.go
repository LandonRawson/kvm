package kvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jetkvm/kvm/internal/sync"

	"github.com/prometheus/common/version"
)

var (
	backlightState = 0 // 0 - NORMAL, 1 - DIMMED, 2 - OFF
)

var (
	dimTicker         *time.Ticker
	offTicker         *time.Ticker
	routeDecisionLock sync.Mutex
	lastRouteDecision string
	lastNetworkUsable *bool
)

const (
	backlightControlClass string = "/sys/class/backlight/backlight/brightness"
)

func hasUsableNetworkAddress() bool {
	if networkManager == nil {
		return false
	}

	if networkManager.IsOnline() {
		return true
	}

	if networkManager.IPv4Ready() || networkManager.IPv6Ready() {
		return true
	}

	if isUsableIPAddress(networkManager.IPv4String()) || isUsableIPAddress(networkManager.IPv6String()) {
		return true
	}

	return false
}

func isUsableIPAddress(ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}

	// IPv6 addresses may carry an interface zone suffix (e.g. %eth0).
	if zoneIndex := strings.Index(ip, "%"); zoneIndex >= 0 {
		ip = ip[:zoneIndex]
	}

	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}

	if parsed.IsUnspecified() || parsed.IsLoopback() {
		return false
	}

	return true
}

func logDisplayRouteDecision(reason, target string) {
	isOnline := false
	ipv4Ready := false
	ipv6Ready := false
	ipv4 := ""
	ipv6 := ""

	if networkManager != nil {
		isOnline = networkManager.IsOnline()
		ipv4Ready = networkManager.IPv4Ready()
		ipv6Ready = networkManager.IPv6Ready()
		ipv4 = networkManager.IPv4String()
		ipv6 = networkManager.IPv6String()
	}

	hasUsable := hasUsableNetworkAddress()
	signature := fmt.Sprintf("reason=%s target=%s usable=%t online=%t ipv4Ready=%t ipv6Ready=%t ipv4=%s ipv6=%s",
		reason, target, hasUsable, isOnline, ipv4Ready, ipv6Ready, ipv4, ipv6)

	routeDecisionLock.Lock()
	if signature == lastRouteDecision {
		routeDecisionLock.Unlock()
		return
	}
	lastRouteDecision = signature
	routeDecisionLock.Unlock()

	logger.Debug().
		Str("reason", reason).
		Str("target", target).
		Bool("hasUsableNetworkAddress", hasUsable).
		Bool("isOnline", isOnline).
		Bool("ipv4Ready", ipv4Ready).
		Bool("ipv6Ready", ipv6Ready).
		Str("ipv4", ipv4).
		Str("ipv6", ipv6).
		Msg("display route decision")
}

func switchToMainScreen() {
	if !hasUsableNetworkAddress() {
		logDisplayRouteDecision("switchToMainScreen", "no_network_screen")
		nativeInstance.SwitchToScreenIfDifferent("no_network_screen")
		return
	}

	logDisplayRouteDecision("switchToMainScreen", "home_screen")
	nativeInstance.SwitchToScreenIfDifferent("home_screen")
}

func updateDisplayUsbState() {
	// USB state is intentionally hidden on the Home Screen.
}

func readSoCTemperatureC() (float64, error) {
	zonePaths, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(zonePaths) == 0 {
		return 0, errors.New("no thermal zones found")
	}

	for _, tempPath := range zonePaths {
		tempRaw, readErr := os.ReadFile(tempPath)
		if readErr != nil {
			continue
		}

		tempVal, parseErr := strconv.ParseFloat(strings.TrimSpace(string(tempRaw)), 64)
		if parseErr != nil || tempVal <= 0 {
			continue
		}

		if tempVal > 1000 {
			tempVal = tempVal / 1000.0
		}

		return tempVal, nil
	}

	return 0, errors.New("failed to read thermal zone temperature")
}

func readCPUStatSample() (uint64, uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, err
	}

	line := strings.SplitN(string(data), "\n", 2)[0]
	parts := strings.Fields(line)
	if len(parts) < 5 || parts[0] != "cpu" {
		return 0, 0, errors.New("invalid /proc/stat cpu line")
	}

	var total uint64
	for i := 1; i < len(parts); i++ {
		v, parseErr := strconv.ParseUint(parts[i], 10, 64)
		if parseErr != nil {
			return 0, 0, parseErr
		}
		total += v
	}

	idle, err := strconv.ParseUint(parts[4], 10, 64)
	if err != nil {
		return 0, 0, err
	}

	return total, idle, nil
}

var (
	lastCPUTotal uint64
	lastCPUIdle  uint64
	hasCPUSample bool
)

func readCPUUtilizationPercent() (float64, error) {
	total, idle, err := readCPUStatSample()
	if err != nil {
		return 0, err
	}

	if !hasCPUSample {
		hasCPUSample = true
		lastCPUTotal = total
		lastCPUIdle = idle
		return 0, errors.New("initial cpu sample")
	}

	deltaTotal := total - lastCPUTotal
	deltaIdle := idle - lastCPUIdle

	lastCPUTotal = total
	lastCPUIdle = idle

	if deltaTotal == 0 {
		return 0, errors.New("cpu delta is zero")
	}

	busy := deltaTotal - deltaIdle
	return (float64(busy) * 100.0) / float64(deltaTotal), nil
}

func readRAMUtilizationPercent() (float64, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	var memTotalKB uint64
	var memAvailableKB uint64

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, parseErr := strconv.ParseUint(fields[1], 10, 64)
				if parseErr == nil {
					memTotalKB = v
				}
			}
		}
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, parseErr := strconv.ParseUint(fields[1], 10, 64)
				if parseErr == nil {
					memAvailableKB = v
				}
			}
		}
	}

	if memTotalKB == 0 || memAvailableKB > memTotalKB {
		return 0, errors.New("invalid meminfo values")
	}

	usedKB := memTotalKB - memAvailableKB
	return (float64(usedKB) * 100.0) / float64(memTotalKB), nil
}

func updateDisplaySystemMetrics() {
	tempText := "--"
	cpuText := "--"
	ramText := "--"

	if tempC, err := readSoCTemperatureC(); err == nil {
		tempText = fmt.Sprintf("%.1fC", tempC)
	}

	if cpuUsage, err := readCPUUtilizationPercent(); err == nil {
		cpuText = fmt.Sprintf("%.1f%%", cpuUsage)
	}

	if ramUsage, err := readRAMUtilizationPercent(); err == nil {
		ramText = fmt.Sprintf("%.1f%%", ramUsage)
	}

	nativeInstance.UpdateLabelIfChanged("home_info_soc_temp", fmt.Sprintf("Temp: %s  CPU: %s", tempText, cpuText))
	nativeInstance.UpdateLabelIfChanged("home_info_ram_usage", fmt.Sprintf("Ram: %s", ramText))
}

func startDisplaySystemMetricsTicker() {
	metricsTicker := time.NewTicker(5 * time.Second)
	reconcileTicker := time.NewTicker(1 * time.Second)

	go func() {
		updateDisplaySystemMetrics()
		requestDisplayUpdate(false, "periodic_main_screen_reconcile")
		for {
			select {
			case <-metricsTicker.C:
				updateDisplaySystemMetrics()
				requestDisplayUpdate(false, "periodic_main_screen_reconcile")
			case <-reconcileTicker.C:
				requestDisplayUpdate(false, "periodic_main_screen_reconcile")
			}
		}
	}()
}

func updateDisplay() {
	if networkManager != nil {
		ipv4 := networkManager.IPv4String()
		if ipv4 == "" {
			ipv4 = "Waiting for IP..."
		}
		nativeInstance.UpdateLabelIfChanged("home_info_ipv4_addr", ipv4)
		nativeInstance.UpdateLabelAndChangeVisibility("home_info_ipv6_addr", networkManager.IPv6String())
	}

	_, _ = nativeInstance.UIObjHide("menu_btn_network")
	_, _ = nativeInstance.UIObjHide("menu_btn_access")

	switch config.NetworkConfig.DHCPClient.String {
	case "jetdhcpc":
		nativeInstance.UpdateLabelIfChanged("dhcp_client_change_label", "Change to udhcpc")
	case "udhcpc":
		nativeInstance.UpdateLabelIfChanged("dhcp_client_change_label", "Change to JetKVM")
	}

	updateDisplayUsbState()

	if lastVideoState.Ready {
		nativeInstance.UpdateLabelIfChanged("hdmi_status_label", "Connected")
		_, _ = nativeInstance.UIObjAddState("hdmi_status_label", "LV_STATE_CHECKED")
	} else {
		nativeInstance.UpdateLabelIfChanged("hdmi_status_label", "Disconnected")
		_, _ = nativeInstance.UIObjClearState("hdmi_status_label", "LV_STATE_CHECKED")
	}
	nativeInstance.UpdateLabelIfChanged("cloud_status_label", fmt.Sprintf("%d active", actionSessions))

	hasUsable := hasUsableNetworkAddress()

	if hasUsable {
		logDisplayRouteDecision("updateDisplay", "home_screen")
		nativeInstance.UISetVar("main_screen", "home_screen")
		nativeInstance.SwitchToScreenIf("home_screen", []string{"no_network_screen", "boot_screen"})
	} else {
		logDisplayRouteDecision("updateDisplay", "no_network_screen")
		nativeInstance.UISetVar("main_screen", "no_network_screen")
		nativeInstance.SwitchToScreenIf("no_network_screen", []string{"home_screen", "boot_screen"})
	}

	if lastNetworkUsable == nil || *lastNetworkUsable != hasUsable {
		target := "no_network_screen"
		if hasUsable {
			target = "home_screen"
		}

		logger.Debug().
			Bool("previousHasUsableNetworkAddress", lastNetworkUsable != nil && *lastNetworkUsable).
			Bool("hasUsableNetworkAddress", hasUsable).
			Str("target", target).
			Msg("forcing main screen switch after network usability transition")

		nativeInstance.SwitchToScreenIfDifferent(target)

		stateCopy := hasUsable
		lastNetworkUsable = &stateCopy
	}

	if cloudConnectionState == CloudConnectionStateNotConfigured {
		_, _ = nativeInstance.UIObjHide("cloud_status_icon")
	} else {
		_, _ = nativeInstance.UIObjShow("cloud_status_icon")
	}

	switch cloudConnectionState {
	case CloudConnectionStateDisconnected:
		_, _ = nativeInstance.UIObjSetImageSrc("cloud_status_icon", "cloud_disconnected")
		stopCloudBlink()
	case CloudConnectionStateConnecting:
		_, _ = nativeInstance.UIObjSetImageSrc("cloud_status_icon", "cloud")
		restartCloudBlink()
	case CloudConnectionStateConnected:
		_, _ = nativeInstance.UIObjSetImageSrc("cloud_status_icon", "cloud")
		stopCloudBlink()
	}
}

const (
	cloudBlinkInterval = 2 * time.Second
	cloudBlinkDuration = 1 * time.Second
)

var (
	cloudBlinkTicker *time.Ticker
	cloudBlinkCancel context.CancelFunc
	cloudBlinkLock   = sync.Mutex{}
)

func doCloudBlink(ctx context.Context) {
	for range cloudBlinkTicker.C {
		if cloudConnectionState != CloudConnectionStateConnecting {
			continue
		}

		_, _ = nativeInstance.UIObjFadeOut("ui_Home_Header_Cloud_Status_Icon", uint32(cloudBlinkDuration.Milliseconds()))

		select {
		case <-ctx.Done():
			return
		case <-time.After(cloudBlinkDuration):
		}

		_, _ = nativeInstance.UIObjFadeIn("ui_Home_Header_Cloud_Status_Icon", uint32(cloudBlinkDuration.Milliseconds()))

		select {
		case <-ctx.Done():
			return
		case <-time.After(cloudBlinkDuration):
		}
	}
}

func restartCloudBlink() {
	stopCloudBlink()
	startCloudBlink()
}

func startCloudBlink() {
	cloudBlinkLock.Lock()
	defer cloudBlinkLock.Unlock()

	if cloudBlinkTicker == nil {
		cloudBlinkTicker = time.NewTicker(cloudBlinkInterval)
	} else {
		cloudBlinkTicker.Reset(cloudBlinkInterval)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cloudBlinkCancel = cancel

	go doCloudBlink(ctx)
}

func stopCloudBlink() {
	cloudBlinkLock.Lock()
	defer cloudBlinkLock.Unlock()

	if cloudBlinkCancel != nil {
		cloudBlinkCancel()
		cloudBlinkCancel = nil
	}

	if cloudBlinkTicker != nil {
		cloudBlinkTicker.Stop()
	}
}

var (
	displayInited     = false
	displayUpdateLock = sync.Mutex{}
	waitDisplayUpdate = sync.Mutex{}
)

func requestDisplayUpdate(shouldWakeDisplay bool, reason string) {
	displayUpdateLock.Lock()
	defer displayUpdateLock.Unlock()

	if !displayInited {
		displayLogger.Info().Msg("display not inited, skipping updates")
		return
	}
	go func() {
		if shouldWakeDisplay {
			wakeDisplay(false, reason)
		}
		displayLogger.Debug().Msg("display updating")
		// TODO: only run once regardless how many pending updates
		updateDisplay()
	}()
}

func waitCtrlAndRequestDisplayUpdate(shouldWakeDisplay bool, reason string) {
	waitDisplayUpdate.Lock()
	defer waitDisplayUpdate.Unlock()

	requestDisplayUpdate(shouldWakeDisplay, reason)
}

func updateStaticContents() {
	//contents that never change

	// get cpu info
	if cpuInfo, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		// get the line starting with "Serial"
		for line := range strings.SplitSeq(string(cpuInfo), "\n") {
			if strings.HasPrefix(line, "Serial") {
				serial := strings.SplitN(line, ":", 2)[1]
				nativeInstance.UpdateLabelAndChangeVisibility("cpu_serial", strings.TrimSpace(serial))
				break
			}
		}
	}

	// get kernel version
	if kernelVersion, err := os.ReadFile("/proc/version"); err == nil {
		kernelVersion := strings.TrimPrefix(string(kernelVersion), "Linux version ")
		kernelVersion = strings.SplitN(kernelVersion, " ", 2)[0]
		nativeInstance.UpdateLabelAndChangeVisibility("kernel_version", kernelVersion)
	}

	nativeInstance.UpdateLabelAndChangeVisibility("build_branch", version.Branch)
	nativeInstance.UpdateLabelAndChangeVisibility("build_date", version.BuildDate)
	nativeInstance.UpdateLabelAndChangeVisibility("golang_version", version.GoVersion)

	// nativeInstance.UpdateLabelAndChangeVisibility("boot_screen_device_id", GetDeviceID())
}

// configureDisplayOnNativeRestart is called when the native process restarts
// it ensures the display is configured correctly after the restart
func configureDisplayOnNativeRestart() {
	displayLogger.Info().Msg("native restarted, configuring display")
	updateStaticContents()
	requestDisplayUpdate(true, "native_restart")
}

// setDisplayBrightness sets /sys/class/backlight/backlight/brightness to alter
// the backlight brightness of the JetKVM hardware's display.
func setDisplayBrightness(brightness int, reason string) error {
	// NOTE: The actual maximum value for this is 255, but out-of-the-box, the value is set to 64.
	// The maximum set here is set to 100 to reduce the risk of drawing too much power (and besides, 255 is very bright!).
	if brightness > 100 || brightness < 0 {
		return errors.New("brightness value out of bounds, must be between 0 and 100")
	}

	// Check the display backlight class is available
	if _, err := os.Stat(backlightControlClass); errors.Is(err, os.ErrNotExist) {
		return errors.New("brightness value cannot be set, possibly not running on JetKVM hardware")
	}

	// Set the value
	bs := []byte(strconv.Itoa(brightness))
	err := os.WriteFile(backlightControlClass, bs, 0644)
	if err != nil {
		return err
	}

	displayLogger.Info().Int("brightness", brightness).Str("reason", reason).Msg("set brightness")
	return nil
}

// tick_displayDim() is called when when dim ticker expires, it simply reduces the brightness
// of the display by half of the max brightness.
func tick_displayDim() {
	err := setDisplayBrightness(config.DisplayMaxBrightness/2, "tick_display_dim")
	if err != nil {
		displayLogger.Warn().Err(err).Msg("failed to dim display")
	}

	dimTicker.Stop()

	backlightState = 1
}

// tick_displayOff() is called when the off ticker expires, it turns off the display
// by setting the brightness to zero.
func tick_displayOff() {
	err := setDisplayBrightness(0, "tick_display_off")
	if err != nil {
		displayLogger.Warn().Err(err).Msg("failed to turn off display")
	}

	offTicker.Stop()

	backlightState = 2
}

// wakeDisplay sets the display brightness back to config.DisplayMaxBrightness and stores the time the display
// last woke, ready for displayTimeoutTick to put the display back in the dim/off states.
// Set force to true to skip the backlight state check, this should be done if altering the tickers.
func wakeDisplay(force bool, reason string) {
	if backlightState == 0 && !force {
		return
	}

	// Don't try to wake up if the display is turned off.
	if config.DisplayMaxBrightness == 0 {
		return
	}

	if reason == "" {
		reason = "wake_display"
	}

	err := setDisplayBrightness(config.DisplayMaxBrightness, reason)
	if err != nil {
		displayLogger.Warn().Err(err).Msg("failed to wake display")
	}

	if config.DisplayDimAfterSec != 0 && dimTicker != nil {
		dimTicker.Reset(time.Duration(config.DisplayDimAfterSec) * time.Second)
	}

	if config.DisplayOffAfterSec != 0 && offTicker != nil {
		offTicker.Reset(time.Duration(config.DisplayOffAfterSec) * time.Second)
	}
	backlightState = 0
}

// startBacklightTickers starts the two tickers for dimming and switching off the display
// if they're not already set. This is done separately to the init routine as the "never dim"
// option has the value set to zero, but time.NewTicker only accept positive values.
func startBacklightTickers() {
	// Don't start the tickers if the display is switched off.
	// Set the display to off if that's the case.
	if config.DisplayMaxBrightness == 0 {
		_ = setDisplayBrightness(0, "display_disabled")
		return
	}

	// Stop existing tickers to prevent multiple active instances on repeated calls
	if dimTicker != nil {
		dimTicker.Stop()
	}

	if offTicker != nil {
		offTicker.Stop()
	}

	if config.DisplayDimAfterSec != 0 {
		displayLogger.Info().Msg("dim_ticker has started")
		dimTicker = time.NewTicker(time.Duration(config.DisplayDimAfterSec) * time.Second)

		go func() {
			for range dimTicker.C {
				tick_displayDim()
			}
		}()
	}

	if config.DisplayOffAfterSec != 0 {
		displayLogger.Info().Msg("off_ticker has started")
		offTicker = time.NewTicker(time.Duration(config.DisplayOffAfterSec) * time.Second)

		go func() {
			for range offTicker.C {
				tick_displayOff()
			}
		}()
	}
}

func initDisplay() {
	go func() {
		displayLogger.Info().Msg("setting initial display contents")
		time.Sleep(500 * time.Millisecond)

		// Enable display updates first so network-driven routing is not blocked
		// if static content updates are slow.
		displayInited = true
		startDisplaySystemMetricsTicker()
		displayLogger.Info().Msg("display inited")
		startBacklightTickers()
		requestDisplayUpdate(true, "init_display")

		updateStaticContents()
		updateDisplayUsbState()
	}()
}
