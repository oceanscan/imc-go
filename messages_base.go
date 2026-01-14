package imc

import (
	"fmt"
)

// IMessage is an interface for all typed IMC messages.
type IMessage interface {
	GetHeader() *Header
	GetMGID() uint16
	ToMessage() *Message
	FromMessage(m *Message) error
}

// Convert generic message to typed message
func (p *Protocol) ToTyped(m *Message) (IMessage, error) {
	msgDef, ok := p.Messages[m.Header.MGID]
	if !ok {
		return nil, fmt.Errorf("unknown message ID: %d", m.Header.MGID)
	}

	return p.CreateTyped(msgDef.Abbrev, m)
}
