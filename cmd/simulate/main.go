package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	ocpp2 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/diagnostics"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// ── Konfiguration ─────────────────────────────────────────────────────────────

const (
	csmsURL = "ws://127.0.0.1:8888"
	stationID = "SIM001"
	evseID    = 1
	connID    = 1
)

// Bekannte RFID-Test-Tokens
var rfidTokens = map[string]string{
	"1": "RFID-TEST-001",
	"2": "RFID-TEST-002",
}

// ── Simulator-State ───────────────────────────────────────────────────────────

type Simulator struct {
	cs          ocpp2.ChargingStation
	mu          sync.Mutex
	txnID       string
	seqNo       int
	power       float64
	maxCurrent  float64
	charging    bool
	meterEnergy float64
	lastToken   string // letzter autorisierter Token
}

func (s *Simulator) nextSeq() int {
	s.seqNo++
	return s.seqNo
}

// ── Handler (CSMS → Simulator) ────────────────────────────────────────────────

func (s *Simulator) OnChangeAvailability(req *availability.ChangeAvailabilityRequest) (*availability.ChangeAvailabilityResponse, error) {
	log.Printf("← ChangeAvailability: status=%s", req.OperationalStatus)
	return availability.NewChangeAvailabilityResponse(availability.ChangeAvailabilityStatusAccepted), nil
}

func (s *Simulator) OnRequestStartTransaction(req *remotecontrol.RequestStartTransactionRequest) (*remotecontrol.RequestStartTransactionResponse, error) {
	log.Printf("← RequestStartTransaction: idToken=%s", req.IDToken.IdToken)
	go s.startTransaction(req.IDToken.IdToken)
	return remotecontrol.NewRequestStartTransactionResponse(remotecontrol.RequestStartStopStatusAccepted), nil
}

func (s *Simulator) OnRequestStopTransaction(req *remotecontrol.RequestStopTransactionRequest) (*remotecontrol.RequestStopTransactionResponse, error) {
	log.Printf("← RequestStopTransaction: txnId=%s", req.TransactionID)
	go s.stopTransaction()
	return remotecontrol.NewRequestStopTransactionResponse(remotecontrol.RequestStartStopStatusAccepted), nil
}

func (s *Simulator) OnTriggerMessage(req *remotecontrol.TriggerMessageRequest) (*remotecontrol.TriggerMessageResponse, error) {
	log.Printf("← TriggerMessage: %s", req.RequestedMessage)
	go func() {
		switch req.RequestedMessage {
		case remotecontrol.MessageTriggerStatusNotification:
			s.sendStatusNotification(availability.ConnectorStatusAvailable)
		case remotecontrol.MessageTriggerMeterValues:
			s.sendMeterValues()
		}
	}()
	return remotecontrol.NewTriggerMessageResponse(remotecontrol.TriggerMessageStatusAccepted), nil
}

func (s *Simulator) OnUnlockConnector(req *remotecontrol.UnlockConnectorRequest) (*remotecontrol.UnlockConnectorResponse, error) {
	log.Printf("← UnlockConnector: evse=%d connector=%d", req.EvseID, req.ConnectorID)
	return remotecontrol.NewUnlockConnectorResponse(remotecontrol.UnlockStatusUnlocked), nil
}

func (s *Simulator) OnSetChargingProfile(req *smartcharging.SetChargingProfileRequest) (*smartcharging.SetChargingProfileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.ChargingProfile != nil && len(req.ChargingProfile.ChargingSchedule) > 0 {
		schedule := req.ChargingProfile.ChargingSchedule[0]
		if len(schedule.ChargingSchedulePeriod) > 0 {
			limit := schedule.ChargingSchedulePeriod[0].Limit
			s.maxCurrent = limit
			s.power = limit * 230 * 3
			log.Printf("← SetChargingProfile: ✅ Neues Limit: %.1f A (%.0f W bei 230V 3-phasig)",
				limit, limit*230*3)
		}
	}
	return smartcharging.NewSetChargingProfileResponse(smartcharging.ChargingProfileStatusAccepted), nil
}

func (s *Simulator) OnClearChargingProfile(req *smartcharging.ClearChargingProfileRequest) (*smartcharging.ClearChargingProfileResponse, error) {
	log.Printf("← ClearChargingProfile")
	s.mu.Lock()
	s.maxCurrent = 16
	s.mu.Unlock()
	return &smartcharging.ClearChargingProfileResponse{Status: smartcharging.ClearChargingProfileStatusAccepted}, nil
}

func (s *Simulator) OnGetCompositeSchedule(req *smartcharging.GetCompositeScheduleRequest) (*smartcharging.GetCompositeScheduleResponse, error) {
	log.Printf("← GetCompositeSchedule: evse=%d duration=%d", req.EvseID, req.Duration)
	return &smartcharging.GetCompositeScheduleResponse{
		Status: smartcharging.GetCompositeScheduleStatusAccepted,
	}, nil
}

func (s *Simulator) OnGetChargingProfiles(req *smartcharging.GetChargingProfilesRequest) (*smartcharging.GetChargingProfilesResponse, error) {
	log.Printf("← GetChargingProfiles")
	return &smartcharging.GetChargingProfilesResponse{
		Status: smartcharging.GetChargingProfileStatusNoProfiles,
	}, nil
}

func (s *Simulator) OnGetBaseReport(req *provisioning.GetBaseReportRequest) (*provisioning.GetBaseReportResponse, error) {
	log.Printf("← GetBaseReport: requestId=%d", req.RequestID)
	return &provisioning.GetBaseReportResponse{
		Status: types.GenericDeviceModelStatusAccepted,
	}, nil
}

func (s *Simulator) OnGetReport(req *provisioning.GetReportRequest) (*provisioning.GetReportResponse, error) {
	return &provisioning.GetReportResponse{Status: types.GenericDeviceModelStatusAccepted}, nil
}

func (s *Simulator) OnGetVariables(req *provisioning.GetVariablesRequest) (*provisioning.GetVariablesResponse, error) {
	results := make([]provisioning.GetVariableResult, len(req.GetVariableData))
	for i, v := range req.GetVariableData {
		results[i] = provisioning.GetVariableResult{
			AttributeStatus: provisioning.GetVariableStatusUnknownVariable,
			Component:       v.Component,
			Variable:        v.Variable,
		}
	}
	return &provisioning.GetVariablesResponse{GetVariableResult: results}, nil
}

func (s *Simulator) OnSetVariables(req *provisioning.SetVariablesRequest) (*provisioning.SetVariablesResponse, error) {
	results := make([]provisioning.SetVariableResult, len(req.SetVariableData))
	for i, v := range req.SetVariableData {
		results[i] = provisioning.SetVariableResult{
			AttributeStatus: provisioning.SetVariableStatusAccepted,
			Component:       v.Component,
			Variable:        v.Variable,
		}
	}
	return &provisioning.SetVariablesResponse{SetVariableResult: results}, nil
}

func (s *Simulator) OnSetNetworkProfile(req *provisioning.SetNetworkProfileRequest) (*provisioning.SetNetworkProfileResponse, error) {
	return &provisioning.SetNetworkProfileResponse{Status: provisioning.SetNetworkProfileStatusAccepted}, nil
}

func (s *Simulator) OnReset(req *provisioning.ResetRequest) (*provisioning.ResetResponse, error) {
	log.Printf("← Reset: type=%s", req.Type)
	return &provisioning.ResetResponse{Status: provisioning.ResetStatusAccepted}, nil
}

func (s *Simulator) OnClearVariableMonitoring(req *diagnostics.ClearVariableMonitoringRequest) (*diagnostics.ClearVariableMonitoringResponse, error) {
	return &diagnostics.ClearVariableMonitoringResponse{}, nil
}
func (s *Simulator) OnCustomerInformation(req *diagnostics.CustomerInformationRequest) (*diagnostics.CustomerInformationResponse, error) {
	return &diagnostics.CustomerInformationResponse{Status: diagnostics.CustomerInformationStatusAccepted}, nil
}
func (s *Simulator) OnGetLog(req *diagnostics.GetLogRequest) (*diagnostics.GetLogResponse, error) {
	return &diagnostics.GetLogResponse{Status: diagnostics.LogStatusAccepted}, nil
}
func (s *Simulator) OnGetMonitoringReport(req *diagnostics.GetMonitoringReportRequest) (*diagnostics.GetMonitoringReportResponse, error) {
	return &diagnostics.GetMonitoringReportResponse{Status: types.GenericDeviceModelStatusAccepted}, nil
}
func (s *Simulator) OnSetMonitoringBase(req *diagnostics.SetMonitoringBaseRequest) (*diagnostics.SetMonitoringBaseResponse, error) {
	return &diagnostics.SetMonitoringBaseResponse{Status: types.GenericDeviceModelStatusAccepted}, nil
}
func (s *Simulator) OnSetMonitoringLevel(req *diagnostics.SetMonitoringLevelRequest) (*diagnostics.SetMonitoringLevelResponse, error) {
	return &diagnostics.SetMonitoringLevelResponse{Status: types.GenericDeviceModelStatusAccepted}, nil
}
func (s *Simulator) OnSetVariableMonitoring(req *diagnostics.SetVariableMonitoringRequest) (*diagnostics.SetVariableMonitoringResponse, error) {
	return &diagnostics.SetVariableMonitoringResponse{}, nil
}

// ── Simulator-Aktionen ────────────────────────────────────────────────────────

func (s *Simulator) sendStatusNotification(status availability.ConnectorStatus) {
	_, err := s.cs.StatusNotification(
		types.NewDateTime(time.Now()),
		status,
		evseID,
		connID,
	)
	if err != nil {
		log.Printf("StatusNotification Fehler: %v", err)
	} else {
		log.Printf("→ StatusNotification: %s", status)
	}
}

// authorize sendet einen AuthorizeRequest und wartet auf die Antwort.
// Gibt true zurück wenn der Token akzeptiert wurde.
func (s *Simulator) authorize(idToken string, tokenType types.IdTokenType) bool {
	log.Printf("→ AuthorizeRequest: Token=%s Typ=%s", idToken, tokenType)

	resp, err := s.cs.Authorize(idToken, tokenType)
	if err != nil {
		log.Printf("⚠️  AuthorizeRequest Fehler: %v", err)
		return false
	}

	status := resp.IdTokenInfo.Status
	if status == types.AuthorizationStatusAccepted {
		log.Printf("✅ Token autorisiert: %s", idToken)
		return true
	}

	log.Printf("❌ Token abgelehnt: %s (Status: %s)", idToken, status)
	return false
}

func (s *Simulator) startTransaction(idToken string) {
	s.mu.Lock()
	if s.charging {
		s.mu.Unlock()
		log.Printf("⚠️  Transaktion läuft bereits!")
		return
	}
	s.mu.Unlock()

	// ── Schritt 1: RFID autorisieren ──────────────────────────────────────
	// In OCPP 2.0.1: Charging Station sendet Authorize BEVOR die Transaktion startet
	if !s.authorize(idToken, types.IdTokenTypeISO14443) {
		log.Printf("⚠️  Ladevorgang abgebrochen: Token nicht autorisiert")
		return
	}

	// ── Schritt 2: Transaktion starten ────────────────────────────────────
	s.mu.Lock()
	s.txnID = fmt.Sprintf("TXN-%d", time.Now().Unix())
	s.charging = true
	s.power = s.maxCurrent * 230 * 3
	s.lastToken = idToken
	txnID := s.txnID
	s.mu.Unlock()

	// StatusNotification: Occupied
	s.sendStatusNotification(availability.ConnectorStatusOccupied)

	// TransactionEvent: Started (mit autorisiertem Token)
	_, err := s.cs.TransactionEvent(
		transactions.TransactionEventStarted,
		types.NewDateTime(time.Now()),
		transactions.TriggerReasonAuthorized, // ← Authorized (nicht CablePluggedIn!)
		s.nextSeq(),
		transactions.Transaction{
			TransactionID: txnID,
			ChargingState: transactions.ChargingStateCharging,
		},
		func(req *transactions.TransactionEventRequest) {
			req.Evse = &types.EVSE{ID: evseID}
			req.IDToken = &types.IdToken{
				IdToken: idToken,
				Type:    types.IdTokenTypeISO14443, // ← echter RFID-Typ
			}
			req.MeterValue = []types.MeterValue{s.buildMeterValue()}
		},
	)
	if err != nil {
		log.Printf("TransactionEvent Started Fehler: %v", err)
		s.mu.Lock()
		s.charging = false
		s.mu.Unlock()
		return
	}
	log.Printf("→ Transaktion gestartet: %s (Token: %s)", txnID, idToken)

	// Messwerte periodisch senden
	go s.meterValueLoop()
}

func (s *Simulator) stopTransaction() {
	s.mu.Lock()
	if !s.charging {
		s.mu.Unlock()
		log.Printf("⚠️  Keine aktive Transaktion!")
		return
	}
	txnID := s.txnID
	token := s.lastToken
	s.charging = false
	s.power = 0
	s.mu.Unlock()

	_, err := s.cs.TransactionEvent(
		transactions.TransactionEventEnded,
		types.NewDateTime(time.Now()),
		transactions.TriggerReasonEVCommunicationLost,
		s.nextSeq(),
		transactions.Transaction{
			TransactionID: txnID,
			ChargingState: transactions.ChargingStateIdle,
			StoppedReason: transactions.ReasonLocal,
		},
		func(req *transactions.TransactionEventRequest) {
			req.Evse = &types.EVSE{ID: evseID}
			req.MeterValue = []types.MeterValue{s.buildMeterValue()}
		},
	)
	if err != nil {
		log.Printf("TransactionEvent Ended Fehler: %v", err)
	} else {
		log.Printf("→ Transaktion beendet: %s (Token: %s)", txnID, token)
	}

	s.sendStatusNotification(availability.ConnectorStatusAvailable)
}

func (s *Simulator) sendMeterValues() {
	_, err := s.cs.MeterValues(evseID, []types.MeterValue{s.buildMeterValue()})
	if err != nil {
		log.Printf("MeterValues Fehler: %v", err)
	}
}

func (s *Simulator) buildMeterValue() types.MeterValue {
	s.mu.Lock()
	power := s.power
	energy := s.meterEnergy
	maxA := s.maxCurrent
	s.mu.Unlock()

	multiplierZero := 0

	return types.MeterValue{
		Timestamp: *types.NewDateTime(time.Now()),
		SampledValue: []types.SampledValue{
			{
				Measurand: types.MeasurandPowerActiveImport,
				Value:     power,
				UnitOfMeasure: &types.UnitOfMeasure{
					Unit:       "W",
					Multiplier: &multiplierZero,
				},
			},
			{
				Measurand: types.MeasurandEnergyActiveImportRegister,
				Value:     energy,
				UnitOfMeasure: &types.UnitOfMeasure{
					Unit:       "Wh",
					Multiplier: &multiplierZero,
				},
			},
			{
				Measurand: types.MeasurandCurrentImport,
				Phase:     types.PhaseL1,
				Value:     maxA,
				UnitOfMeasure: &types.UnitOfMeasure{
					Unit:       "A",
					Multiplier: &multiplierZero,
				},
			},
			{
				Measurand: types.MeasurandCurrentOffered,
				Value:     maxA,
				UnitOfMeasure: &types.UnitOfMeasure{
					Unit:       "A",
					Multiplier: &multiplierZero,
				},
			},
		},
	}
}

func (s *Simulator) meterValueLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		charging := s.charging
		if charging {
			s.meterEnergy += s.power * 15 / 3600
			s.power = s.maxCurrent * 230 * 3
		}
		txnID := s.txnID
		s.mu.Unlock()

		if !charging {
			return
		}

		s.cs.TransactionEvent(
			transactions.TransactionEventUpdated,
			types.NewDateTime(time.Now()),
			transactions.TriggerReasonMeterValuePeriodic,
			s.nextSeq(),
			transactions.Transaction{
				TransactionID: txnID,
				ChargingState: transactions.ChargingStateCharging,
			},
			func(req *transactions.TransactionEventRequest) {
				req.Evse = &types.EVSE{ID: evseID}
				req.MeterValue = []types.MeterValue{s.buildMeterValue()}
			},
		)

		s.mu.Lock()
		log.Printf("📊 Messwerte: %.0fW | %.1f Wh | Limit: %.1fA",
			s.power, s.meterEnergy, s.maxCurrent)
		s.mu.Unlock()

		<-ticker.C
	}
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	sim := &Simulator{maxCurrent: 16}

	sim.cs = ocpp2.NewChargingStation(stationID, nil, nil)

	sim.cs.SetAvailabilityHandler(sim)
	sim.cs.SetRemoteControlHandler(sim)
	sim.cs.SetSmartChargingHandler(sim)
	sim.cs.SetProvisioningHandler(sim)
	sim.cs.SetDiagnosticsHandler(sim)

	go func() {
		for err := range sim.cs.Errors() {
			log.Printf("⚠️  OCPP Fehler: %v", err)
		}
	}()

	log.Printf("🔌 Verbinde mit CSMS: %s", csmsURL)
	if err := sim.cs.Start(csmsURL); err != nil {
		log.Fatalf("Verbindung fehlgeschlagen: %v", err)
	}
	log.Printf("✅ WebSocket verbunden!")

	resp, err := sim.cs.BootNotification(
		provisioning.BootReasonPowerUp,
		"P30-X",
		"KEBA",
		func(req *provisioning.BootNotificationRequest) {
			req.ChargingStation.SerialNumber = "SIM-001"
			req.ChargingStation.FirmwareVersion = "2.1.0"
		},
	)
	if err != nil {
		log.Fatalf("BootNotification fehlgeschlagen: %v", err)
	}
	log.Printf("✅ BootNotification: Status=%s Interval=%ds", resp.Status, resp.Interval)

	sim.sendStatusNotification(availability.ConnectorStatusAvailable)

	printMenu()
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		cmd := scanner.Text()
		switch cmd {
		case "1":
			token := rfidTokens["1"]
			log.Printf("💳 RFID gescannt: %s", token)
			go sim.startTransaction(token)
		case "2":
			token := rfidTokens["2"]
			log.Printf("💳 RFID gescannt: %s", token)
			go sim.startTransaction(token)
		case "3":
			log.Printf("🔌 Fahrzeug wird getrennt...")
			go sim.stopTransaction()
		case "4":
			sim.mu.Lock()
			log.Printf("📊 Status: charging=%v txn=%s token=%s limit=%.1fA power=%.0fW energy=%.1fWh",
				sim.charging, sim.txnID, sim.lastToken,
				sim.maxCurrent, sim.power, sim.meterEnergy)
			sim.mu.Unlock()
		case "5":
			sim.sendStatusNotification(availability.ConnectorStatusAvailable)
		case "q":
			log.Printf("👋 Simulator beendet.")
			sim.cs.Stop()
			return
		default:
			fmt.Println("Unbekannter Befehl")
		}
		printMenu()
	}
}

func printMenu() {
	fmt.Println("\n─────────────────────────────────────────────")
	fmt.Println("  OCPP 2.0.1 KEBA Simulator")
	fmt.Println("─────────────────────────────────────────────")
	fmt.Println("  1 = 💳 RFID-TEST-001 scannen + laden")
	fmt.Println("  2 = 💳 RFID-TEST-002 scannen + laden")
	fmt.Println("  3 = 🔌 Fahrzeug trennen (Transaktion beenden)")
	fmt.Println("  4 = 📊 Status anzeigen")
	fmt.Println("  5 = 📡 StatusNotification senden")
	fmt.Println("  q = Beenden")
	fmt.Println("─────────────────────────────────────────────")
	fmt.Print("Befehl: ")
}