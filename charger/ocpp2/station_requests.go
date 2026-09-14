package ocpp2

// station_requests.go: Requests die der CSMS an die Charging Station sendet.
// Analog zu charger/ocpp/cp_requests.go in OCPP 1.6.
//
// WICHTIGSTE ÄNDERUNG: SetChargingProfile mit TxDefaultProfile
// → kein Race-Condition-Problem mehr wie in OCPP 1.6

import (
    "errors"

    "github.com/evcc-io/evcc/api"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
    "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// SetChargingProfileRequest sendet ein ChargingProfile an die Station.
//
// evseId=0: gilt für die gesamte Station
// evseId>0: gilt für eine bestimmte EVSE
//
// Mit TxDefaultProfile: persistent, gilt für alle aktuellen und zukünftigen
// Transaktionen → LÖST das 4kW-Problem aus OCPP 1.6!
func (st *Station) SetChargingProfileRequest(evseId int, profile types.ChargingProfile) error {
    if !st.Connected() {
        return api.ErrTimeout
    }
    rc := make(chan error, 1)
    err := st.cs.SetChargingProfile(
        st.id,
        func(resp *smartcharging.SetChargingProfileResponse, err error) {
            if err == nil && resp != nil && resp.Status != smartcharging.ChargingProfileStatusAccepted {
                err = errors.New(string(resp.Status))
            }
            rc <- err
        },
        evseId,
        profile,
    )
    return wait(err, rc)
}

// RequestStartTransactionRequest fordert die Station auf, eine Transaktion zu starten.
// OCPP 2.0.1: ersetzt RemoteStartTransaction aus OCPP 1.6.
// IdToken: Identifikation des Nutzers (RFID, App-Token, etc.)
func (st *Station) RequestStartTransactionRequest(evseId int, idToken types.IdToken) error {
    if !st.Connected() {
        return api.ErrTimeout
    }
    rc := make(chan error, 1)
    err := st.cs.RequestStartTransaction(
        st.id,
        func(resp *remotecontrol.RequestStartTransactionResponse, err error) {
            if err == nil && resp != nil && resp.Status != remotecontrol.RequestStartStopStatusAccepted {
                err = errors.New(string(resp.Status))
            }
            rc <- err
        },
        idToken,
        func(req *remotecontrol.RequestStartTransactionRequest) {
            req.EvseId = &evseId
        },
    )
    return wait(err, rc)
}

// RequestStopTransactionRequest fordert die Station auf, eine Transaktion zu beenden.
// OCPP 2.0.1: Transaction-ID ist jetzt ein STRING (nicht mehr int wie in 1.6!).
func (st *Station) RequestStopTransactionRequest(transactionId string) error {
    if !st.Connected() {
        return api.ErrTimeout
    }
    rc := make(chan error, 1)
    err := st.cs.RequestStopTransaction(
        st.id,
        func(resp *remotecontrol.RequestStopTransactionResponse, err error) {
            if err == nil && resp != nil && resp.Status != remotecontrol.RequestStartStopStatusAccepted {
                err = errors.New(string(resp.Status))
            }
            rc <- err
        },
        transactionId,
    )
    return wait(err, rc)
}

// ChangeAvailabilityRequest ändert den Betriebsstatus einer EVSE.
// Operative: EVSE verfügbar | Inoperative: EVSE gesperrt
func (st *Station) ChangeAvailabilityRequest(evseId int, status availability.OperationalStatusType) error {
    if !st.Connected() {
        return api.ErrTimeout
    }
    rc := make(chan error, 1)
    err := st.cs.ChangeAvailability(
        st.id,
        func(resp *availability.ChangeAvailabilityResponse, err error) {
            if err == nil && resp != nil &&
                resp.Status != availability.ChangeAvailabilityStatusAccepted &&
                resp.Status != availability.ChangeAvailabilityStatusScheduled {
                err = errors.New(string(resp.Status))
            }
            rc <- err
        },
        evseId,
        status,
    )
    return wait(err, rc)
}

// TriggerMessageRequest fordert die Station auf, eine bestimmte Nachricht zu senden.
func (st *Station) TriggerMessageRequest(evseId int, message remotecontrol.MessageTriggerType) error {
    if !st.Connected() {
        return api.ErrTimeout
    }
    rc := make(chan error, 1)
    err := st.cs.TriggerMessage(
        st.id,
        func(resp *remotecontrol.TriggerMessageResponse, err error) {
            if err == nil && resp != nil && resp.Status != remotecontrol.TriggerMessageStatusAccepted {
                err = errors.New(string(resp.Status))
            }
            rc <- err
        },
        message,
        func(req *remotecontrol.TriggerMessageRequest) {
            if evseId > 0 {
                req.Evse = &types.EVSE{Id: evseId}
            }
        },
    )
    return wait(err, rc)
}