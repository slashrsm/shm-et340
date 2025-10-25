// main.go - SHM to Venus OS DBus adapter with random data generation
package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/introspect"
	"github.com/godbus/dbus/v5"
	log "github.com/sirupsen/logrus"
)

// Config holds all configuration for the application
type Config struct {
	DBusName string
	LogLevel string
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

func generateRandomPhase() *singlePhase {
	L := singlePhase{}

	// Generate random voltage (220-240V range)
	L.voltage = 220 + rand.Float32()*20

	// Generate random power (-5000 to 5000W to simulate import/export)
	L.power = (rand.Float32() - 0.5) * 10000

	// Calculate current from power and voltage (I = P/V)
	L.a = L.power / L.voltage

	// Generate random energy values (accumulated kWh)
	L.forward = rand.Float64() * 10000 // 0-10000 kWh imported
	L.reverse = rand.Float64() * 10000 // 0-10000 kWh exported

	return &L
}

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

func (a *App) GenerateRandomData() {
	log.Debug("----------------------")
	log.Debug("Generating random meter data")

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

	// Generate random values for 3 phases
	L1 := generateRandomPhase()
	L2 := generateRandomPhase()
	L3 := generateRandomPhase()

	// Calculate totals
	powertot := L1.power + L2.power + L3.power
	bezugtot := L1.forward + L2.forward + L3.forward
	einsptot := L1.reverse + L2.reverse + L3.reverse
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

	log.Info(fmt.Sprintf("Random data published to D-Bus: %.1f W", powertot))
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

	log.Info("Successfully connected to dbus and registered as a meter... Starting random data generation")

	// Start timer to generate random data every 5 seconds
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				a.GenerateRandomData()
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

	// Create configuration
	config := Config{
		DBusName: "com.victronenergy.grid.cgwacs_ttyUSB0_di30_mb1",
		LogLevel: lvl,
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
