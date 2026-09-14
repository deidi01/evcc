package charger

// keba-ocpp2.go: KEBA P30 X-Series Charger via OCPP 2.0.1
//
// Vorteile gegenüber OCPP 1.6:
//   ✓ TxDefaultChargingProfile: kein Race-Condition / 4kW-Limit mehr
//   ✓ Korrekte Authentifizierung via IdToken
//   ✓ Basis für Plug&Charge (ISO 15118) in späteren Versionen
//   ✓ String-basierte Transaction-IDs

import (
    "context"
    "fmt"
    "time"

    "github.com/evcc-io/evcc/api"
    "github.com/evcc-io/evcc/api/implement"
    ocpp2pkg "github.com/evcc-io/evcc/charger/ocpp2"
    "github.com/evcc-io/evcc/util"
    "github.com/evcc-io/evcc/util/sponsor"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// KebaOCPP2 implementiert api.Charger für KEBA P30 X-Series via OCPP 2.0.1.
type KebaOCPP2 struct {
    *embed
    implement.Caps
    log       *util.Logger
    cs        *ocpp2pkg.CS2
    st        *ocpp2pkg.Station
    evse      *ocpp2pkg.EVSE
    stationID string
    evseID    int
    idTag     string
    current   float64 // zuletzt gesetzter Strom in Ampere
    enabled   bool
}

func init() {
    registry.AddCtx("keba-ocpp2", NewKebaOCPP2FromConfig)
}

// NewKebaOCPP2FromConfig erzeugt einen neuen Charger aus der YAML-Konfiguration.
func NewKebaOCPP2FromConfig(ctx context.Context, other map[string]any) (api.Charger, error) {
    cc := struct {
        embed     `mapstructure:",squash"`
        StationID string
        EvseID    int
        IdTag     string
        Meter     bool
    }{
        EvseID: 1,
        Meter:  true,
    }
    if err := util.DecodeOther(other, &cc); err != nil {
        return nil, err
    }
    if cc.StationID == "" {
        return nil, fmt.Errorf("keba-ocpp2: stationid darf nicht leer sein")
    }
    return NewKebaOCPP2(ctx, cc.embed, cc.StationID, cc.EvseID, cc.IdTag, cc.Meter)
}

// NewKebaOCPP2 erzeugt eine neue Charger-Instanz.
func NewKebaOCPP2(ctx context.Context, e embed, stationID string, evseID int, idTag string, hasMeter bool) (*KebaOCPP2, error) {
    if !sponsor.IsAuthorized() {
        return nil, api.ErrSponsorRequired
    }

    log := util.NewLogger("keba-ocpp2")

    cs, err := ocpp2pkg.Instance2()
    if err != nil {
        return nil, fmt.Errorf("ocpp2 not configured: %w", err)
    }

    wb := &KebaOCPP2{
        embed:     &e,
        Caps:      implement.New(),
        log:       log,
        cs:        cs,
        stationID: stationID,
        evseID:    evseID,
        idTag:     idTag,
        current:   6, // Sicherer Startwert: 6A Minimum
    }

    // Station am CSMS registrieren
    wb.st, err = cs.RegisterStation(
        stationID,
        func() *ocpp2pkg.Station {
            return ocpp2pkg.NewStation(log, cs, stationID)
        },
        func(st *ocpp2pkg.Station) error {
            return wb.setup(ctx, st, hasMeter)
        },
    )
    if err != nil {
        return nil, err
    }

    return wb, nil
}

// setup wird nach dem ersten Connect der Charging Station aufgerufen.
func (wb *KebaOCPP2) setup(ctx context.Context, st *ocpp2pkg.Station, hasMeter bool) error {
    wb.log.DEBUG.Printf("waiting for station %s to connect...", wb.stationID)

    // Warten auf BootNotification
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-st.HasConnected():
        wb.log.INFO.Printf("station %s connected", wb.stationID)
    }

    // EVSE erstellen
    evse, err := ocpp2pkg.NewEVSE(ctx, wb.log, wb.evseID, st, wb.idTag, 30*time.Second)
    if err != nil {
        return fmt.Errorf("failed to create evse %d: %w", wb.evseID, err)
    }
    wb.evse = evse

    // Warten auf erstes StatusNotification
    if err := evse.Initialized(); err != nil {
        return fmt.Errorf("evse %d not initialized: %w", wb.evseID, err)
    }
    wb.log.INFO.Printf("evse %d initialized", wb.evseID)

    // *** KERNPUNKT: TxDefaultChargingProfile sofort setzen ***
    // Das setzt das Ladelimit persistent für alle aktuellen und zukünftigen
    // Transaktionen – kein Race-Condition-Problem mehr wie in OCPP 1.6!
    if err := wb.setTxDefaultProfile(wb.current); err != nil {
        wb.log.WARN.Printf("failed to set initial TxDefaultProfile: %v (wird bei Enable() erneut versucht)", err)
    }

    // Capabilities nach erfolgreicher Initialisierung registrieren
    if hasMeter {
        implement.Has(wb, implement.Meter(wb.evse.CurrentPower))
        implement.Has(wb, implement.MeterEnergy(wb.evse.TotalEnergy))
        implement.Has(wb, implement.PhaseCurrents(wb.evse.Currents))
    }
    implement.Has(wb, implement.CurrentGetter(wb.evse.GetMaxCurrent))
    if wb.idTag != "" {
        implement.Has(wb, implement.Identifier(wb.identify))
    }

    // Reboot-Monitor starten
    st.MonitorReboot(ctx, func() error {
        return wb.setup(ctx, st, hasMeter)
    })

    return nil
}

// ── api.Charger Interface ─────────────────────────────────────────────────────

// Status gibt den aktuellen Ladestatus zurück.
//
// OCPP 2.0.1 Mapping:
//   Available             → StatusA (kein Fahrzeug)
//   Occupied + kein Txn   → StatusB (Fahrzeug verbunden, lädt nicht)
//   Occupied + aktive Txn → StatusC (Fahrzeug lädt)
//   Faulted               → StatusF (Fehler)
func (wb *KebaOCPP2) Status() (api.ChargeStatus, error) {
    if wb.evse == nil {
        return api.StatusA, nil
    }

    s, err := wb.evse.Status()
    if err != nil {
        return api.StatusA, err
    }

    switch s {
    case availability.ConnectorStatusAvailable:
        return api.StatusA, nil
    case availability.ConnectorStatusOccupied:
        txn, err := wb.evse.TransactionID()
        if err != nil {
            return api.StatusB, err
        }
        if txn != "" {
            return api.StatusC, nil
        }
        return api.StatusB, nil
    case availability.ConnectorStatusFaulted:
        return api.StatusF, nil
    default:
        return api.StatusA, nil
    }
}

// Enabled gibt zurück ob das Laden aktiv ist.
func (wb *KebaOCPP2) Enabled() (bool, error) {
    return wb.enabled, nil
}

// Enable aktiviert oder deaktiviert das Laden.
// Via TxDefaultChargingProfile: limit=current → Laden erlaubt, limit=0 → Laden gesperrt.
func (wb *KebaOCPP2) Enable(enable bool) error {
    var amps float64
    if enable {
        amps = wb.current
    }
    if err := wb.setTxDefaultProfile(amps); err != nil {
        return err
    }
    wb.enabled = enable
    return nil
}

// MaxCurrent implementiert api.Charger.
func (wb *KebaOCPP2) MaxCurrent(current int64) error {
    return wb.MaxCurrentMillis(float64(current))
}

var _ api.ChargerEx = (*KebaOCPP2)(nil)

// MaxCurrentMillis implementiert api.ChargerEx (Komma-genaue Stromwerte).
func (wb *KebaOCPP2) MaxCurrentMillis(current float64) error {
    if wb.enabled {
        if err := wb.setTxDefaultProfile(current); err != nil {
            return err
        }
    }
    wb.current = current
    return nil
}

// ── Private Helpers ───────────────────────────────────────────────────────────

// setTxDefaultProfile sendet ein TxDefaultChargingProfile mit dem angegebenen Strom.
//
// TxDefaultProfile in OCPP 2.0.1:
//   - Gilt für ALLE aktuellen UND zukünftigen Transaktionen
//   - Muss NICHT nach TransactionStart gesendet werden (kein Race Condition!)
//   - Persistiert auf der Wallbox bis zum nächsten SetChargingProfile
//   - amps=0 → Laden effektiv gesperrt
func (wb *KebaOCPP2) setTxDefaultProfile(amps float64) error {
    if wb.st == nil {
        return fmt.Errorf("station not connected")
    }

    schedule := types.ChargingSchedule{
        Id:               1,
        ChargingRateUnit: types.ChargingRateUnitAmpere,
        ChargingSchedulePeriod: []types.ChargingSchedulePeriod{
            {
                StartPeriod: 0,
                Limit:       amps,
                // NumberPhases: optional, nil = Charger entscheidet
            },
        },
        // Kein StartSchedule / Duration → gilt ab sofort und unbegrenzt
    }

    profile := types.ChargingProfile{
        Id:                     ocpp2pkg.DefaultChargingProfileId,
        StackLevel:             ocpp2pkg.DefaultStackLevel,
        ChargingProfilePurpose: types.ChargingProfilePurposeTxDefaultProfile,
        ChargingProfileKind:    types.ChargingProfileKindAbsolute,
        ChargingSchedule:       []types.ChargingSchedule{schedule},
    }

    wb.log.DEBUG.Printf("SetChargingProfile: evse=%d amps=%.1f (TxDefault)", wb.evseID, amps)
    return wb.st.SetChargingProfileRequest(wb.evseID, profile)
}

// identify implementiert api.Identifier (RFID-Erkennung via IdToken).
func (wb *KebaOCPP2) identify() ([]string, error) {
    if wb.evse == nil {
        return nil, nil
    }
    token := wb.evse.IdToken()
    if token == "" {
        return nil, nil
    }
    return []string{token}, nil
}