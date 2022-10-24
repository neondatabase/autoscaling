package api

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

type MessageKind string

const (
	MessageKindResource MessageKind = "resources"
)

// PodName represents the namespaced name of a pod
//
// Some analogous types already exist elsewhere (e.g., in the kubernetes API packages), but they
// don't provide satisfactory JSON marshal/unmarshalling.
type PodName struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (n PodName) String() string {
	return fmt.Sprintf("%s:%s", n.Namespace, n.Name)
}

/////////////////////////////////
// (Autoscaler) Agent Messages //
/////////////////////////////////

// AgentMessage is the type of a message sent from an autoscaler-agent to the scheduler plugin
type AgentMessage struct {
	// ID is a unique identifier associated with the request. The PluginMessage responding to this
	// AgentMessage must have a matching ID.
	ID   uuid.UUID
	body agentMessageBody
}

func (m *AgentMessage) Kind() MessageKind {
	return m.body.kind()
}

// Internal storage of AgentMessage types.
type agentMessageBody struct {
	Resources *ResourceRequest `json:"resources,omitempty"`
}

func newAgentMessage(id uuid.UUID, body agentMessageBody) AgentMessage {
	return AgentMessage{ID: id, body: body}
}

func (b *agentMessageBody) typeName() string {
	return "AgentMessage"
}

func (b *agentMessageBody) kind() MessageKind {
	switch {
	case b.Resources != nil:
		return MessageKindResource
	default:
		panic("internal error: request body has no non-nil field")
	}
}

// ResourceRequest represents either a request for additional resources (given by the total amount),
// or a notification that the autoscaler-agent has independently decreased its resource usage
//
// All ResourceRequests expect a ResourcePermit in response.
type ResourceRequest struct {
	VCPUs uint16  `json:"vCPUs"`
	Pod   PodName `json:"pod"`
}

func (m AgentMessage) AsResourceRequest() (*ResourceRequest, error) {
	return castMessage(&m.body, MessageKindResource, m.body.Resources)
}

// Request builds and returns a AgentMessage for this ResourceRequest, assigning it a new UUID
//
// The returned Request is guaranteed to successfully return the original ResourceRequest on any
// call to AsResourceRequest.
func (r *ResourceRequest) Request() AgentMessage {
	if r == nil {
		_ = *r // intentionally cause a nil ptr exception if we get passed one.
	}
	return newAgentMessage(uuid.New(), agentMessageBody{Resources: r})
}

// agentMessageJSON is the intermediate JSON representation of the AgentMessage type
//
// This is essentially the same as AgentMessage, execpt that Body is a public field here but not
// there, because we don't want to provide direct access.
type agentMessageJSON struct {
	ID   uuid.UUID        `json:"id"`
	Body agentMessageBody `json:",inline"`
}

func (m *agentMessageJSON) validate() error {
	kinds := make([]MessageKind, 0, 1)

	// Add all the types of messages present
	if m.Body.Resources != nil {
		kinds = append(kinds, MessageKindResource)
	}

	if len(kinds) == 0 {
		return fmt.Errorf("Empty message")
	} else if len(kinds) > 1 {
		return fmt.Errorf("Multiple message types present: %v", kinds)
	} else {
		return nil
	}
}

func (r *AgentMessage) UnmarshalJSON(b []byte) error {
	var m agentMessageJSON
	jsonDecoder := json.NewDecoder(bytes.NewReader(b))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(&m); err != nil {
		return err
	}

	if err := m.validate(); err != nil {
		return err
	}

	r.ID = m.ID
	r.body = m.Body
	return nil
}

func (r *AgentMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(&agentMessageJSON{ID: r.ID, Body: r.body})
}

/////////////////////////////////
// (Scheduler) Plugin Messages //
/////////////////////////////////

// PluginMessage is the type of a message sent from the scheduler plugin to an autoscaler-agent
type PluginMessage struct {
	ID   uuid.UUID
	body pluginMessageBody
}

type pluginMessageBody struct {
	Resources *ResourcePermit `json:"permit,omitempty"`
}

func newPluginMessage(id uuid.UUID, body pluginMessageBody) PluginMessage {
	return PluginMessage{ID: id, body: body}
}

func (b *pluginMessageBody) typeName() string {
	return "PluginMessage"
}

func (b *pluginMessageBody) kind() MessageKind {
	switch {
	case b.Resources != nil:
		return MessageKindResource
	default:
		panic("internal error: request body has no non-nil field")
	}
}

// ResourcePermit represents the response to a ResourceRequest, informing the autoscaler-agent of
// the amount of resources it has been permitted to use
//
// If the original ResourceRequest was merely notifying the scheduler plugin of an independent
// decrease, then the values in the ResourcePermit will match the request.
//
// If it was requesting an increase in resources, then the ResourcePermit will contain resource
// amounts greater than or equal to the previous values and less than or equal to the requested
// values. In other words, the ResourcePermit may have resource amounts anywhere between the current
// and requested values, inclusive.
type ResourcePermit struct {
	VCPUs uint16 `json:"vCPUs"`
}

func (m PluginMessage) AsResourcePermit() (*ResourcePermit, error) {
	return castMessage(&m.body, MessageKindResource, m.body.Resources)
}

func (r *ResourcePermit) Response(requestID uuid.UUID) PluginMessage {
	if r == nil {
		_ = *r // intentionally cause a nil ptr exception if we get passed one.
	}
	return newPluginMessage(requestID, pluginMessageBody{Resources: r})
}

// pluginMessageJSON is the intermediate JSON representation of the PluginMessage type
//
// This is essentially the same as PluginMessage, execpt that Body is a public field here but not
// there, because we don't want to provide direct access.
type pluginMessageJSON struct {
	ID   uuid.UUID         `json:"id"`
	Body pluginMessageBody `json:",inline"`
}

func (m *pluginMessageJSON) validate() error {
	kinds := make([]MessageKind, 0, 1)

	// Add all the types of messages present
	if m.Body.Resources != nil {
		kinds = append(kinds, MessageKindResource)
	}

	if len(kinds) == 0 {
		return fmt.Errorf("Empty message")
	} else if len(kinds) > 1 {
		return fmt.Errorf("Multiple message types present: %v", kinds)
	} else {
		return nil
	}
}

func (r *PluginMessage) UnmarshalJSON(b []byte) error {
	var m pluginMessageJSON
	jsonDecoder := json.NewDecoder(bytes.NewReader(b))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(&m); err != nil {
		return err
	}

	if err := m.validate(); err != nil {
		return err
	}

	r.ID = m.ID
	r.body = m.Body
	return nil
}

func (r *PluginMessage) MarshalJSON() ([]byte, error) {
	return json.Marshal(&pluginMessageJSON{ID: r.ID, Body: r.body})
}

////////////////////////////////
// Helper types and functions //
////////////////////////////////

type messageBody interface {
	typeName() string
	kind() MessageKind
}

func castMessage[B messageBody, F any](b B, kind MessageKind, field *F) (*F, error) {
	bodyKind := b.kind()
	if bodyKind != kind {
		return nil, fmt.Errorf(
			"%s has wrong type: expected %s but found %s",
			b.typeName(), kind, bodyKind,
		)
	}

	return field, nil
}
