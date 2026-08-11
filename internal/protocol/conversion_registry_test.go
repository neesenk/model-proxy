package protocol

import "testing"

func TestProtocolConversionRegistryIsComplete(t *testing.T) {
	want := len(supportedWireProtocols) * (len(supportedWireProtocols) - 1)
	if len(protocolConversions) != want {
		t.Fatalf("conversion registry has %d pairs, want %d", len(protocolConversions), want)
	}

	for _, client := range supportedWireProtocols {
		for _, backend := range supportedWireProtocols {
			conversion, ok := lookupProtocolConversion(string(client), string(backend))
			if client == backend {
				if ok {
					t.Errorf("same-protocol pair %s unexpectedly registered", client)
				}
				continue
			}
			if !ok {
				t.Errorf("missing conversion pair %s -> %s", client, backend)
				continue
			}
			if conversion.request == nil || conversion.response == nil || conversion.stream == nil {
				t.Errorf("incomplete conversion pair %s -> %s: %+v", client, backend, conversion)
			}
		}
	}
}

func TestProtocolConversionRegistryRejectsUnknownProtocols(t *testing.T) {
	for _, pair := range [][2]string{
		{"", "responses"},
		{"anthropic", ""},
		{"unknown", "openai"},
		{"openai", "unknown"},
	} {
		if _, ok := lookupProtocolConversion(pair[0], pair[1]); ok {
			t.Errorf("lookupProtocolConversion(%q, %q) unexpectedly succeeded", pair[0], pair[1])
		}
		if needsConversion(pair[0], pair[1]) {
			t.Errorf("needsConversion(%q, %q)=true for unknown protocol", pair[0], pair[1])
		}
	}
}
