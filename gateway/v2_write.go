package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type v2WriteResponse struct {
	Status      string `json:"status"`
	EntryID     string `json:"entryId"`
	ClientNonce string `json:"clientNonce"`
	DeliveryID  string `json:"deliveryId"`
	Path        string `json:"path,omitempty"`
	Option      int    `json:"option,omitempty"`
	Killed      bool   `json:"killed,omitempty"`
	PIDs        []int  `json:"pids,omitempty"`
	Key         string `json:"key,omitempty"`
}

type v2WriteError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
}

type v2ProcessIdentity struct {
	Started   v2ProcessStarted
	ParentPID int
}

type v2EntryVerifyDependencies struct {
	SocketIdentity  func(string) (uint64, uint64, error)
	ListPanes       func(string) ([]byte, error)
	ProcessAlive    func(int) bool
	ProcessIdentity func(int) (v2ProcessIdentity, error)
}

type v2WriteDependencies struct {
	Verify           func(v2TerminalEntry) (*paneBinding, *v2WriteError)
	VerifyWithTrace  func(v2TerminalEntry, *v2DeliveryTrace) (*paneBinding, *v2WriteError)
	Send             func(*paneBinding, string) error
	Keys             func(*paneBinding, string) error
	Choose           func(*paneBinding, int) error
	Kill             func(*paneBinding) ([]int, error)
	Upload           func(string, []byte) (string, error)
	PrepareReconcile func(v2TerminalEntry, string) func() bool
	MatchAttached    func(string, string) bool
	PersistClosed    func(string) error
}

type v2WriteOutcome struct {
	Response v2WriteResponse
	Error    *v2WriteError
	Trace    *v2DeliveryTrace
}

type v2DeliveryTrace struct {
	DeliveryID string
	EntryID    string
	Started    time.Time
}

func logV2TimingAt(trace *v2DeliveryTrace, phase string, started, ended time.Time, fields string) {
	deliveryID, entryID, totalStarted := "-", "-", started
	if trace != nil {
		if trace.DeliveryID != "" {
			deliveryID = trace.DeliveryID
		}
		if trace.EntryID != "" {
			entryID = trace.EntryID
		}
		if !trace.Started.IsZero() {
			totalStarted = trace.Started
		}
	}
	if started.IsZero() {
		started = ended
	}
	if totalStarted.IsZero() {
		totalStarted = started
	}
	if fields != "" {
		fields = " " + fields
	}
	log.Printf("v2 timing: delivery=%s entry=%s phase=%s%s start_us=%d end_us=%d duration_ms=%.3f total_ms=%.3f",
		deliveryID, entryID, phase, fields, started.UnixMicro(), ended.UnixMicro(),
		float64(ended.Sub(started))/float64(time.Millisecond), float64(ended.Sub(totalStarted))/float64(time.Millisecond))
}

func logV2Timing(trace *v2DeliveryTrace, phase string, started time.Time, fields string) {
	logV2TimingAt(trace, phase, started, time.Now(), fields)
}

type v2WriteOperation struct {
	Action     string
	DeliveryID string
	Done       chan struct{}
	Outcome    v2WriteOutcome
}

type v2WriteService struct {
	store        *terminalEntryStore
	dependencies v2WriteDependencies
	mu           sync.Mutex
	operations   map[string]*v2WriteOperation
	pendingMu    sync.Mutex
	pending      map[string]*v2PendingEcho
	pendingRun   bool
}

type v2PendingEcho struct {
	trace       *v2DeliveryTrace
	clientNonce string
	expected    string
	started     time.Time
}

func newV2WriteService(store *terminalEntryStore, dependencies v2WriteDependencies) *v2WriteService {
	service := &v2WriteService{store: store, dependencies: dependencies, operations: map[string]*v2WriteOperation{}, pending: map[string]*v2PendingEcho{}}
	if service.dependencies.VerifyWithTrace == nil {
		if service.dependencies.Verify == nil {
			service.dependencies.VerifyWithTrace = defaultV2VerifyEntryWithTrace
		} else {
			service.dependencies.VerifyWithTrace = func(entry v2TerminalEntry, _ *v2DeliveryTrace) (*paneBinding, *v2WriteError) {
				return service.dependencies.Verify(entry)
			}
		}
	}
	if service.dependencies.Verify == nil {
		service.dependencies.Verify = defaultV2VerifyEntry
	}
	if service.dependencies.Send == nil {
		service.dependencies.Send = sendToPane
	}
	if service.dependencies.Keys == nil {
		service.dependencies.Keys = sendV2Key
	}
	if service.dependencies.Choose == nil {
		service.dependencies.Choose = sendV2Choice
	}
	if service.dependencies.Kill == nil {
		service.dependencies.Kill = terminatePane
	}
	if service.dependencies.Upload == nil {
		service.dependencies.Upload = func(name string, data []byte) (string, error) {
			path, _, err := writeUploadedFile(filepath.Join(homeDir(), "Library", "Caches", "corral-uploads"), name, bytes.NewReader(data))
			return path, err
		}
	}
	if service.dependencies.PrepareReconcile == nil {
		service.dependencies.PrepareReconcile = defaultV2PrepareReconcile
	}
	if service.dependencies.MatchAttached == nil {
		service.dependencies.MatchAttached = func(entryID, expected string) bool { return defaultV2MatchAttached(store, entryID, expected) }
	}
	if service.dependencies.PersistClosed == nil {
		service.dependencies.PersistClosed = closedSessions.add
	}
	return service
}

func newV2DeliveryID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err == nil {
		return "d1_" + base64.RawURLEncoding.EncodeToString(raw)
	}
	return fmt.Sprintf("d1_%x", time.Now().UnixNano())
}

func (service *v2WriteService) execute(entryID, clientNonce, action string, run func(string) v2WriteOutcome) v2WriteOutcome {
	key := entryID + "\x00" + clientNonce
	service.mu.Lock()
	if existing := service.operations[key]; existing != nil {
		if existing.Action != action {
			service.mu.Unlock()
			return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusConflict, Code: "client_nonce_conflict", Message: "clientNonce was already used for another action"}}
		}
		done := existing.Done
		service.mu.Unlock()
		<-done
		return existing.Outcome
	}
	operation := &v2WriteOperation{Action: action, DeliveryID: newV2DeliveryID(), Done: make(chan struct{})}
	service.operations[key] = operation
	service.mu.Unlock()

	outcome := run(operation.DeliveryID)
	service.mu.Lock()
	operation.Outcome = outcome
	close(operation.Done)
	service.mu.Unlock()
	return outcome
}

func (service *v2WriteService) verifiedEntry(entryID string) (v2TerminalEntry, *paneBinding, *v2WriteError) {
	return service.verifiedEntryWithTrace(entryID, nil)
}

func (service *v2WriteService) verifiedEntryWithTrace(entryID string, trace *v2DeliveryTrace) (v2TerminalEntry, *paneBinding, *v2WriteError) {
	entry, status := service.store.lookup(entryID)
	if status == http.StatusGone {
		return v2TerminalEntry{}, nil, &v2WriteError{Status: status, Code: "entry_gone", Message: "entry generation has exited"}
	}
	if status == http.StatusServiceUnavailable {
		return v2TerminalEntry{}, nil, &v2WriteError{Status: status, Code: "not_ready", Message: "initial entry index is not ready", Retryable: true}
	}
	if status != http.StatusOK {
		return v2TerminalEntry{}, nil, &v2WriteError{Status: http.StatusNotFound, Code: "entry_not_found", Message: "entry not found"}
	}
	binding, writeErr := service.dependencies.VerifyWithTrace(entry, trace)
	if writeErr != nil {
		return v2TerminalEntry{}, nil, writeErr
	}
	return entry, binding, nil
}

func (service *v2WriteService) send(entryID, clientNonce, text string) v2WriteOutcome {
	return service.sendAt(entryID, clientNonce, text, time.Now())
}

func (service *v2WriteService) sendAt(entryID, clientNonce, text string, requestStarted time.Time) v2WriteOutcome {
	return service.execute(entryID, clientNonce, "send", func(deliveryID string) (outcome v2WriteOutcome) {
		trace := &v2DeliveryTrace{DeliveryID: deliveryID, EntryID: entryID, Started: requestStarted}
		defer func() { outcome.Trace = trace }()
		logV2Timing(trace, "post_enter", requestStarted, "result=decoded")
		verifyStarted := time.Now()
		entry, binding, writeErr := service.verifiedEntryWithTrace(entryID, trace)
		if writeErr != nil {
			logV2Timing(trace, "verify_done", verifyStarted, "result=error code="+writeErr.Code)
			return v2WriteOutcome{Error: writeErr}
		}
		logV2Timing(trace, "verify_done", verifyStarted, "result=ok")
		formatted := formatSessionSendText(entry.Kind, text)
		prepareStarted := time.Now()
		reconcile := service.dependencies.PrepareReconcile(entry, formatted)
		logV2Timing(trace, "echo_probe_prepared", prepareStarted, "result=ok")
		requestSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(formatted)))
		log.Printf("v2 send: entry=%s delivery=%s socket=%q pane=%s bytes=%d sha256=%s", entryID, deliveryID, binding.Socket, binding.TmuxID, len(formatted), requestSHA)
		sendStarted := time.Now()
		if err := service.dependencies.Send(binding, formatted); err != nil {
			logV2Timing(trace, "send_keys_done", sendStarted, "result=error")
			return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusInternalServerError, Code: "send_failed", Message: err.Error()}}
		}
		logV2Timing(trace, "send_keys_done", sendStarted, "result=ok")
		service.publishDelivery(trace, clientNonce, "accepted", "")
		go func() {
			probeStarted := time.Now()
			logV2TimingAt(trace, "echo_probe_start", probeStarted, probeStarted, "stage=initial")
			matched := false
			if reconcile != nil {
				matched = reconcile()
			} else {
				matched = service.dependencies.MatchAttached(entryID, formatted)
			}
			logV2Timing(trace, "echo_probe_done", probeStarted, fmt.Sprintf("stage=initial result=%t", matched))
			if matched {
				confirmedAt := time.Now()
				logV2TimingAt(trace, "jsonl_confirmed", confirmedAt, confirmedAt, "stage=initial")
				service.store.clearAttachmentSuspect(entryID)
				service.publishDelivery(trace, clientNonce, "echoed", "stage=initial")
				return
			}
			recheckStarted := time.Now()
			rechecked := service.dependencies.MatchAttached(entryID, formatted)
			logV2Timing(trace, "echo_probe_done", recheckStarted, fmt.Sprintf("stage=recheck result=%t", rechecked))
			if rechecked {
				confirmedAt := time.Now()
				logV2TimingAt(trace, "jsonl_confirmed", confirmedAt, confirmedAt, "stage=recheck")
				service.store.clearAttachmentSuspect(entryID)
				service.publishDelivery(trace, clientNonce, "echoed", "stage=recheck")
				return
			}
			service.store.markAttachmentSuspect(entryID, "delivery_unattributed")
			service.publishDelivery(trace, clientNonce, "unattributed", "stage=recheck")
			pendingStarted := time.Now()
			logV2TimingAt(trace, "echo_probe_start", pendingStarted, pendingStarted, "stage=attachment_event")
			service.queuePendingEcho(entryID, formatted, clientNonce, trace, pendingStarted)
		}()
		return v2WriteOutcome{Response: acceptedV2WriteResponse(entryID, clientNonce, deliveryID)}
	})
}

func (service *v2WriteService) publishDelivery(trace *v2DeliveryTrace, clientNonce, status, fields string) {
	producedAt := time.Now()
	statusFields := "status=" + status
	if fields != "" {
		statusFields += " " + fields
	}
	logV2TimingAt(trace, "delivery_status", producedAt, producedAt, statusFields)
	service.store.publishDelivery(v2DeliveryEvent{
		Type: "delivery", EntryID: trace.EntryID, ClientNonce: clientNonce, DeliveryID: trace.DeliveryID, Status: status,
		ProducedAt: producedAt, TraceStarted: trace.Started,
	})
}

func (service *v2WriteService) queuePendingEcho(entryID, expected, clientNonce string, trace *v2DeliveryTrace, started time.Time) {
	key := entryID + "\x00" + trace.DeliveryID
	service.pendingMu.Lock()
	service.pending[key] = &v2PendingEcho{trace: trace, clientNonce: clientNonce, expected: expected, started: started}
	if service.pendingRun {
		service.pendingMu.Unlock()
		return
	}
	service.pendingRun = true
	service.pendingMu.Unlock()
	go service.runPendingEchoes()
}

func (service *v2WriteService) runPendingEchoes() {
	changes, cancel := service.store.subscribe("")
	defer cancel()
	for {
		service.reconcilePendingEchoes()
		service.pendingMu.Lock()
		if len(service.pending) == 0 {
			service.pendingRun = false
			service.pendingMu.Unlock()
			return
		}
		service.pendingMu.Unlock()
		if _, ok := <-changes; !ok {
			return
		}
	}
}

func (service *v2WriteService) reconcilePendingEchoes() {
	service.pendingMu.Lock()
	items := make(map[string]*v2PendingEcho, len(service.pending))
	for key, pending := range service.pending {
		items[key] = pending
	}
	service.pendingMu.Unlock()
	for key, pending := range items {
		entryID := pending.trace.EntryID
		_, status := service.store.lookup(entryID)
		matched := status == http.StatusOK && service.dependencies.MatchAttached(entryID, pending.expected)
		if status == http.StatusOK && !matched {
			continue
		}
		service.pendingMu.Lock()
		if service.pending[key] != pending {
			service.pendingMu.Unlock()
			continue
		}
		delete(service.pending, key)
		service.pendingMu.Unlock()
		logV2Timing(pending.trace, "echo_probe_done", pending.started, fmt.Sprintf("stage=attachment_event result=%t", matched))
		if matched {
			confirmedAt := time.Now()
			logV2TimingAt(pending.trace, "jsonl_confirmed", confirmedAt, confirmedAt, "stage=attachment_event")
			service.store.clearAttachmentSuspect(entryID)
			service.publishDelivery(pending.trace, pending.clientNonce, "echoed", "stage=attachment_event")
		}
	}
}

func (service *v2WriteService) upload(entryID, clientNonce, name string, data []byte) v2WriteOutcome {
	return service.execute(entryID, clientNonce, "upload", func(deliveryID string) v2WriteOutcome {
		_, _, writeErr := service.verifiedEntry(entryID)
		if writeErr != nil {
			return v2WriteOutcome{Error: writeErr}
		}
		path, err := service.dependencies.Upload(name, data)
		if err != nil {
			if errors.Is(err, errUploadTooLarge) {
				return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusRequestEntityTooLarge, Code: "upload_too_large", Message: err.Error()}}
			}
			return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusInternalServerError, Code: "upload_failed", Message: "failed to store attachment", Retryable: true}}
		}
		response := acceptedV2WriteResponse(entryID, clientNonce, deliveryID)
		response.Path = path
		return v2WriteOutcome{Response: response}
	})
}

func (service *v2WriteService) choose(entryID, clientNonce string, option int) v2WriteOutcome {
	return service.execute(entryID, clientNonce, "choose", func(deliveryID string) v2WriteOutcome {
		_, binding, writeErr := service.verifiedEntry(entryID)
		if writeErr != nil {
			return v2WriteOutcome{Error: writeErr}
		}
		if err := service.dependencies.Choose(binding, option); err != nil {
			return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusInternalServerError, Code: "choose_failed", Message: err.Error()}}
		}
		response := acceptedV2WriteResponse(entryID, clientNonce, deliveryID)
		response.Option = option
		return v2WriteOutcome{Response: response}
	})
}

func (service *v2WriteService) keys(entryID, clientNonce, key string) v2WriteOutcome {
	return service.execute(entryID, clientNonce, "keys", func(deliveryID string) v2WriteOutcome {
		_, binding, writeErr := service.verifiedEntry(entryID)
		if writeErr != nil {
			return v2WriteOutcome{Error: writeErr}
		}
		if err := service.dependencies.Keys(binding, key); err != nil {
			return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusInternalServerError, Code: "keys_failed", Message: err.Error()}}
		}
		response := acceptedV2WriteResponse(entryID, clientNonce, deliveryID)
		response.Key = key
		return v2WriteOutcome{Response: response}
	})
}

func (service *v2WriteService) kill(entryID, clientNonce string) v2WriteOutcome {
	return service.execute(entryID, clientNonce, "kill", func(deliveryID string) v2WriteOutcome {
		entry, binding, writeErr := service.verifiedEntry(entryID)
		if writeErr != nil {
			return v2WriteOutcome{Error: writeErr}
		}
		service.store.markPendingRemoval(entryID, "killed")
		pids, err := service.dependencies.Kill(binding)
		if err != nil {
			service.store.clearPendingRemoval(entryID)
			return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusInternalServerError, Code: "kill_failed", Message: err.Error()}}
		}
		service.store.removeEntry(entryID, "killed")
		if entry.Attachment != nil && entry.Attachment.SessionID != "" {
			if err := service.dependencies.PersistClosed(entry.Attachment.SessionID); err != nil {
				return v2WriteOutcome{Error: &v2WriteError{Status: http.StatusInternalServerError, Code: "closed_state_failed", Message: "CLI terminated but closed state could not be persisted"}}
			}
		}
		response := acceptedV2WriteResponse(entryID, clientNonce, deliveryID)
		response.Killed = true
		response.PIDs = append([]int(nil), pids...)
		return v2WriteOutcome{Response: response}
	})
}

func acceptedV2WriteResponse(entryID, clientNonce, deliveryID string) v2WriteResponse {
	return v2WriteResponse{Status: "accepted", EntryID: entryID, ClientNonce: clientNonce, DeliveryID: deliveryID}
}

func serveV2EntryWrite(w http.ResponseWriter, r *http.Request, service *v2WriteService, entryID, action string) {
	requestStarted := time.Now()
	if r.Method != http.MethodPost {
		writeV2Error(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required", false)
		return
	}
	var outcome v2WriteOutcome
	switch action {
	case "send":
		var body struct {
			ClientNonce string `json:"clientNonce"`
			Text        string `json:"text"`
		}
		if writeErr := decodeV2WriteJSON(w, r, &body); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if writeErr := validateV2ClientNonce(body.ClientNonce); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if body.Text == "" {
			writeV2Error(w, http.StatusBadRequest, "invalid_request", "non-empty text is required", false)
			return
		}
		outcome = service.sendAt(entryID, body.ClientNonce, body.Text, requestStarted)
	case "upload":
		clientNonce, name, data, writeErr := decodeV2Upload(w, r)
		if writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		outcome = service.upload(entryID, clientNonce, name, data)
	case "choose":
		var body struct {
			ClientNonce string `json:"clientNonce"`
			Option      int    `json:"option"`
		}
		if writeErr := decodeV2WriteJSON(w, r, &body); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if writeErr := validateV2ClientNonce(body.ClientNonce); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if body.Option < 1 {
			writeV2Error(w, http.StatusBadRequest, "invalid_request", "option must be a positive integer", false)
			return
		}
		outcome = service.choose(entryID, body.ClientNonce, body.Option)
	case "keys":
		var body struct {
			ClientNonce string `json:"clientNonce"`
			Key         string `json:"key"`
		}
		if writeErr := decodeV2WriteJSON(w, r, &body); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if writeErr := validateV2ClientNonce(body.ClientNonce); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if !validV2Key(body.Key) {
			writeV2Error(w, http.StatusBadRequest, "invalid_key", "key is not in the allowed set", false)
			return
		}
		outcome = service.keys(entryID, body.ClientNonce, body.Key)
	case "kill":
		var body struct {
			ClientNonce string `json:"clientNonce"`
		}
		if writeErr := decodeV2WriteJSON(w, r, &body); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		if writeErr := validateV2ClientNonce(body.ClientNonce); writeErr != nil {
			writeV2WriteError(w, writeErr)
			return
		}
		outcome = service.kill(entryID, body.ClientNonce)
	default:
		writeV2Error(w, http.StatusNotFound, "route_not_found", "v2 API route not found", false)
		return
	}
	if outcome.Error != nil {
		if outcome.Trace != nil {
			responseReady := time.Now()
			logV2TimingAt(outcome.Trace, "response_ready", responseReady, responseReady,
				fmt.Sprintf("status=%d result=%s request_ms=%.3f", outcome.Error.Status, outcome.Error.Code, float64(responseReady.Sub(requestStarted))/float64(time.Millisecond)))
		}
		writeV2WriteError(w, outcome.Error)
		return
	}
	if outcome.Trace != nil {
		responseReady := time.Now()
		logV2TimingAt(outcome.Trace, "response_ready", responseReady, responseReady,
			fmt.Sprintf("status=200 result=accepted request_ms=%.3f", float64(responseReady.Sub(requestStarted))/float64(time.Millisecond)))
	}
	writeJSON(w, http.StatusOK, outcome.Response)
}

func decodeV2WriteJSON(w http.ResponseWriter, r *http.Request, target any) *v2WriteError {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err := decoder.Decode(target); err != nil {
		return &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "invalid JSON body"}
	}
	return nil
}

func validateV2ClientNonce(value string) *v2WriteError {
	if value == "" {
		return &v2WriteError{Status: http.StatusBadRequest, Code: "client_nonce_required", Message: "clientNonce is required"}
	}
	if len(value) > 256 || strings.TrimSpace(value) != value {
		return &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_client_nonce", Message: "clientNonce is invalid"}
	}
	return nil
}

func decodeV2Upload(w http.ResponseWriter, r *http.Request) (string, string, []byte, *v2WriteError) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1024*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		return "", "", nil, &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "multipart file and clientNonce are required"}
	}
	var clientNonce, name string
	var data []byte
	for {
		part, nextErr := reader.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(nextErr, &maxErr) {
				return "", "", nil, &v2WriteError{Status: http.StatusRequestEntityTooLarge, Code: "upload_too_large", Message: errUploadTooLarge.Error()}
			}
			return "", "", nil, &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "invalid multipart body"}
		}
		switch part.FormName() {
		case "clientNonce":
			value, readErr := io.ReadAll(io.LimitReader(part, 4097))
			_ = part.Close()
			if readErr != nil || len(value) > 4096 {
				return "", "", nil, &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_client_nonce", Message: "clientNonce is invalid"}
			}
			clientNonce = string(value)
		case "file":
			if part.FileName() == "" || name != "" {
				_ = part.Close()
				return "", "", nil, &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "exactly one file is required"}
			}
			name = part.FileName()
			value, readErr := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
			_ = part.Close()
			if readErr != nil {
				var maxErr *http.MaxBytesError
				if errors.As(readErr, &maxErr) {
					return "", "", nil, &v2WriteError{Status: http.StatusRequestEntityTooLarge, Code: "upload_too_large", Message: errUploadTooLarge.Error()}
				}
				return "", "", nil, &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "invalid multipart body"}
			}
			if int64(len(value)) > maxUploadBytes {
				return "", "", nil, &v2WriteError{Status: http.StatusRequestEntityTooLarge, Code: "upload_too_large", Message: errUploadTooLarge.Error()}
			}
			data = value
		default:
			_ = part.Close()
		}
	}
	if writeErr := validateV2ClientNonce(clientNonce); writeErr != nil {
		return "", "", nil, writeErr
	}
	if name == "" {
		return "", "", nil, &v2WriteError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "multipart field file is required"}
	}
	return clientNonce, name, data, nil
}

func writeV2WriteError(w http.ResponseWriter, writeErr *v2WriteError) {
	writeV2Error(w, writeErr.Status, writeErr.Code, writeErr.Message, writeErr.Retryable)
}

func sendV2Choice(binding *paneBinding, option int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	log.Printf("v2 choose: socket=%q pane=%s option=%d", binding.Socket, binding.TmuxID, option)
	return tmuxCommandContext(ctx, binding.Socket, v2ChoiceArgs(binding.TmuxID, option)...).Run()
}

func v2ChoiceArgs(paneID string, option int) []string {
	return []string{"send-keys", "-t", paneID, strconv.Itoa(option)}
}

func validV2Key(key string) bool {
	if len(key) == 1 && ((key[0] >= '0' && key[0] <= '9') || (key[0] >= 'A' && key[0] <= 'Z') || (key[0] >= 'a' && key[0] <= 'z')) {
		return true
	}
	switch key {
	case "Enter", "Up", "Down", "Left", "Right", "Escape", "Tab", "Ctrl+C":
		return true
	}
	return false
}

func sendV2Key(binding *paneBinding, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	log.Printf("v2 keys: socket=%q pane=%s key=%q", binding.Socket, binding.TmuxID, key)
	return tmuxCommandContext(ctx, binding.Socket, v2KeyArgs(binding.TmuxID, key)...).Run()
}

func v2KeyArgs(paneID, key string) []string {
	if len(key) == 1 {
		return []string{"send-keys", "-t", paneID, "-l", "--", key}
	}
	if key == "Ctrl+C" {
		key = "C-c"
	}
	return []string{"send-keys", "-t", paneID, key}
}

func sameV2EntryIdentity(left, right v2EntryIdentity) bool {
	return left.HostID == right.HostID && filepath.Clean(left.SocketPath) == filepath.Clean(right.SocketPath) &&
		left.SocketDevice == right.SocketDevice && left.SocketInode == right.SocketInode && left.PaneID == right.PaneID &&
		left.AgentPID == right.AgentPID && left.StartSec == right.StartSec && left.StartUsec == right.StartUsec
}

func defaultV2VerifyEntry(entry v2TerminalEntry) (*paneBinding, *v2WriteError) {
	return defaultV2VerifyEntryWithTrace(entry, nil)
}

func defaultV2VerifyEntryWithTrace(entry v2TerminalEntry, trace *v2DeliveryTrace) (*paneBinding, *v2WriteError) {
	return verifyV2EntryWithDependenciesTrace(entry, v2EntryVerifyDependencies{
		SocketIdentity: func(path string) (uint64, uint64, error) {
			info, err := os.Stat(path)
			if err != nil {
				return 0, 0, err
			}
			device, inode := physicalIdentity(info)
			return device, inode, nil
		},
		ListPanes: func(socket string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			return tmuxCommandContext(ctx, socket, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_pid}\t#{window_name}").CombinedOutput()
		},
		ProcessAlive:    processAlive,
		ProcessIdentity: v2ProcessIdentityForPID,
	}, trace)
}

func verifyV2EntryWithDependencies(entry v2TerminalEntry, dependencies v2EntryVerifyDependencies) (*paneBinding, *v2WriteError) {
	return verifyV2EntryWithDependenciesTrace(entry, dependencies, nil)
}

func verifyV2EntryWithDependenciesTrace(entry v2TerminalEntry, dependencies v2EntryVerifyDependencies, trace *v2DeliveryTrace) (*paneBinding, *v2WriteError) {
	if entry.runtime == nil {
		return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
	}
	want := entry.runtime.Identity
	log.Printf("v2 verify: entry=%s socket=%q pane=%s sticky_pane_pid=%d agent_pid=%d", entry.EntryID, want.SocketPath, want.PaneID, entry.runtime.Binding.PanePID, want.AgentPID)
	device, inode, err := dependencies.SocketIdentity(want.SocketPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		}
		return nil, &v2WriteError{Status: http.StatusServiceUnavailable, Code: "identity_unavailable", Message: "tmux socket identity is unavailable", Retryable: true}
	}
	if device != want.SocketDevice || inode != want.SocketInode {
		return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
	}
	sticky := entry.runtime.Binding
	if sticky.PanePID <= 0 || filepath.Clean(sticky.Socket) != filepath.Clean(want.SocketPath) || sticky.TmuxID != want.PaneID || sticky.Kind != entry.Kind || v2EntryID(want) != entry.EntryID {
		return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
	}
	var out []byte
	listed := false
	for attempt := 1; attempt <= 2; attempt++ {
		attemptStarted := time.Now()
		out, err = dependencies.ListPanes(want.SocketPath)
		if err == nil {
			if trace != nil {
				logV2Timing(trace, "verify_list_panes", attemptStarted, fmt.Sprintf("attempt=%d result=ok", attempt))
			}
			listed = true
			break
		}
		if trace == nil {
			trace = &v2DeliveryTrace{EntryID: entry.EntryID, Started: attemptStarted}
		}
		logV2Timing(trace, "verify_list_panes", attemptStarted, fmt.Sprintf("attempt=%d result=error error=%q output=%q", attempt, err.Error(), string(out)))
		message := strings.ToLower(string(out))
		if strings.Contains(message, "no server running") || strings.Contains(message, "no sessions") || strings.Contains(message, "server exited") || strings.Contains(message, "error connecting") {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		}
		if attempt == 1 {
			time.Sleep(25 * time.Millisecond)
		}
	}
	pane := paneBinding{Socket: want.SocketPath, TmuxID: want.PaneID, Kind: entry.Kind}
	if listed {
		found := false
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) < 2 || fields[0] != want.PaneID {
				continue
			}
			pane.PanePID, _ = strconv.Atoi(fields[1])
			if len(fields) == 3 {
				pane.WindowName = fields[2]
			}
			found = pane.PanePID > 0
			break
		}
		if !found || pane.PanePID != sticky.PanePID {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		}
	} else {
		if !dependencies.ProcessAlive(sticky.PanePID) || !dependencies.ProcessAlive(want.AgentPID) {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		}
		pane = sticky
		log.Printf("v2 verify: entry=%s list-panes unavailable after retry; using sticky pane identity pane=%s pane_pid=%d", entry.EntryID, pane.TmuxID, pane.PanePID)
	}
	if !dependencies.ProcessAlive(want.AgentPID) {
		return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
	}
	identity, identityErr := dependencies.ProcessIdentity(want.AgentPID)
	if identityErr != nil {
		if !dependencies.ProcessAlive(want.AgentPID) {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		}
		log.Printf("v2 verify: entry=%s agent_pid=%d targeted identity unavailable: %v; using sticky identity", entry.EntryID, want.AgentPID, identityErr)
	} else if identity.Started.Sec != want.StartSec || identity.Started.Usec != want.StartUsec {
		return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
	} else {
		descends, known := want.AgentPID == pane.PanePID, true
		if !descends {
			descends, known = v2ProcessDescendsFromPane(identity, pane.PanePID, dependencies.ProcessIdentity)
		}
		if known && !descends {
			return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
		}
		if !known {
			if !dependencies.ProcessAlive(want.AgentPID) {
				return nil, &v2WriteError{Status: http.StatusGone, Code: "entry_gone", Message: "entry generation has exited"}
			}
			log.Printf("v2 verify: entry=%s agent_pid=%d parent chain unavailable; using sticky identity", entry.EntryID, want.AgentPID)
		}
	}
	pane.Kind = entry.Kind
	pane.ProcessPIDs = append([]int(nil), sticky.ProcessPIDs...)
	return &pane, nil
}

func v2ProcessDescendsFromPane(identity v2ProcessIdentity, panePID int, lookup func(int) (v2ProcessIdentity, error)) (bool, bool) {
	seen := map[int]bool{}
	current := identity.ParentPID
	for range 64 {
		if current == panePID {
			return true, true
		}
		if current <= 1 || seen[current] {
			return false, true
		}
		seen[current] = true
		next, err := lookup(current)
		if err != nil {
			return false, false
		}
		current = next.ParentPID
	}
	return false, false
}

func defaultV2PrepareReconcile(entry v2TerminalEntry, expected string) func() bool {
	if entry.Attachment == nil {
		return nil
	}
	record := v2AttachedSessionRecord(entry)
	if record == nil || record.SessionFile == "" {
		return nil
	}
	baseline, err := fileWritePointForPath(record.SessionFile)
	if err != nil {
		return nil
	}
	return func() bool {
		if record.Kind == "codex" {
			return waitForCodexSendReconciliation(record.SessionFile, baseline, expected, 5*time.Second) == nil
		}
		_, err := waitForClaudeSendReconciliation(record.SessionFile, baseline, expected, 5*time.Second)
		return err == nil
	}
}

func v2AttachedSessionRecord(entry v2TerminalEntry) *sessionRecord {
	if entry.Attachment == nil {
		return nil
	}
	if entry.runtime != nil && entry.runtime.Record != nil {
		record := entry.runtime.Record
		if record.ID == entry.Attachment.RecordID && record.SessionID == entry.Attachment.SessionID && record.SessionFile != "" {
			return record
		}
	}
	return findSession(entry.Attachment.RecordID, false)
}

func waitForCodexSendReconciliation(path string, baseline fileWritePoint, expected string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		current, err := fileWritePointForPath(path)
		if err == nil && sameFileIdentity(baseline, current) && current.Size > baseline.Size {
			file, openErr := os.Open(path)
			if openErr == nil {
				_, seekErr := file.Seek(baseline.Size, io.SeekStart)
				data, readErr := io.ReadAll(io.LimitReader(file, 8*1024*1024))
				_ = file.Close()
				if seekErr == nil && readErr == nil && appendedCodexUserMessageMatches(data, expected) {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("expected user message did not appear in %s within %s", path, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func appendedCodexUserMessageMatches(data []byte, expected string) bool {
	want := normalizeSendEcho(expected)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		var row struct {
			Type    string `json:"type"`
			Payload struct {
				Type    string          `json:"type"`
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &row) == nil && row.Type == "response_item" && row.Payload.Type == "message" && row.Payload.Role == "user" &&
			normalizeSendEcho(parseContentText(row.Payload.Content, "input_text")) == want {
			return true
		}
	}
	return false
}

func defaultV2MatchAttached(store *terminalEntryStore, entryID, expected string) bool {
	entry, status := store.lookup(entryID)
	if status != http.StatusOK || entry.Attachment == nil {
		return false
	}
	record := v2AttachedSessionRecord(entry)
	if record == nil || record.SessionFile == "" {
		return false
	}
	file, err := os.Open(record.SessionFile)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	const maxTail = int64(8 * 1024 * 1024)
	if info.Size() > maxTail {
		_, _ = file.Seek(info.Size()-maxTail, io.SeekStart)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTail))
	if err != nil {
		return false
	}
	if record.Kind == "codex" {
		return appendedCodexUserMessageMatches(data, expected)
	}
	return appendedClaudeUserMessageMatches(data, expected)
}
