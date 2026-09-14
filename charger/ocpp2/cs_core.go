package ocpp2

// cs_core.go implementiert alle CSMSHandler-Interfaces.
// Das CSMS empfängt diese Nachrichten VON der Charging Station.
//
// Wichtigster Unterschied zu OCPP 1.6:
//   StartTransaction + StopTransaction → TransactionEvent (Started/Updated/Ended)

import (
    "time"

    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/authorization"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/diagnostics"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/firmware"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// ── provisioning.CSMSHandler ──────────────────────────────────────────────────

// OnBootNotificationRequest wird beim Start der Charging Station aufgerufen.
func (cs *CS2) OnBootNotificationRequest(id string, req *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
    if st, err := cs.StationByID(id); err == nil {
        return st.OnBootNotification(req)
    }
    // Station noch nicht konfiguriert → Pending
    return &provisioning.BootNotificationResponse{
        CurrentTime: types.NewDateTime(time.Now()),
        Interval:    int(Timeout.Seconds()),
        Status:      provisioning.RegistrationStatusPending,
    }, nil
}

// OnHeartbeatRequest beantwortet den Heartbeat der Charging Station.
func (cs *CS2) OnHeartbeatRequest(id string, req *availability.HeartbeatRequest) (*availability.HeartbeatResponse, error) {
    return &availability.HeartbeatResponse{
        CurrentTime: types.NewDateTime(time.Now()),
    }, nil
}

// ── availability.CSMSHandler ──────────────────────────────────────────────────

// OnStatusNotificationRequest empfängt den EVSE-Status (Available/Occupied/Faulted).
// OCPP 2.0.1: Eine Nachricht pro EVSE (nicht pro Connector wie in 1.6).
func (cs *CS2) OnStatusNotificationRequest(id string, req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
    cs.mu.Lock()
    // Status für spätere EVSE-Verbindungen cachen
    if reg, ok := cs.regs[id]; ok && req != nil {
        reg.mu.Lock()
        reg.status[req.EvseId] = req
        reg.mu.Unlock()
    }
    cs.mu.Unlock()

    if st, err := cs.StationByID(id); err == nil {
        return st.OnStatusNotification(req)
    }
    return new(availability.StatusNotificationResponse), nil
}

// ── transactions.CSMSHandler ──────────────────────────────────────────────────

// OnTransactionEventRequest ist der zentrale Handler für den Ladeprozess.
//
// OCPP 2.0.1 fasst Start/Stop/Update in einer einzigen Nachricht zusammen:
//   EventType: Started  → Transaktion begonnen (war: StartTransaction)
//   EventType: Updated  → Messwerte während des Ladens (war: MeterValues)
//   EventType: Ended    → Transaktion beendet (war: StopTransaction)
//
// Außerdem enthält TransactionEvent direkt Messwerte – kein separater MeterValues
// für Transaktions-bezogene Werte mehr nötig.
func (cs *CS2) OnTransactionEventRequest(id string, req *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
    if st, err := cs.StationByID(id); err == nil {
        return st.OnTransactionEvent(req)
    }
    // Station nicht bekannt → akzeptieren um Blocker zu vermeiden
    return &transactions.TransactionEventResponse{
        IdTokenInfo: &types.IdTokenInfo{
            Status: types.AuthorizationStatusAccepted,
        },
    }, nil
}

// ── meter.CSMSHandler ─────────────────────────────────────────────────────────

// OnMeterValuesRequest empfängt Messwerte außerhalb von Transaktionen.
func (cs *CS2) OnMeterValuesRequest(id string, req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
    if st, err := cs.StationByID(id); err == nil {
        return st.OnMeterValues(req)
    }
    return new(meter.MeterValuesResponse), nil
}

// ── authorization.CSMSHandler ─────────────────────────────────────────────────

// OnAuthorizeRequest beantwortet Autorisierungsanfragen.
// Für MVP: alle IdToken akzeptieren (analog zu OCPP 1.6 Implementierung).
func (cs *CS2) OnAuthorizeRequest(id string, req *authorization.AuthorizeRequest) (*authorization.AuthorizeResponse, error) {
    cs.log.DEBUG.Printf("%s: Authorize IdToken=%s Type=%s", id, req.IdToken.IdToken, req.IdToken.Type)
    return &authorization.AuthorizeResponse{
        IdTokenInfo: types.IdTokenInfo{
            Status: types.AuthorizationStatusAccepted,
        },
    }, nil
}

// ── smartcharging.CSMSHandler ─────────────────────────────────────────────────

// OnReportChargingProfilesRequest empfängt Charging Profile Berichte (auf Anfrage).
func (cs *CS2) OnReportChargingProfilesRequest(id string, req *smartcharging.ReportChargingProfilesRequest) (*smartcharging.ReportChargingProfilesResponse, error) {
    cs.log.DEBUG.Printf("%s: ReportChargingProfiles requestId=%d", id, req.RequestId)
    return new(smartcharging.ReportChargingProfilesResponse), nil
}

// OnNotifyChargingLimitRequest empfängt Benachrichtigungen über Ladelimits.
func (cs *CS2) OnNotifyChargingLimitRequest(id string, req *smartcharging.NotifyChargingLimitRequest) (*smartcharging.NotifyChargingLimitResponse, error) {
    cs.log.DEBUG.Printf("%s: NotifyChargingLimit evseId=%d", id, req.EvseId)
    return new(smartcharging.NotifyChargingLimitResponse), nil
}

// OnNotifyEVChargingScheduleRequest: EV meldet seinen Ladeplan (ISO 15118).
func (cs *CS2) OnNotifyEVChargingScheduleRequest(id string, req *smartcharging.NotifyEVChargingScheduleRequest) (*smartcharging.NotifyEVChargingScheduleResponse, error) {
    cs.log.DEBUG.Printf("%s: NotifyEVChargingSchedule evseId=%d", id, req.EvseId)
    // VERIFY: Typ des Status-Feldes in der Response
    return &smartcharging.NotifyEVChargingScheduleResponse{
        Status: smartcharging.GenericDeviceModelStatusAccepted,
    }, nil
}

// OnNotifyEVChargingNeedsRequest: EV meldet seine Ladeanforderungen (ISO 15118).
func (cs *CS2) OnNotifyEVChargingNeedsRequest(id string, req *smartcharging.NotifyEVChargingNeedsRequest) (*smartcharging.NotifyEVChargingNeedsResponse, error) {
    cs.log.DEBUG.Printf("%s: NotifyEVChargingNeeds evseId=%d", id, req.EvseId)
    return &smartcharging.NotifyEVChargingNeedsResponse{
        Status: smartcharging.NotifyEVChargingNeedsStatusAccepted,
    }, nil
}

// OnClearedChargingLimitRequest: Charging Station meldet dass ein Limit aufgehoben wurde.
func (cs *CS2) OnClearedChargingLimitRequest(id string, req *smartcharging.ClearedChargingLimitRequest) (*smartcharging.ClearedChargingLimitResponse, error) {
    cs.log.DEBUG.Printf("%s: ClearedChargingLimit", id)
    return new(smartcharging.ClearedChargingLimitResponse), nil
}

// ── diagnostics.CSMSHandler ───────────────────────────────────────────────────

func (cs *CS2) OnNotifyEventRequest(id string, req *diagnostics.NotifyEventRequest) (*diagnostics.NotifyEventResponse, error) {
    return new(diagnostics.NotifyEventResponse), nil
}

func (cs *CS2) OnNotifyMonitoringReportRequest(id string, req *diagnostics.NotifyMonitoringReportRequest) (*diagnostics.NotifyMonitoringReportResponse, error) {
    return new(diagnostics.NotifyMonitoringReportResponse), nil
}

func (cs *CS2) OnNotifyCustomerInformationRequest(id string, req *diagnostics.NotifyCustomerInformationRequest) (*diagnostics.NotifyCustomerInformationResponse, error) {
    return new(diagnostics.NotifyCustomerInformationResponse), nil
}

func (cs *CS2) OnLogStatusNotificationRequest(id string, req *diagnostics.LogStatusNotificationRequest) (*diagnostics.LogStatusNotificationResponse, error) {
    return new(diagnostics.LogStatusNotificationResponse), nil
}

// ── firmware.CSMSHandler ──────────────────────────────────────────────────────

func (cs *CS2) OnFirmwareStatusNotificationRequest(id string, req *firmware.FirmwareStatusNotificationRequest) (*firmware.FirmwareStatusNotificationResponse, error) {
    cs.log.DEBUG.Printf("%s: FirmwareStatus: %s", id, req.Status)
    return new(firmware.FirmwareStatusNotificationResponse), nil
}

func (cs *CS2) OnPublishFirmwareStatusNotificationRequest(id string, req *firmware.PublishFirmwareStatusNotificationRequest) (*firmware.PublishFirmwareStatusNotificationResponse, error) {
    return new(firmware.PublishFirmwareStatusNotificationResponse), nil
}