// main.go - SHM to Venus OS DBus adapter with Shelly meter Modbus reading
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goburrow/modbus"
	"github.com/godbus/dbus/introspect"
	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"
)

// Config holds all configuration for the application
type Config struct {
	DBusName        string
	LogLevel        string
	ConsumptionIP   string
	SolarIP         string
	PollingInterval time.Duration
}

// App represents the main application
type App struct {
	config     Config
	dbusConn   *dbus.Conn
	values     map[int]map[objectpath]dbus.Variant
	mu         sync.RWMutex
	shutdownCh chan struct{}
}

type singlePhase struct {
	voltage float32 // Volts: 230,0
	a       float32 // Amps: 8,3
	power   float32 // Watts: 1909
	forward float64 // kWh, purchased power
	reverse float64 // kWh, sold power
}

// meterData holds data read from a Shelly meter
type meterData struct {
	totalPower         float32
	totalForward       float64
	totalReverse       float64
	phaseVoltages      []float32
	phaseCurrents      []float32
	phasePowers        []float32
	phaseForwardEnergy []float64
	phaseReverseEnergy []float64
}

// readMeterData reads data from a Shelly meter via Modbus
func readMeterData(ip string) (*meterData, error) {
	// Connect to Modbus TCP
	handler := modbus.NewTCPClientHandler(fmt.Sprintf("%s:502", ip))
	handler.Timeout = 1 * time.Second
	handler.SlaveId = 1

	err := handler.Connect()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %v", ip, err)
	}
	defer handler.Close()

	client := modbus.NewClient(handler)
	data := &meterData{}

	// Read input registers (Shelly uses input registers)
	// Total power (31013)
	results, err := client.ReadInputRegisters(1013, 2)
	if err != nil {
		log.Warnf("Failed to read power from %s: %v", ip, err)
	} else {
		data.totalPower = float32(int32((uint32(results[0])<<16)|uint32(results[1]))) / 1.0
	}

	// Total forward energy (31162)
	results, err = client.ReadInputRegisters(1162, 2)
	if err != nil {
		log.Warnf("Failed to read forward energy from %s: %v", ip, err)
	} else {
		data.totalForward = float64(int32((uint32(results[0])<<16)|uint32(results[1]))) / 1000.0
	}

	// Total reverse energy (31164)
	results, err = client.ReadInputRegisters(1164, 2)
	if err != nil {
		log.Warnf("Failed to read reverse energy from %s: %v", ip, err)
	} else {
		data.totalReverse = float64(int32((uint32(results[0])<<16)|uint32(results[1]))) / 1000.0
	}

	// Initialize phase arrays
	data.phaseVoltages = make([]float32, 3)
	data.phaseCurrents = make([]float32, 3)
	data.phasePowers = make([]float32, 3)
	data.phaseForwardEnergy = make([]float64, 3)
	data.phaseReverseEnergy = make([]float64, 3)

	// Read data for each phase
	for phase := 0; phase < 3; phase++ {
		emOffset := phase * 20
		dataOffset := phase * 20

		// Phase voltage
		results, err = client.ReadInputRegisters(uint16(1020+emOffset), 1)
		if err == nil {
			data.phaseVoltages[phase] = float32(results[0]) / 1.0
		}

		// Phase current
		results, err = client.ReadInputRegisters(uint16(1022+emOffset), 1)
		if err == nil {
			data.phaseCurrents[phase] = float32(results[0]) / 100.0 // Shelly returns current in 0.01A units
		}

		// Phase power
		results, err = client.ReadInputRegisters(uint16(1024+emOffset), 2)
		if err == nil {
			data.phasePowers[phase] = float32(int32((uint32(results[0])<<16)|uint32(results[1]))) / 1.0
		}

		// Phase forward energy
		results, err = client.ReadInputRegisters(uint16(1182+dataOffset), 2)
		if err == nil {
			data.phaseForwardEnergy[phase] = float64(int32((uint32(results[0])<<16)|uint32(results[1]))) / 1000.0
		}

		// Phase reverse energy
		results, err = client.ReadInputRegisters(uint16(1184+dataOffset), 2)
		if err == nil {
			data.phaseReverseEnergy[phase] = float64(int32((uint32(results[0])<<16)|uint32(results[1]))) / 1000.0
		}
	}

	return data, nil
}

// calculateNetData calculates net grid values from consumption and solar meter data
func calculateNetData(consumption, solar *meterData) (*meterData, error) {
	if consumption == nil || solar == nil {
		return nil, fmt.Errorf("meter data is nil")
	}

	net := &meterData{}

	// Net power: consumption - solar (positive = importing, negative = exporting)
	net.totalPower = consumption.totalPower - solar.totalPower

	// For energy, we keep consumption and solar separate since they're accumulated meters
	// The forward/reverse logic might need adjustment based on the actual meter configuration
	net.totalForward = consumption.totalForward
	net.totalReverse = solar.totalForward // Solar generation is "delivered" energy

	// Initialize phase arrays
	net.phaseVoltages = make([]float32, 3)
	net.phaseCurrents = make([]float32, 3)
	net.phasePowers = make([]float32, 3)
	net.phaseForwardEnergy = make([]float64, 3)
	net.phaseReverseEnergy = make([]float64, 3)

	// Calculate net values per phase
	net.phaseVoltages[0] = consumption.phaseVoltages[0]
	net.phasePowers[0] = consumption.phasePowers[0] - solar.phasePowers[1]
	net.phaseCurrents[0] = consumption.phaseCurrents[0] - solar.phaseCurrents[1]
	net.phaseForwardEnergy[0] = consumption.phaseForwardEnergy[0] + solar.phaseReverseEnergy[1]
	net.phaseReverseEnergy[0] = consumption.phaseReverseEnergy[0] + solar.phaseForwardEnergy[1]

	net.phaseVoltages[1] = consumption.phaseVoltages[1]
	net.phasePowers[1] = consumption.phasePowers[1] - solar.phasePowers[0]
	net.phaseCurrents[1] = consumption.phaseCurrents[1] - solar.phaseCurrents[0]
	net.phaseForwardEnergy[1] = consumption.phaseForwardEnergy[1] + solar.phaseReverseEnergy[0]
	net.phaseReverseEnergy[1] = consumption.phaseReverseEnergy[1] + solar.phaseForwardEnergy[0]

	net.phaseVoltages[2] = consumption.phaseVoltages[2]
	net.phasePowers[2] = consumption.phasePowers[2] - solar.phasePowers[2]
	net.phaseCurrents[2] = consumption.phaseCurrents[2] - solar.phaseCurrents[2]
	net.phaseForwardEnergy[2] = consumption.phaseForwardEnergy[2] + solar.phaseReverseEnergy[2]
	net.phaseReverseEnergy[2] = consumption.phaseReverseEnergy[2] + solar.phaseForwardEnergy[2]

	return net, nil
}

const intro = `
<node>
   <interface name="com.victronenergy.BusItem">
    <signal name="PropertiesChanged">
      <arg type="a{sv}" name="properties" />
    </signal>
    <method name="SetValue">
      <arg direction="in"  type="v" name="value" />
      <arg direction="out" type="i" />
    </method>
    <method name="GetText">
      <arg direction="out" type="s" />
    </method>
    <method name="GetValue">
      <arg direction="out" type="v" />
    </method>
    <method name="GetItems">
      <arg direction="out" type="a{sa{sv}}" name="values"/>
    </method>
	</interface>` + introspect.IntrospectDataString + `</node> `

type objectpath string

func (f objectpath) GetValue() (dbus.Variant, *dbus.Error) {
	log.Debug("GetValue() called for ", f)
	app := GetApp()
	if app == nil {
		return dbus.Variant{}, dbus.NewError("com.victronenergy.BusItem.Error", []interface{}{"Application not initialized"})
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	log.Debug("...returning ", app.values[0][f])
	return app.values[0][f], nil
}

func (f objectpath) GetText() (string, *dbus.Error) {
	log.Debug("GetText() called for ", f)
	app := GetApp()
	if app == nil {
		return "", dbus.NewError("com.victronenergy.BusItem.Error", []interface{}{"Application not initialized"})
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	log.Debug("...returning ", app.values[1][f])
	return strings.Trim(app.values[1][f].String(), "\""), nil
}

func (f objectpath) SetValue(value dbus.Variant) (int32, *dbus.Error) {
	log.Debug("SetValue() called for ", f, " with value ", value)
	app := GetApp()
	if app == nil {
		return 0, dbus.NewError("com.victronenergy.BusItem.Error", []interface{}{"Application not initialized"})
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	app.values[0][f] = value
	return 0, nil
}

var globalApp *App

func GetApp() *App {
	return globalApp
}

func SetApp(app *App) {
	globalApp = app
}

func init() {
	lvl, ok := os.LookupEnv("LOG_LEVEL")
	if !ok {
		lvl = "info"
	}

	ll, err := log.ParseLevel(lvl)
	if err != nil {
		ll = log.DebugLevel
	}

	log.SetLevel(ll)
}

func (a *App) ReadMeterData() {
	log.Debug("----------------------")
	log.Debug("Reading meter data from Shelly devices")

	changedItems := make(map[string]map[string]dbus.Variant)

	update := func(path, unit string, value float64, precision int) {
		a.mu.Lock()
		defer a.mu.Unlock()

		formatString := fmt.Sprintf("%%.%df%%s", precision)
		textValue := fmt.Sprintf(formatString, value, unit)

		currentValue, valueExists := a.values[0][objectpath(path)]

		// Only update and add to batch if the value has actually changed
		if !valueExists || currentValue.Value() != value {
			a.values[0][objectpath(path)] = dbus.MakeVariant(value)
			a.values[1][objectpath(path)] = dbus.MakeVariant(textValue)

			// Add the changed properties to our batch map.
			changedItems[path] = map[string]dbus.Variant{
				"Value": dbus.MakeVariant(value),
				"Text":  dbus.MakeVariant(textValue),
			}
		}
	}

	// Read data from both meters
	consumptionData, consumptionErr := readMeterData(a.config.ConsumptionIP)
	solarData, solarErr := readMeterData(a.config.SolarIP)

	// If we can only read one meter, use it directly
	var netData *meterData
	if consumptionErr != nil && solarErr != nil {
		log.Warn("Failed to read from both meters - skipping data update")
		return
	} else if consumptionErr != nil {
		log.Warnf("Failed to read consumption meter: %v, using solar data only", consumptionErr)
		netData = solarData
	} else if solarErr != nil {
		log.Warnf("Failed to read solar meter: %v, using consumption data only", solarErr)
		netData = consumptionData
	} else {
		// Both meters read successfully, calculate net values
		netData, _ = calculateNetData(consumptionData, solarData)
	}

	// Convert to singlePhase structs for compatibility with existing code
	L1 := &singlePhase{
		voltage: netData.phaseVoltages[0],
		a:       netData.phaseCurrents[0],
		power:   netData.phasePowers[0],
		forward: netData.phaseForwardEnergy[0],
		reverse: netData.phaseReverseEnergy[0],
	}

	L2 := &singlePhase{
		voltage: netData.phaseVoltages[1],
		a:       netData.phaseCurrents[1],
		power:   netData.phasePowers[1],
		forward: netData.phaseForwardEnergy[1],
		reverse: netData.phaseReverseEnergy[1],
	}

	L3 := &singlePhase{
		voltage: netData.phaseVoltages[2],
		a:       netData.phaseCurrents[2],
		power:   netData.phasePowers[2],
		forward: netData.phaseForwardEnergy[2],
		reverse: netData.phaseReverseEnergy[2],
	}

	// Calculate totals
	powertot := netData.totalPower
	bezugtot := netData.totalForward
	einsptot := netData.totalReverse
	totalCurrent := L1.a + L2.a + L3.a
	totalVoltage := (L1.voltage + L2.voltage + L3.voltage) / 3.0

	if log.IsLevelEnabled(log.DebugLevel) {
		PrintPhaseTable(L1, L2, L3)
	}

	// Update totals
	update("/Ac/Power", "W", float64(powertot), 1)
	update("/Ac/Energy/Reverse", "kWh", einsptot, 2)
	update("/Ac/Energy/Forward", "kWh", bezugtot, 2)
	update("/Ac/Current", "A", float64(totalCurrent), 2)
	update("/Ac/Voltage", "V", float64(totalVoltage), 2)

	// Update L1 values
	update("/Ac/L1/Power", "W", float64(L1.power), 1)
	update("/Ac/L1/Voltage", "V", float64(L1.voltage), 2)
	update("/Ac/L1/Current", "A", float64(L1.a), 2)
	update("/Ac/L1/Energy/Forward", "kWh", L1.forward, 2)
	update("/Ac/L1/Energy/Reverse", "kWh", L1.reverse, 2)

	// Update L2 values
	update("/Ac/L2/Power", "W", float64(L2.power), 1)
	update("/Ac/L2/Voltage", "V", float64(L2.voltage), 2)
	update("/Ac/L2/Current", "A", float64(L2.a), 2)
	update("/Ac/L2/Energy/Forward", "kWh", L2.forward, 2)
	update("/Ac/L2/Energy/Reverse", "kWh", L2.reverse, 2)

	// Update L3 values
	update("/Ac/L3/Power", "W", float64(L3.power), 1)
	update("/Ac/L3/Voltage", "V", float64(L3.voltage), 2)
	update("/Ac/L3/Current", "A", float64(L3.a), 2)
	update("/Ac/L3/Energy/Forward", "kWh", L3.forward, 2)
	update("/Ac/L3/Energy/Reverse", "kWh", L3.reverse, 2)

	// finally, post the updates
	a.emitItemsChanged(changedItems)

	log.Info(fmt.Sprintf("Meter data published to D-Bus: %.1f W (consumption: %s, solar: %s)", powertot, a.config.ConsumptionIP, a.config.SolarIP))
}

// NewApp creates a new application instance
func NewApp(config Config) (*App, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to system bus: %w", err)
	}

	app := &App{
		config:     config,
		dbusConn:   conn,
		values:     make(map[int]map[objectpath]dbus.Variant),
		shutdownCh: make(chan struct{}),
	}

	// Initialize the values maps
	app.values[0] = make(map[objectpath]dbus.Variant) // For VALUE variant
	app.values[1] = make(map[objectpath]dbus.Variant) // For STRING variant

	return app, nil
}

// Run starts the application
func (a *App) Run() error {
	// Initialize DBus values
	a.InitializeValues()

	// Register DBus paths
	if err := a.RegisterDBusPaths(); err != nil {
		return fmt.Errorf("failed to register DBus paths: %w", err)
	}

	log.Info("Successfully connected to dbus and registered as a meter... Starting meter data reading")

	// Start timer to read meter data at configured interval
	ticker := time.NewTicker(a.config.PollingInterval)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				a.ReadMeterData()
			case <-a.shutdownCh:
				return
			}
		}
	}()

	// Wait for shutdown signal
	<-a.shutdownCh
	return nil
}

// Shutdown gracefully stops the application
func (a *App) Shutdown() {
	close(a.shutdownCh)
	if a.dbusConn != nil {
		a.dbusConn.Close()
	}
}

func PrintPhaseTable(L1, L2, L3 *singlePhase) {
	log.Println("+-----+-------------+---------------+---------------+")
	log.Println("|value|   L1 \t|     L2  \t|   L3  \t|")
	log.Println("+-----+-------------+---------------+---------------+")
	log.Printf("|  V  | %8.2f \t| %8.2f \t| %8.2f \t|", L1.voltage, L2.voltage, L3.voltage)
	log.Printf("|  A  | %8.2f \t| %8.2f \t| %8.2f \t|", L1.a, L2.a, L3.a)
	log.Printf("|  W  | %8.2f \t| %8.2f \t| %8.2f \t|", L1.power, L2.power, L3.power)
	log.Printf("| kWh | %8.2f \t| %8.2f \t| %8.2f \t|", L1.forward, L2.forward, L3.forward)
	log.Printf("| kWh | %8.2f \t| %8.2f \t| %8.2f \t|", L1.reverse, L2.reverse, L3.reverse)
	log.Println("+-----+-------------+---------------+---------------+")
}

func (a *App) RegisterDBusPaths() error {
	paths := []dbus.ObjectPath{
		// Basic Paths whch never change
		"/Connected", "/CustomName", "/DeviceInstance", "/DeviceType",
		"/ErrorCode", "/FirmwareVersion", "/Mgmt/Connection", "/Mgmt/ProcessName",
		"/Mgmt/ProcessVersion", "/ProductName", "/Serial",
		// Updating Paths, which change every time the meter sends a packet
		"/Ac/L1/Power", "/Ac/L2/Power", "/Ac/L3/Power",
		"/Ac/L1/Voltage", "/Ac/L2/Voltage", "/Ac/L3/Voltage",
		"/Ac/L1/Current", "/Ac/L2/Current", "/Ac/L3/Current",
		"/Ac/L1/Energy/Forward", "/Ac/L2/Energy/Forward", "/Ac/L3/Energy/Forward",
		"/Ac/L1/Energy/Reverse", "/Ac/L2/Energy/Reverse", "/Ac/L3/Energy/Reverse",
		"/Ac/Current", "/Ac/Voltage", "/Ac/Power", "/Ac/Energy/Forward", "/Ac/Energy/Reverse",
	}

	a.dbusConn.Export(a, "/", "com.victronenergy.BusItem")

	a.dbusConn.Export(introspect.Introspectable(intro), "/", "org.freedesktop.DBus.Introspectable")

	for _, p := range paths {
		log.Debug("Exporting dbus path: ", p)
		a.dbusConn.Export(objectpath(p), p, "com.victronenergy.BusItem")
	}

	// only after all paths are exported, request the name
	log.Infof("All paths exported. Requesting name %s on D-Bus...", a.config.DBusName)
	reply, err := a.dbusConn.RequestName(a.config.DBusName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return fmt.Errorf("failed to request DBus name: %w", err)
	}

	if reply != dbus.RequestNameReplyPrimaryOwner {
		return fmt.Errorf("name %s already taken on dbus", a.config.DBusName)
	}

	log.Info("Successfully acquired D-Bus name.")
	return nil
}

func (a *App) emitItemsChanged(items map[string]map[string]dbus.Variant) {
	if len(items) > 0 {
		a.dbusConn.Emit("/", "com.victronenergy.BusItem.ItemsChanged", items)
	}
}

// InitializeValues sets up the initial DBus values
func (a *App) InitializeValues() {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Basic device information
	a.values[0]["/Connected"] = dbus.MakeVariant(1)
	a.values[1]["/Connected"] = dbus.MakeVariant("1")

	a.values[0]["/CustomName"] = dbus.MakeVariant("Grid meter")
	a.values[1]["/CustomName"] = dbus.MakeVariant("Grid meter")

	a.values[0]["/DeviceInstance"] = dbus.MakeVariant(30)
	a.values[1]["/DeviceInstance"] = dbus.MakeVariant("30")

	a.values[0]["/DeviceType"] = dbus.MakeVariant(71)
	a.values[1]["/DeviceType"] = dbus.MakeVariant("71")

	a.values[0]["/ErrorCode"] = dbus.MakeVariantWithSignature(0, dbus.SignatureOf(123))
	a.values[1]["/ErrorCode"] = dbus.MakeVariant("0")

	a.values[0]["/FirmwareVersion"] = dbus.MakeVariant(2)
	a.values[1]["/FirmwareVersion"] = dbus.MakeVariant("2")

	a.values[0]["/Mgmt/Connection"] = dbus.MakeVariant("/dev/ttyUSB0")
	a.values[1]["/Mgmt/Connection"] = dbus.MakeVariant("/dev/ttyUSB0")

	a.values[0]["/Mgmt/ProcessName"] = dbus.MakeVariant("/opt/color-control/dbus-cgwacs/dbus-cgwacs")
	a.values[1]["/Mgmt/ProcessName"] = dbus.MakeVariant("/opt/color-control/dbus-cgwacs/dbus-cgwacs")

	a.values[0]["/Mgmt/ProcessVersion"] = dbus.MakeVariant("1.8.0")
	a.values[1]["/Mgmt/ProcessVersion"] = dbus.MakeVariant("1.8.0")

	a.values[0]["/ProductName"] = dbus.MakeVariant("Grid meter")
	a.values[1]["/ProductName"] = dbus.MakeVariant("Grid meter")

	a.values[0]["/Serial"] = dbus.MakeVariant("BP98305081235")
	a.values[1]["/Serial"] = dbus.MakeVariant("BP98305081235")

	// Initialize power values
	a.values[0]["/Ac/L1/Power"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/L1/Power"] = dbus.MakeVariant("1 W")
	a.values[0]["/Ac/L2/Power"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/L2/Power"] = dbus.MakeVariant("1 W")
	a.values[0]["/Ac/L3/Power"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/L3/Power"] = dbus.MakeVariant("1 W")

	// Initialize voltage values
	a.values[0]["/Ac/L1/Voltage"] = dbus.MakeVariant(230)
	a.values[1]["/Ac/L1/Voltage"] = dbus.MakeVariant("230 V")
	a.values[0]["/Ac/L2/Voltage"] = dbus.MakeVariant(230)
	a.values[1]["/Ac/L2/Voltage"] = dbus.MakeVariant("230 V")
	a.values[0]["/Ac/L3/Voltage"] = dbus.MakeVariant(230)
	a.values[1]["/Ac/L3/Voltage"] = dbus.MakeVariant("230 V")

	// Initialize current values
	a.values[0]["/Ac/L1/Current"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/L1/Current"] = dbus.MakeVariant("1 A")
	a.values[0]["/Ac/L2/Current"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/L2/Current"] = dbus.MakeVariant("1 A")
	a.values[0]["/Ac/L3/Current"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/L3/Current"] = dbus.MakeVariant("1 A")

	// Initialize energy values
	a.values[0]["/Ac/L1/Energy/Forward"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/L1/Energy/Forward"] = dbus.MakeVariant("0 kWh")
	a.values[0]["/Ac/L2/Energy/Forward"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/L2/Energy/Forward"] = dbus.MakeVariant("0 kWh")
	a.values[0]["/Ac/L3/Energy/Forward"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/L3/Energy/Forward"] = dbus.MakeVariant("0 kWh")

	a.values[0]["/Ac/L1/Energy/Reverse"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/L1/Energy/Reverse"] = dbus.MakeVariant("0 kWh")
	a.values[0]["/Ac/L2/Energy/Reverse"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/L2/Energy/Reverse"] = dbus.MakeVariant("0 kWh")
	a.values[0]["/Ac/L3/Energy/Reverse"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/L3/Energy/Reverse"] = dbus.MakeVariant("0 kWh")

	// Initialize total values
	a.values[0]["/Ac/Current"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/Current"] = dbus.MakeVariant("1 A")
	a.values[0]["/Ac/Voltage"] = dbus.MakeVariant(230)
	a.values[1]["/Ac/Voltage"] = dbus.MakeVariant("230 V")
	a.values[0]["/Ac/Power"] = dbus.MakeVariant(1.0)
	a.values[1]["/Ac/Power"] = dbus.MakeVariant("1 W")
	a.values[0]["/Ac/Energy/Forward"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/Energy/Forward"] = dbus.MakeVariant("0 kWh")
	a.values[0]["/Ac/Energy/Reverse"] = dbus.MakeVariant(0.0)
	a.values[1]["/Ac/Energy/Reverse"] = dbus.MakeVariant("0 kWh")
}

func (a *App) GetItems() (map[string]map[string]dbus.Variant, *dbus.Error) {
	log.Debug("GetItems() called on root")
	a.mu.RLock()
	defer a.mu.RUnlock()

	items := make(map[string]map[string]dbus.Variant)

	// Iterate over all known paths
	for path, valueVariant := range a.values[0] {
		pathStr := string(path)
		textVariant, ok := a.values[1][path]
		if !ok {
			// This case should ideally not happen if InitializeValues is correct
			textVariant = dbus.MakeVariant("")
		}

		items[pathStr] = map[string]dbus.Variant{
			"Value": valueVariant,
			"Text":  textVariant,
		}
	}

	return items, nil
}

func main() {
	// Configure logging
	lvl, ok := os.LookupEnv("LOG_LEVEL")
	if !ok {
		lvl = "info"
	}

	ll, err := log.ParseLevel(lvl)
	if err != nil {
		ll = log.DebugLevel
	}
	log.SetLevel(ll)

	// Configure meter IPs
	consumptionIP := os.Getenv("CONSUMPTION_METER_IP")
	if consumptionIP == "" {
		consumptionIP = "192.168.11.190"
	}

	solarIP := os.Getenv("SOLAR_METER_IP")
	if solarIP == "" {
		solarIP = "192.168.11.52"
	}

	// Configure polling interval
	pollingStr := os.Getenv("POLLING_INTERVAL_SECONDS")
	pollingInterval := 1 * time.Second
	if pollingStr != "" {
		if seconds, err := strconv.Atoi(pollingStr); err == nil {
			pollingInterval = time.Duration(seconds) * time.Second
		}
	}

	// Create configuration
	config := Config{
		DBusName:        "com.victronenergy.grid.cgwacs_ttyUSB0_di30_mb1",
		LogLevel:        lvl,
		ConsumptionIP:   consumptionIP,
		SolarIP:         solarIP,
		PollingInterval: pollingInterval,
	}

	// Create and run application
	app, err := NewApp(config)
	if err != nil {
		log.Fatalf("Failed to create application: %v", err)
	}

	// Set the global app instance
	SetApp(app)

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Info("Received shutdown signal, cleaning up...")
		app.Shutdown()
		os.Exit(0)
	}()

	// Run the application
	if err := app.Run(); err != nil {
		log.Fatalf("Application error: %v", err)
	}
}
