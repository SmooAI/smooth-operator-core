package protocol

import "encoding/json"

// EventType is the server→client `type` discriminator carried on every event frame.
type EventType string

// All server→client event type discriminator values.
const (
	EventImmediateResponse         EventType = "immediate_response"
	EventEventualResponse          EventType = "eventual_response"
	EventStreamChunk               EventType = "stream_chunk"
	EventStreamToken               EventType = "stream_token"
	EventKeepalive                 EventType = "keepalive"
	EventWriteConfirmationRequired EventType = "write_confirmation_required"
	EventOTPVerificationRequired   EventType = "otp_verification_required"
	EventOTPSent                   EventType = "otp_sent"
	EventOTPVerified               EventType = "otp_verified"
	EventOTPInvalid                EventType = "otp_invalid"
	EventError                     EventType = "error"
	EventPong                      EventType = "pong"
)

// eventTypes is the set of known event discriminators.
var eventTypes = map[EventType]struct{}{
	EventImmediateResponse:         {},
	EventEventualResponse:          {},
	EventStreamChunk:               {},
	EventStreamToken:               {},
	EventKeepalive:                 {},
	EventWriteConfirmationRequired: {},
	EventOTPVerificationRequired:   {},
	EventOTPSent:                   {},
	EventOTPVerified:               {},
	EventOTPInvalid:                {},
	EventError:                     {},
	EventPong:                      {},
}

// IsKnownEventType reports whether t is a recognised server event discriminator.
func IsKnownEventType(t EventType) bool {
	_, ok := eventTypes[t]
	return ok
}

// ActionType is the client→server `action` discriminator carried on every action frame.
type ActionType string

// All client→server action discriminator values.
const (
	ActionCreateConversationSession ActionType = "create_conversation_session"
	ActionSendMessage               ActionType = "send_message"
	ActionGetSession                ActionType = "get_session"
	ActionGetConversationMessages   ActionType = "get_conversation_messages"
	ActionConfirmToolAction         ActionType = "confirm_tool_action"
	ActionVerifyOTP                 ActionType = "verify_otp"
	ActionPing                      ActionType = "ping"
)

// ServerEvent is the ergonomic, discriminated representation of any server→client
// frame. Go has no sum types, so rather than force a sealed-interface dance on
// callers, a ServerEvent carries the common envelope fields plus the raw frame
// bytes. Discriminate on Type and call the matching typed accessor (As*) to decode
// the concrete generated payload.
//
//	switch ev.Type {
//	case protocol.EventStreamToken:
//	    tok, _ := ev.AsStreamToken()
//	    fmt.Print(tok.Token)
//	case protocol.EventEventualResponse:
//	    final, _ := ev.AsEventualResponse()
//	    ...
//	}
type ServerEvent struct {
	// Type is the event discriminator (`type` on the wire).
	Type EventType
	// RequestID echoes the originating action's requestId, when present.
	RequestID string
	// Status is the HTTP-like status code (0 if absent).
	Status int
	// Node is the workflow node name, present on stream_chunk events.
	Node string
	// Token is the streamed token text, present on stream_token events.
	Token string
	// Raw is the complete, undecoded event frame. Use the As* accessors to decode
	// it into the concrete generated type.
	Raw json.RawMessage
}

// envelopeWire mirrors the common server envelope fields for cheap discrimination
// without committing to a concrete payload shape.
type envelopeWire struct {
	Type      EventType `json:"type"`
	RequestID string    `json:"requestId"`
	Status    int       `json:"status"`
	Node      string    `json:"node"`
	Token     string    `json:"token"`
}

// ParseServerEvent decodes a raw server frame into a discriminable ServerEvent.
// It returns an error if the frame is not valid JSON or carries an unknown `type`.
func ParseServerEvent(frame []byte) (ServerEvent, error) {
	var env envelopeWire
	if err := json.Unmarshal(frame, &env); err != nil {
		return ServerEvent{}, err
	}
	if !IsKnownEventType(env.Type) {
		return ServerEvent{}, &UnknownEventError{Type: string(env.Type)}
	}
	raw := make(json.RawMessage, len(frame))
	copy(raw, frame)
	return ServerEvent{
		Type:      env.Type,
		RequestID: env.RequestID,
		Status:    env.Status,
		Node:      env.Node,
		Token:     env.Token,
		Raw:       raw,
	}, nil
}

// UnknownEventError is returned when a frame carries an unrecognised `type`.
type UnknownEventError struct{ Type string }

func (e *UnknownEventError) Error() string {
	return "smooth-agent: unknown server event type " + strconvQuote(e.Type)
}

// AsImmediateResponse decodes the event as an immediate_response.
func (e ServerEvent) AsImmediateResponse() (ImmediateResponse, error) {
	return decode[ImmediateResponse](e.Raw)
}

// AsEventualResponse decodes the event as an eventual_response (terminal turn event).
func (e ServerEvent) AsEventualResponse() (EventualResponse, error) {
	return decode[EventualResponse](e.Raw)
}

// AsStreamChunk decodes the event as a stream_chunk.
func (e ServerEvent) AsStreamChunk() (StreamChunk, error) {
	return decode[StreamChunk](e.Raw)
}

// AsStreamToken decodes the event as a stream_token.
func (e ServerEvent) AsStreamToken() (StreamToken, error) {
	return decode[StreamToken](e.Raw)
}

// AsKeepalive decodes the event as a keepalive.
func (e ServerEvent) AsKeepalive() (Keepalive, error) {
	return decode[Keepalive](e.Raw)
}

// AsWriteConfirmationRequired decodes the event as a write_confirmation_required (HITL confirm).
func (e ServerEvent) AsWriteConfirmationRequired() (WriteConfirmationRequired, error) {
	return decode[WriteConfirmationRequired](e.Raw)
}

// AsOTPVerificationRequired decodes the event as an otp_verification_required (HITL auth gate).
func (e ServerEvent) AsOTPVerificationRequired() (OTPVerificationRequired, error) {
	return decode[OTPVerificationRequired](e.Raw)
}

// AsOTPSent decodes the event as an otp_sent.
func (e ServerEvent) AsOTPSent() (OTPSent, error) {
	return decode[OTPSent](e.Raw)
}

// AsOTPVerified decodes the event as an otp_verified.
func (e ServerEvent) AsOTPVerified() (OTPVerified, error) {
	return decode[OTPVerified](e.Raw)
}

// AsOTPInvalid decodes the event as an otp_invalid.
func (e ServerEvent) AsOTPInvalid() (OTPInvalid, error) {
	return decode[OTPInvalid](e.Raw)
}

// AsError decodes the event as an error event.
func (e ServerEvent) AsError() (Error, error) {
	return decode[Error](e.Raw)
}

// AsPong decodes the event as a pong.
func (e ServerEvent) AsPong() (Pong, error) {
	return decode[Pong](e.Raw)
}

// IsTerminal reports whether this event ends a streaming turn (success or error).
func (e ServerEvent) IsTerminal() bool {
	return e.Type == EventEventualResponse || e.Type == EventError
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	err := json.Unmarshal(raw, &v)
	return v, err
}

// strconvQuote is a tiny strconv.Quote without pulling strconv into this file's
// import set beyond what's needed; kept local to avoid an extra import in callers.
func strconvQuote(s string) string {
	return "\"" + s + "\""
}
