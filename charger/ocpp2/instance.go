package ocpp2

import (
    "errors"
    "fmt"
    "net/http"
    "net/url"
    "strings"
    "sync"
    "time"

    "github.com/evcc-io/evcc/util"
    ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/authorization"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/diagnostics"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/firmware"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
    "github.com/lorenzodonini/ocpp-go/ocppj"
    "github.com/lorenzodonini/ocpp-go/ws"
)

// Config hält die Konfiguration des OCPP 2.0.1 Servers.
type Config struct {
    Port int `json:"port"`
}

var (
    instance2    *CS2
    started2     func() error // memoized: läuft genau einmal
    port2        = 8888       // OCPP 2.0.1 auf eigenem Port (1.6 nutzt 8887)
    boundPort2   int
    externalUrl2 string
)

// Port2 gibt den TCP-Port zurück, auf dem der OCPP 2.0.1 CSMS lauscht.
func Port2() int { return boundPort2 }

// ExternalUrl2 gibt die WebSocket-URL des OCPP 2.0.1 CSMS zurück.
func ExternalUrl2() string {
    if externalUrl2 == "" {
        return ""
    }
    u, err := url.Parse(externalUrl2)
    if err != nil {
        return ""
    }
    u.Scheme = "ws"
    u.Host = fmt.Sprintf("%s:%d", strings.Split(u.Host, ":")[0], port2)
    return u.String()
}

// NewServer2 baut den OCPP 2.0.1 CSMS – startet ihn aber noch nicht.
// Wird analog zu ocpp.NewServer aus OCPP 1.6 aufgerufen.
func NewServer2(cfg Config, networkExternalUrl string) {
    port2 = cfg.Port
    externalUrl2 = networkExternalUrl

    log := util.NewLogger("ocpp2")

    // Eigenen WebSocket-Server erstellen (wie in OCPP 1.6)
    server := ws.NewServer()
    server.SetCheckOriginHandler(func(r *http.Request) bool { return true })
    timeouts := ws.NewServerTimeoutConfig()
    timeouts.PingWait = pingWait
    server.SetTimeoutConfig(timeouts)

    dispatcher := ocppj.NewDefaultServerDispatcher(ocppj.NewFIFOQueueMap(0))

    // CSMS mit eigenem Server/Dispatcher erstellen
    // VERIFY: ocpp2.NewCSMS nimmt (ws.Server, ocppj.ServerDispatcher) als Parameter
    csms := ocpp2.NewCSMS(server, dispatcher)

    inst := &CS2{
        CSMS:       csms,
        log:        log,
        regs:       make(map[string]*registration2),
        server:     server,
        dispatcher: dispatcher,
    }
    inst.txnId.Store(time.Now().UTC().Unix())

    // Alle Handler-Interfaces verdrahten.
    // CS2 implementiert alle nötigen CSMSHandler-Interfaces.
    csms.SetProvisioningHandler(inst)
    csms.SetAvailabilityHandler(inst)
    csms.SetTransactionsHandler(inst)
    csms.SetMeterHandler(inst)
    csms.SetAuthorizationHandler(inst)
    csms.SetSmartChargingHandler(inst)
    csms.SetDiagnosticsHandler(inst)
    csms.SetFirmwareHandler(inst)

    // Verbindungs-Handler
    csms.SetNewChargingStationHandler(inst.NewChargingStation)
    csms.SetChargingStationDisconnectedHandler(inst.ChargingStationDisconnected)

    started2 = sync.OnceValue(inst.listen)
    instance2 = inst
}

// listen startet den CSMS und blockiert bis der Port gebunden ist.
func (cs *CS2) listen() error {
    cs.dispatcher.SetTimeout(Timeout)
    go cs.errorHandler(cs.Errors())
    go cs.CSMS.Start(port2, "/{ws}")

    tick := time.NewTicker(10 * time.Millisecond)
    defer tick.Stop()
    timeout := time.After(10 * time.Second)

    for cs.server.Addr() == nil {
        select {
        case <-tick.C:
        case <-timeout:
            return errors.New("ocpp2: timeout waiting for server to bind")
        }
    }
    boundPort2 = cs.server.Addr().Port
    cs.log.INFO.Printf("OCPP 2.0.1 CSMS listening on port %d", boundPort2)
    return nil
}

// Instance2 gibt den OCPP 2.0.1 CSMS zurück und startet ihn beim ersten Aufruf.
func Instance2() (*CS2, error) {
    if instance2 == nil {
        return nil, errors.New("ocpp2 not configured")
    }
    return instance2, started2()
}

// Compile-time checks: CS2 muss alle CSMSHandler-Interfaces implementieren.
var (
    _ provisioning.CSMSHandler  = (*CS2)(nil)
    _ availability.CSMSHandler  = (*CS2)(nil)
    _ transactions.CSMSHandler  = (*CS2)(nil)
    _ meter.CSMSHandler         = (*CS2)(nil)
    _ authorization.CSMSHandler = (*CS2)(nil)
    _ smartcharging.CSMSHandler = (*CS2)(nil)
    _ diagnostics.CSMSHandler   = (*CS2)(nil)
    _ firmware.CSMSHandler      = (*CS2)(nil)
)