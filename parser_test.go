package imc

import (
	"testing"
)

func TestParseXML(t *testing.T) {
	proto, err := ParseXML("IMC.xml")
	if err != nil {
		t.Fatalf("Failed to parse IMC.xml: %v", err)
	}

	if proto.Name != "IMC" {
		t.Errorf("Expected name 'IMC', got '%s'", proto.Name)
	}

	if len(proto.Messages) == 0 {
		t.Error("Expected messages, got none")
	}

	// Check a specific message
	found := false
	for _, msg := range proto.Messages {
		if msg.Abbrev == "Announce" {
			found = true
			if msg.ID != 151 {
				t.Errorf("Expected Announce ID 151, got %d", msg.ID)
			}
			break
		}
	}
	if !found {
		t.Error("Did not find Announce message")
	}

	t.Logf("Parsed %d messages from IMC version %s", len(proto.Messages), proto.Version)
}
