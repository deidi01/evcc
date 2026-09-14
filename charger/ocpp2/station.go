package ocpp2

// station.go: Station = OCPP 2.0.1 "Charging Station" (≙ "Charge Point" in OCPP 1.6)
// Verwaltet Verbindungsstatus, BootNotification und EVSE-Registrierung.

import (
    "context"
    "fmt"
    "sync"
    "time"

    "github.com/evcc-io/evcc/api"
    "github.com/evcc-io/evcc/util"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// Station repräsentiert eine verbundene OCPP 2.0.1 Charging Station.
type Station struct {
    mu  sync.RWMutex
    cs  *CS2
    log *util.Logger

    id        string
    connected bool

    onceConnect sync.Once
    connectC    chan struct{} // wird geschlossen sobald BootNotification empfangen

    bootTimer         *time.Timer
    bootNotificationC chan *provisioning.BootNotificationRequest
    BootNotification  *provisioning.BootNotificationResponse // letztes akzeptiertes Result

    evses map[int]*EVSE // keyed by EVSE-ID (1-basiert)
}

// NewStation erstellt eine neue Station-Instanz.
func NewStation(log *util.Logger, cs *CS2, id string) *Station {
    return &Station{
        cs:                cs,
        log:               log,
        id:                id,
        evses:             make(map[int]*EVSE),
        connectC:          make(chan struct{}),
        bootNotificationC: make(chan *provisioning.BootNotificationRequest, 1),
    }
}

func (st *Station) ID() string {
    st.mu.RLock()
    defer st.mu.RUnlock()
    return st.id
}

// RegisterID setzt die ID nach anonymer Erstverbindung.
func (st *Station) RegisterID(id string) {
    st.mu.Lock()
    defer st.mu.Unlock()
    if st.id != "" {
        panic("ocpp2: cannot re-register station id")
    }
    st.id = id
}

// ── EVSE-Verwaltung ───────────────────────────────────────────────────────────

func (st *Station) registerEVSE(id int, evse *EVSE) error {
    st.mu.Lock()
    defer st.mu.Unlock()
    if _, ok := st.evses[id]; ok {
        return fmt.Errorf("evse already registered: %d", id)
    }
    st.evses[id] = evse
    return nil
}

func (st *Station) deregisterEVSE(id int) {
    st.mu.Lock()
    defer st.mu.Unlock()
    delete(st.evses, id)
}

func (st *Station) evseByID(id int) *EVSE {
    st.mu.RLock()
    defer st.mu.RUnlock()
    return st.evses[id]
}

// ── Verbindungsstatus ─────────────────────────────────────────────────────────

func (st *Station) Connected() bool {
    st.mu.RLock()
    defer st.mu.RUnlock()
    return st.connected
}

// HasConnected gibt einen Channel zurück, der beim ersten Connect geschlossen wird.
func (st *Station) HasConnected() <-chan struct{} {
    return st.connectC
}

func (st *Station) connect(v bool) {
    st.mu.Lock()
    defer st.mu.Unlock()
    st.connected = v
    if v {
        st.onceConnect.Do(func() { close(st.connectC) })
    } else {
        st.stopBootTimer()
    }
}

func (st *Station) stopBootTimer() {
    if st.bootTimer != nil {
        st.bootTimer.Stop()
        st.bootTimer = nil
    }
}

// onTransportConnect wird beim WebSocket-Connect aufgerufen.
// Startet einen Timer: Falls keine BootNotification kommt, verbinden wir trotzdem.
func (st *Station) onTransportConnect() {
    st.mu.Lock()
    defer st.mu.Unlock()
    st.stopBootTimer()
    st.bootTimer = time.AfterFunc(Timeout, st.onBootTimeout)

    // Proaktiv BootNotification triggern (wie in OCPP 1.6 Implementierung)
    time.AfterFunc(TriggerBootDelay, func() {
        st.mu.RLock()
        if st.bootTimer == nil || st.BootNotification != nil {
            st.mu.RUnlock()
            return
        }
        st.mu.RUnlock()
        st.log.DEBUG.Printf("proactively triggering BootNotification")
        // VERIFY: TriggerMessage API in OCPP 2.0.1
        // st.TriggerMessageRequest(0, remotecontrol.MessageTriggerBootNotification)
    })
}

func (st *Station) onBootTimeout() {
    st.mu.Lock()
    if st.bootTimer == nil {
        st.mu.Unlock()
        return
    }
    st.bootTimer = nil
    st.mu.Unlock()
    st.log.DEBUG.Printf("boot notification timeout – proceeding anyway")
    st.connect(true)
}

// ── Message Handler ───────────────────────────────────────────────────────────

// OnBootNotification verarbeitet die BootNotification der Charging Station.
func (st *Station) OnBootNotification(req *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
    st.mu.Lock()
    st.stopBootTimer()
    st.mu.Unlock()

    st.log.DEBUG.Printf("BootNotification: model=%s vendor=%s reason=%s",
        req.ChargingStation.Model, req.ChargingStation.VendorName, req.Reason)

    resp := &provisioning.BootNotificationResponse{
        CurrentTime: types.NewDateTime(time.Now()),
        Interval:    int(heartbeatInterval.Seconds()),
        Status:      provisioning.RegistrationStatusAccepted,
    }
    st.BootNotification = resp

    // An MonitorReboot-Goroutine senden (non-blocking)
    select {
    case st.bootNotificationC <- req:
    default:
    }

    st.connect(true)
    return resp, nil
}

// OnStatusNotification leitet den Status an die betroffene EVSE weiter.
func (st *Station) OnStatusNotification(req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
    st.log.DEBUG.Printf("StatusNotification: evse=%d connector=%d status=%s",
        req.EvseId, req.ConnectorId, req.ConnectorStatus)
    if evse := st.evseByID(req.EvseId); evse != nil {
        return evse.OnStatusNotification(req)
    }
    return new(availability.StatusNotificationResponse), nil
}

// OnTransactionEvent leitet das Event an die betroffene EVSE weiter.
func (st *Station) OnTransactionEvent(req *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
    evseId := 0
    if req.Evse != nil {
        evseId = req.Evse.Id
    }
    st.log.DEBUG.Printf("TransactionEvent: evse=%d type=%s txn=%s",
        evseId, req.EventType, req.TransactionInfo.TransactionId)

    if evse := st.evseByID(evseId); evse != nil {
        return evse.OnTransactionEvent(req)
    }
    return &transactions.TransactionEventResponse{
        IdTokenInfo: &types.IdTokenInfo{
            Status: types.AuthorizationStatusAccepted,
        },
    }, nil
}

// OnMeterValues leitet Messwerte an die betroffene EVSE weiter.
func (st *Station) OnMeterValues(req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
    if evse := st.evseByID(req.EvseId); evse != nil {
        return evse.OnMeterValues(req)
    }
    return new(meter.MeterValuesResponse), nil
}

// MonitorReboot startet eine Goroutine die bei Reboot die Station neu initialisiert.
func (st *Station) MonitorReboot(ctx context.Context, setup func() error) {
    // Initiale BootNotification aus dem Channel leeren
    select {
    case <-st.bootNotificationC:
    default:
    }
    go func() {
        for {
            select {
            case <-ctx.Done():
                return
            case boot := <-st.bootNotificationC:
                st.log.INFO.Printf("reboot detected (model=%s vendor=%s), re-initialising",
                    boot.ChargingStation.Model, boot.ChargingStation.VendorName)
                if err := setup(); err != nil {
                    st.log.ERROR.Printf("re-initialise after reboot failed: %v", err)
                }
            }
        }
    }()
}

// wait wartet auf einen Callback-Channel oder Timeout.
// Helper für alle synchronen Request-Methoden.
func wait(err error, rc <-chan error) error {
    if err != nil {
        return err
    }
    select {
    case err = <-rc:
    case <-time.After(Timeout):
        err = api.ErrTimeout
    }
    return err
}