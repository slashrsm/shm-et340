package main

import (
	"encoding/binary"
	"fmt"
	"math"
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
	config          Config
	dbusConn        *dbus.Conn
	values          map[int]map[objectpath]dbus.Variant
	mu              sync.RWMutex
	shutdownCh      chan struct{}
	consumptionConn *modbus.TCPClientHandler
	solarConn       *modbus.TCPClientHandler
}

// meterData holds data read from a Shelly meter
type meterData struct {
	totalPower         float32
	totalForward       float32
	totalReverse       float32
	phaseVoltages      []float32
	phaseCurrents      []float32
	phasePowers        []float32
	phaseForwardEnergy []float32
	phaseReverseEnergy []float32
}

func (a *App) ensureConnection(ip string, connection *modbus.TCPClientHandler) (*modbus.TCPClientHandler, error) {
	if connection != nil {
		// Test if connection is still alive
		if _, testErr := modbus.NewClient(connection).ReadInputRegisters(1, 1); testErr == nil {
			return nil, nil // Connection is still good
		}
		// Connection failed, close it and reconnect
		connection.Close()
		connection = nil
	}

	// Establish new connection
	handler := modbus.NewTCPClientHandler(fmt.Sprintf("%s:502", ip))
	handler.Timeout = 2 * time.Second
	handler.SlaveId = 1

	if err := handler.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to meter %s: %v", ip, err)
	}

	connection = handler
	if log.IsLevelEnabled(log.DebugLevel) {
		log.Debugf("Established persistent connection to meter: %s", ip)
	}

	return handler, nil
}

func (a *App) readMeterData(ip string, isConsumption bool) (*meterData, error) {
	// Ensure connections are established and healthy
	var newConn *modbus.TCPClientHandler
	var err error
	if isConsumption {
		newConn, err = a.ensureConnection(ip, a.consumptionConn)
	} else {
		newConn, err = a.ensureConnection(ip, a.solarConn)
	}

	if err != nil {
		return nil, err
	}
	if newConn != nil {
		if isConsumption {
			a.consumptionConn = newConn
		} else {
			a.solarConn = newConn
		}
	}

	// Get the appropriate client
	var client modbus.Client
	if isConsumption {
		client = modbus.NewClient(a.consumptionConn)
	} else {
		client = modbus.NewClient(a.solarConn)
	}

	data := &meterData{}

	// Read input registers (Shelly uses input registers)
	// Total power (31013)
	results, err := client.ReadInputRegisters(1013, 2)
	if err != nil {
		// Connection might be dead, mark for reconnection
		if isConsumption {
			a.consumptionConn.Close()
			a.consumptionConn = nil
		} else {
			a.solarConn.Close()
			a.solarConn = nil
		}
		log.Warnf("Failed to read power from %s: %v", ip, err)
	} else {
		data.totalPower = math.Float32frombits(
			binary.BigEndian.Uint32(
				[]byte{
					results[2], // C
					results[3], // D
					results[0], // A
					results[1], // B
				},
			),
		)
	}

	// Total forward energy (31162)
	results, err = client.ReadInputRegisters(1162, 2)
	if err == nil {
		data.totalForward = math.Float32frombits(
			binary.BigEndian.Uint32(
				[]byte{
					results[2], // C
					results[3], // D
					results[0], // A
					results[1], // B
				},
			),
		)
	}

	results, err = client.ReadInputRegisters(1164, 2)
	if err == nil {
		data.totalReverse = math.Float32frombits(
			binary.BigEndian.Uint32(
				[]byte{
					results[2], // C
					results[3], // D
					results[0], // A
					results[1], // B
				},
			),
		)
	}

	// Initialize phase arrays
	data.phaseVoltages = make([]float32, 3)
	data.phaseCurrents = make([]float32, 3)
	data.phasePowers = make([]float32, 3)
	data.phaseForwardEnergy = make([]float32, 3)
	data.phaseReverseEnergy = make([]float32, 3)

	for phase := 0; phase < 3; phase++ {
		emOffset := phase * 20
		dataOffset := phase * 20

		// Phase voltage
		results, err = client.ReadInputRegisters(uint16(1020+emOffset), 2)
		if err == nil {
			data.phaseVoltages[phase] = math.Float32frombits(
				binary.BigEndian.Uint32(
					[]byte{
						results[2], // C
						results[3], // D
						results[0], // A
						results[1], // B
					},
				),
			)
		}

		// Phase current
		results, err = client.ReadInputRegisters(uint16(1022+emOffset), 2)
		if err == nil {
			data.phaseCurrents[phase] = math.Float32frombits(
				binary.BigEndian.Uint32(
					[]byte{
						results[2], // C
						results[3], // D
						results[0], // A
						results[1], // B
					},
				),
			)
		}

		// Phase power
		results, err = client.ReadInputRegisters(uint16(1024+emOffset), 2)
		if err == nil {
			data.phasePowers[phase] = math.Float32frombits(
				binary.BigEndian.Uint32(
					[]byte{
						results[2], // C
						results[3], // D
						results[0], // A
						results[1], // B
					},
				),
			)
		}

		// Phase forward energy
		results, err = client.ReadInputRegisters(uint16(1182+dataOffset), 2)
		if err == nil {
			data.phaseForwardEnergy[phase] = math.Float32frombits(
				binary.BigEndian.Uint32(
					[]byte{
						results[2], // C
						results[3], // D
						results[0], // A
						results[1], // B
					},
				),
			)
		}

		// Phase reverse energy
		results, err = client.ReadInputRegisters(uint16(1184+dataOffset), 2)
		if err == nil {
			data.phaseReverseEnergy[phase] = math.Float32frombits(
				binary.BigEndian.Uint32(
					[]byte{
						results[2], // C
						results[3], // D
						results[0], // A
						results[1], // B
					},
				),
			)
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
	net.phaseForwardEnergy = make([]float32, 3)
	net.phaseReverseEnergy = make([]float32, 3)

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

	// Read data from both meters using persistent connections
	consumptionData, consumptionErr := a.readMeterData(a.config.ConsumptionIP, true)
	solarData, solarErr := a.readMeterData(a.config.SolarIP, false)

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

	if log.IsLevelEnabled(log.DebugLevel) {
		PrintPhaseTable(netData, consumptionData, solarData)
	}

	// Update totals
	update("/Ac/Power", "W", float64(netData.totalPower), 1)
	update("/Ac/Energy/Reverse", "kWh", float64(netData.totalReverse), 2)
	update("/Ac/Energy/Forward", "kWh", float64(netData.totalForward), 2)
	update("/Ac/Current", "A", float64(netData.phaseCurrents[0]+netData.phaseCurrents[1]+netData.phaseCurrents[2]), 2)
	update("/Ac/Voltage", "V", float64((netData.phaseVoltages[0]+netData.phaseVoltages[1]+netData.phaseVoltages[2])/3.0), 2)

	// Update L1 values
	update("/Ac/L1/Power", "W", float64(netData.phasePowers[0]), 1)
	update("/Ac/L1/Voltage", "V", float64(netData.phaseVoltages[0]), 2)
	update("/Ac/L1/Current", "A", float64(netData.phaseCurrents[0]), 2)
	update("/Ac/L1/Energy/Forward", "kWh", float64(netData.phaseForwardEnergy[0]), 2)
	update("/Ac/L1/Energy/Reverse", "kWh", float64(netData.phaseReverseEnergy[0]), 2)

	// Update L2 values
	update("/Ac/L2/Power", "W", float64(netData.phasePowers[1]), 1)
	update("/Ac/L2/Voltage", "V", float64(netData.phaseVoltages[1]), 2)
	update("/Ac/L2/Current", "A", float64(netData.phaseCurrents[1]), 2)
	update("/Ac/L2/Energy/Forward", "kWh", float64(netData.phaseForwardEnergy[1]), 2)
	update("/Ac/L2/Energy/Reverse", "kWh", float64(netData.phaseReverseEnergy[1]), 2)

	// Update L3 values
	update("/Ac/L3/Power", "W", float64(netData.phasePowers[2]), 1)
	update("/Ac/L3/Voltage", "V", float64(netData.phaseVoltages[2]), 2)
	update("/Ac/L3/Current", "A", float64(netData.phaseCurrents[2]), 2)
	update("/Ac/L3/Energy/Forward", "kWh", float64(netData.phaseForwardEnergy[2]), 2)
	update("/Ac/L3/Energy/Reverse", "kWh", float64(netData.phaseReverseEnergy[2]), 2)

	// finally, post the updates
	a.emitItemsChanged(changedItems)

	log.Info(fmt.Sprintf("Meter data published to D-Bus: %.1f W", netData.totalPower))
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

	// Close persistent Modbus connections
	if a.consumptionConn != nil {
		a.consumptionConn.Close()
	}
	if a.solarConn != nil {
		a.solarConn.Close()
	}
}

func PrintPhaseTable(net, consumption, solar *meterData) {
	log.Println("+-----+-------------+---------------+---------------+")
	log.Println("|value|   L1 \t|     L2  \t|   L3  \t|")
	log.Println("+-----+-------------+---------------+---------------+")
	log.Printf("|  V  | %8.2f \t| %8.2f \t| %8.2f \t|", net.phaseVoltages[0], net.phaseVoltages[1], net.phaseVoltages[2])
	log.Printf("|  s  | %8.2f \t| %8.2f \t| %8.2f \t|", solar.phaseVoltages[0], solar.phaseVoltages[1], solar.phaseVoltages[2])
	log.Printf("|  c  | %8.2f \t| %8.2f \t| %8.2f \t|", consumption.phaseVoltages[0], consumption.phaseVoltages[1], consumption.phaseVoltages[2])
	log.Println("+-----+-------------+---------------+---------------+")
	log.Printf("|  A  | %8.2f \t| %8.2f \t| %8.2f \t|", net.phaseCurrents[0], net.phaseCurrents[1], net.phaseCurrents[2])
	log.Printf("|  s  | %8.2f \t| %8.2f \t| %8.2f \t|", solar.phaseCurrents[0], solar.phaseCurrents[1], solar.phaseCurrents[2])
	log.Printf("|  c  | %8.2f \t| %8.2f \t| %8.2f \t|", consumption.phaseCurrents[0], consumption.phaseCurrents[1], consumption.phaseCurrents[2])
	log.Println("+-----+-------------+---------------+---------------+")
	log.Printf("|  W  | %8.2f \t| %8.2f \t| %8.2f \t|", net.phasePowers[0], net.phasePowers[1], net.phasePowers[2])
	log.Printf("|  s  | %8.2f \t| %8.2f \t| %8.2f \t|", solar.phasePowers[0], solar.phasePowers[1], solar.phasePowers[2])
	log.Printf("|  c  | %8.2f \t| %8.2f \t| %8.2f \t|", consumption.phasePowers[0], consumption.phasePowers[1], consumption.phasePowers[2])
	log.Println("+-----+-------------+---------------+---------------+")
	log.Printf("| kWh | %8.2f \t| %8.2f \t| %8.2f \t|", net.phaseForwardEnergy[0], net.phaseForwardEnergy[1], net.phaseForwardEnergy[2])
	log.Printf("|  s  | %8.2f \t| %8.2f \t| %8.2f \t|", solar.phaseForwardEnergy[0], solar.phaseForwardEnergy[1], solar.phaseForwardEnergy[2])
	log.Printf("|  c  | %8.2f \t| %8.2f \t| %8.2f \t|", consumption.phaseForwardEnergy[0], consumption.phaseForwardEnergy[1], consumption.phaseForwardEnergy[2])
	log.Println("+-----+-------------+---------------+---------------+")
	log.Printf("| kWh | %8.2f \t| %8.2f \t| %8.2f \t|", net.phaseReverseEnergy[0], net.phaseReverseEnergy[1], net.phaseReverseEnergy[2])
	log.Printf("|  s  | %8.2f \t| %8.2f \t| %8.2f \t|", solar.phaseReverseEnergy[0], solar.phaseReverseEnergy[1], solar.phaseReverseEnergy[2])
	log.Printf("|  c  | %8.2f \t| %8.2f \t| %8.2f \t|", consumption.phaseReverseEnergy[0], consumption.phaseReverseEnergy[1], consumption.phaseReverseEnergy[2])
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
		if seconds, err := strconv.ParseFloat(pollingStr, 32); err == nil {
			pollingInterval = time.Duration(int(seconds*1000)) * time.Millisecond
		}
	}
	log.Debugf("Polling interval: %d", pollingInterval.Milliseconds())

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
