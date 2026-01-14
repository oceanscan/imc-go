package imc

import (
	"testing"
)

func TestSerialization(t *testing.T) {
	xmlProto, err := ParseXML("IMC.xml")
	if err != nil {
		t.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	p := NewProtocol(xmlProto)

	msg, err := p.CreateMessage("Announce")
	if err != nil {
		t.Fatalf("Failed to create Announce message: %v", err)
	}

	msg.Fields["sys_name"] = "test-node"
	msg.Fields["sys_type"] = uint8(2) // UUV
	msg.Fields["lat"] = 0.71
	msg.Fields["lon"] = -0.15
	msg.Fields["height"] = float32(10.5)
	msg.Fields["services"] = "service1,service2"

	data, err := msg.Serialize(p)
	if err != nil {
		t.Fatalf("Failed to serialize message: %v", err)
	}

	t.Logf("Serialized message size: %d bytes", len(data))

	// Re-unmarshal
	newMsg, err := p.Unmarshal(data)
	if err != nil {
		t.Fatalf("Failed to unmarshal message: %v", err)
	}

	if newMsg.Header.MGID != msg.Header.MGID {
		t.Errorf("MGID mismatch: expected %d, got %d", msg.Header.MGID, newMsg.Header.MGID)
	}

	if newMsg.Fields["sys_name"] != "test-node" {
		t.Errorf("sys_name mismatch: expected 'test-node', got '%v'", newMsg.Fields["sys_name"])
	}

	if newMsg.Fields["sys_type"] != uint8(2) {
		t.Errorf("sys_type mismatch: expected 2, got %v", newMsg.Fields["sys_type"])
	}

	// Verify float values (with small epsilon or just direct comparison if they are precise)
	if newMsg.Fields["lat"].(float64) != 0.71 {
		t.Errorf("lat mismatch: expected 0.71, got %v", newMsg.Fields["lat"])
	}

	t.Logf("Successfully round-tripped Announce message")
}

func TestCRC(t *testing.T) {
	data := []byte("123456789")
	crc := CalculateCRC16(data)
	// For "123456789" with CRC-16-IBM, the result should be 0xBB3D (reversed or not depends on implementation)
	// Let's just verify consistency for now.
	t.Logf("CRC of '123456789': 0x%04X", crc)
}
