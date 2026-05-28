package ws

type MessageType string

const (
	MessageTypeInput      MessageType = "input"
	MessageTypeOutput     MessageType = "output"
	MessageTypeResize     MessageType = "resize"
	MessageTypePasteImage MessageType = "paste-image"
)

type Message struct {
	Type MessageType `json:"type"`
	Data string      `json:"data,omitempty"`
	Mime string      `json:"mime,omitempty"`
	Rows uint16      `json:"rows,omitempty"`
	Cols uint16      `json:"cols,omitempty"`
}
