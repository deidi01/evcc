package ocpp2

// evse.go: EVSE = Electric Vehicle Supply Equipment
// OCPP 2.0.1 Äquivalent zu "Connector" in OCPP 1.6.
// Verwaltet Status, Transaktionsstatus und Messwerte einer Ladeeinheit.

import (
    "context"
    "fmt"
    "strconv"
    "strings"
    "sync"
    "time"

    "github.com/benbjohnson/clock"
    "github.com/evcc-io/evcc/api"
    "github.com/evcc-io/evcc/util"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// EVSE repräsentiert eine einzelne Ladeeinheit (EVSE) der Charging Station.
type EVSE struct {
    mu    sync.Mutex
    log   *util.Logger
    clock clock.Clock

    st *Station
    id int // EVSE-ID (1-basiert)

    // Verbindungsstatus
    status  *availability.StatusNotificationRequest
    statusC chan struct{} // geschlossen nach erstem StatusNotification

    // Transaktionsstatus (String-IDs in OCPP 2.0.1!)
    txnId   string
    idToken string

    // Messwerte
    meterUpdated  time.Time
    meterInterval time.Duration
    measurements  map[types.Measurand]types.SampledValue

    // Für automatischen Remote-Start
    remoteIdToken string
}

// NewEVSE erstellt eine EVSE und registriert sie an der Station.
func NewEVSE(ctx context.Context, log *util.Logger, id int, st *Station, idToken string, meterInterval time.Duration) (*EVSE, error) {
    evse := &EVSE{
        log:           log,
        st:            st,
        id:            id,
        clock:         clock.New(),
        statusC:       make(chan struct{}),
        measurements:  make(map[types.Measurand]types.SampledValue),
        remoteIdToken: idToken,
        meterInterval: meterInterval,
    }

    if err := st.registerEVSE(id, evse); err != nil {
        return nil, err
    }

    // EVSE beim Context-Ende deregistrieren
    go func() {
        <-ctx.Done()
        st.deregisterEVSE(evse.id)
    }()

    // Gecachten Status anwenden (falls Station schon Daten hatte)
    var hasStatus bool
    st.cs.WithEVSEStatus(st.ID(), id, func(s *availability.StatusNotificationRequest) {
        if _, err := evse.OnStatusNotification(s); err == nil {
            hasStatus = true
        }
    })

    // StatusNotification aktiv anfordern falls noch keiner da
    if !hasStatus {
        if err := st.TriggerMessageRequest(id, remotecontrol.MessageTriggerStatusNotification); err != nil {
            log.WARN.Printf("failed triggering StatusNotification for evse %d: %v", id, err)
        }
    }

    return evse, nil
}

func (evse *EVSE) ID() int { return evse.id }

// IdToken gibt den IdToken der aktuellen Transaktion zurück.
func (evse *EVSE) IdToken() string {
    evse.mu.Lock()
    defer evse.mu.Unlock()
    return evse.idToken
}

// TransactionID gibt die aktuelle Transaktions-ID zurück (leer = keine Transaktion).
// OCPP 2.0.1: Transaction-IDs sind Strings!
func (evse *EVSE) TransactionID() (string, error) {
    if !evse.st.Connected() {
        return "", api.ErrTimeout
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()
    return evse.txnId, nil
}

// Status gibt den aktuellen EVSE-Verbindungsstatus zurück.
func (evse *EVSE) Status() (availability.ConnectorStatusType, error) {
    if !evse.st.Connected() {
        return "", api.ErrTimeout
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()
    if evse.status == nil {
        return availability.ConnectorStatusUnavailable, nil
    }
    return evse.status.ConnectorStatus, nil
}

// Initialized wartet auf das erste StatusNotification.
func (evse *EVSE) Initialized() error {
    trigger := time.After(Timeout / 2)
    timeout := time.After(Timeout)
    for {
        select {
        case <-evse.statusC:
            return nil
        case <-trigger:
            _ = evse.st.TriggerMessageRequest(evse.id, remotecontrol.MessageTriggerStatusNotification)
        case <-timeout:
            return api.ErrTimeout
        }
    }
}

// NeedsAuthentication gibt an ob die EVSE auf einen IdToken wartet.
func (evse *EVSE) NeedsAuthentication() bool {
    if !evse.st.Connected() {
        return false
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()
    return evse.isWaitingForAuth()
}

func (evse *EVSE) isWaitingForAuth() bool {
    // Occupied + keine aktive Transaktion = warte auf Authentifizierung
    return evse.status != nil &&
        evse.txnId == "" &&
        evse.status.ConnectorStatus == availability.ConnectorStatusOccupied
}

func (evse *EVSE) isMeterTimeout() bool {
    return evse.clock.Since(evse.meterUpdated) > max(evse.meterInterval+10*time.Second, Timeout)
}

// ── Messwerte (api.Meter, api.PhaseCurrents, api.MeterEnergy) ────────────────

// CurrentPower implementiert api.Meter.
func (evse *EVSE) CurrentPower() (float64, error) {
    if !evse.st.Connected() {
        return 0, api.ErrTimeout
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()

    if evse.isMeterTimeout() {
        if evse.txnId != "" {
            return 0, api.ErrTimeout
        }
        return 0, nil
    }
    if m, ok := evse.measurements[types.MeasurandPowerActiveImport]; ok {
        f, err := strconv.ParseFloat(m.Value, 64)
        return scaleOCPP2(f, m.UnitOfMeasure), err
    }
    if res, found, err := evse.phaseMeasurements(types.MeasurandPowerActiveImport); found {
        return res[0] + res[1] + res[2], err
    }
    if evse.txnId == "" {
        return 0, nil
    }
    return 0, api.ErrNotAvailable
}

// TotalEnergy implementiert api.MeterEnergy.
func (evse *EVSE) TotalEnergy() (float64, error) {
    if !evse.st.Connected() {
        return 0, api.ErrTimeout
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()

    if evse.txnId != "" && evse.isMeterTimeout() {
        return 0, api.ErrTimeout
    }
    if m, ok := evse.measurements[types.MeasurandEnergyActiveImportRegister]; ok {
        f, err := strconv.ParseFloat(m.Value, 64)
        return scaleOCPP2(f, m.UnitOfMeasure) / 1e3, err // Wh → kWh
    }
    return 0, api.ErrNotAvailable
}

// Currents implementiert api.PhaseCurrents.
func (evse *EVSE) Currents() (float64, float64, float64, error) {
    if !evse.st.Connected() {
        return 0, 0, 0, api.ErrTimeout
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()

    if evse.isMeterTimeout() {
        if evse.txnId != "" {
            return 0, 0, 0, api.ErrTimeout
        }
        return 0, 0, 0, nil
    }
    if res, found, err := evse.phaseMeasurements(types.MeasurandCurrentImport); found {
        return res[0], res[1], res[2], err
    }
    return 0, 0, 0, api.ErrNotAvailable
}

// GetMaxCurrent implementiert api.CurrentGetter.
func (evse *EVSE) GetMaxCurrent() (float64, error) {
    if !evse.st.Connected() {
        return 0, api.ErrTimeout
    }
    evse.mu.Lock()
    defer evse.mu.Unlock()

    if evse.isMeterTimeout() {
        return 0, api.ErrTimeout
    }
    if m, ok := evse.measurements[types.MeasurandCurrentOffered]; ok {
        f, err := strconv.ParseFloat(m.Value, 64)
        return scaleOCPP2(f, m.UnitOfMeasure), err
    }
    return 0, api.ErrNotAvailable
}

// phaseMeasurements liest Phasenmesswerte (L1/L2/L3).
func (evse *EVSE) phaseMeasurements(measurand types.Measurand) ([3]float64, bool, error) {
    var (
        res   [3]float64
        found bool
    )
    for i := range res {
        key := types.Measurand(fmt.Sprintf("%s.L%d", measurand, i+1))
        m, ok := evse.measurements[key]
        if !ok {
            continue
        }
        found = true
        f, err := strconv.ParseFloat(m.Value, 64)
        if err != nil {
            return res, found, fmt.Errorf("invalid phase value %s: %w", key, err)
        }
        res[i] = scaleOCPP2(f, m.UnitOfMeasure)
    }
    return res, found, nil
}

// scaleOCPP2 skaliert einen Messwert gemäß OCPP 2.0.1 UnitOfMeasure.
// OCPP 2.0.1 verwendet ein Multiplier-Feld (Potenz von 10) anstelle von Präfixen.
func scaleOCPP2(f float64, unit *types.UnitOfMeasure) float64 {
    if unit == nil {
        return f
    }
    if unit.Multiplier != nil && *unit.Multiplier != 0 {
        exp := *unit.Multiplier
        result := f
        if exp > 0 {
            for i := 0; i < exp; i++ {
                result *= 10
            }
        } else {
            for i := 0; i > exp; i-- {
                result /= 10
            }
        }
        return result
    }
    // Fallback: Einheiten-Präfix (kW, mA, etc.)
    switch {
    case strings.HasPrefix(unit.Unit, "k"):
        return f * 1e3
    case strings.HasPrefix(unit.Unit, "m"):
        return f / 1e3
    default:
        return f
    }
}

// assumeMeterStopped setzt Leistung/Strom auf 0 nach Transaktionsende.
func (evse *EVSE) assumeMeterStopped() {
    evse.meterUpdated = evse.clock.Now()
    for _, measurand := range []types.Measurand{
        types.MeasurandPowerActiveImport,
        types.MeasurandCurrentImport,
    } {
        for phase := 1; phase <= 3; phase++ {
            key := types.Measurand(fmt.Sprintf("%s.L%d", measurand, phase))
            if _, ok := evse.measurements[key]; ok {
                evse.measurements[key] = types.SampledValue{Value: "0"}
            }
        }
        if _, ok := evse.measurements[measurand]; ok {
            evse.measurements[measurand] = types.SampledValue{Value: "0"}
        }
    }
}

// ── Message Handler ───────────────────────────────────────────────────────────

// OnStatusNotification verarbeitet den EVSE-Status.
func (evse *EVSE) OnStatusNotification(req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
    evse.mu.Lock()
    defer evse.mu.Unlock()

    if evse.status == nil {
        evse.status = req
        close(evse.statusC) // Initiales Signal
    } else {
        evse.status = req
    }

    // Stale-Transaktion löschen wenn EVSE wieder Available
    if req.ConnectorStatus == availability.ConnectorStatusAvailable && evse.txnId != "" {
        evse.log.DEBUG.Printf("evse %d: clearing stale transaction %s on Available", evse.id, evse.txnId)
        evse.txnId = ""
        evse.idToken = ""
        evse.assumeMeterStopped()
    }

    // Automatischer Remote-Start bei Occupied + konfiguriertem IdToken
    if evse.isWaitingForAuth() && evse.remoteIdToken != "" {
        go func(token string) {
            idToken := types.IdToken{
                IdToken: token,
                Type:    types.IdTokenTypeCentral,
            }
            if err := evse.st.RequestStartTransactionRequest(evse.id, idToken); err != nil {
                evse.log.ERROR.Printf("evse %d: RequestStartTransaction: %v", evse.id, err)
            }
        }(evse.remoteIdToken)
    }

    return new(availability.StatusNotificationResponse), nil
}

// OnTransactionEvent verarbeitet Transaktionsereignisse.
// KERNSTÜCK: ein Handler für Start, Update UND Ende einer Transaktion.
func (evse *EVSE) OnTransactionEvent(req *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
    evse.mu.Lock()
    defer evse.mu.Unlock()

    // Messwerte aus dem TransactionEvent direkt verarbeiten
    for _, mv := range req.MeterValue {
        evse.applyMeterValue(mv)
    }

    switch req.EventType {
    case transactions.TransactionEventStarted:
        evse.txnId = req.TransactionInfo.TransactionId
        if req.IdToken != nil {
            evse.idToken = req.IdToken.IdToken
        }
        evse.log.DEBUG.Printf("evse %d: transaction started: %s", evse.id, evse.txnId)

    case transactions.TransactionEventUpdated:
        // Messwerte bereits oben verarbeitet

    case transactions.TransactionEventEnded:
        evse.log.DEBUG.Printf("evse %d: transaction ended: %s", evse.id, evse.txnId)
        evse.txnId = ""
        evse.idToken = ""
        evse.assumeMeterStopped()
    }

    return &transactions.TransactionEventResponse{
        IdTokenInfo: &types.IdTokenInfo{
            Status: types.AuthorizationStatusAccepted,
        },
    }, nil
}

// OnMeterValues verarbeitet standalone MeterValues (außerhalb von Transaktionen).
func (evse *EVSE) OnMeterValues(req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
    evse.mu.Lock()
    defer evse.mu.Unlock()
    for _, mv := range req.MeterValue {
        evse.applyMeterValue(mv)
    }
    return new(meter.MeterValuesResponse), nil
}

// applyMeterValue trägt einen MeterValue in die Messwerttabelle ein.
// Muss mit evse.mu gehalten aufgerufen werden.
func (evse *EVSE) applyMeterValue(mv types.MeterValue) {
    ts := evse.clock.Now()
    if mv.Timestamp != nil {
        ts = mv.Timestamp.Time
    }
    if ts.Before(evse.meterUpdated) {
        return // Ältere Werte ignorieren
    }
    evse.meterUpdated = ts
    for _, sv := range mv.SampledValue {
        key := types.Measurand(sv.Measurand)
        if sv.Phase != "" {
            key = types.Measurand(fmt.Sprintf("%s.%s", sv.Measurand, sv.Phase))
        }
        evse.measurements[key] = sv
    }
}

// WatchDog triggert MeterValues wenn Messwerte zu alt werden.
func (evse *EVSE) WatchDog(ctx context.Context, timeout time.Duration) {
    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()
    for {
        evse.mu.Lock()
        stale := evse.clock.Since(evse.meterUpdated) > timeout
        evse.mu.Unlock()

        if stale {
            _ = evse.st.TriggerMessageRequest(evse.id, remotecontrol.MessageTriggerMeterValues)
        }

        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
        }
    }
}