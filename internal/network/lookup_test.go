package network

import "testing"

func TestRouteLookupPreservesScope(t *testing.T) {
	req := RouteLookupRequest{Target: "192.0.2.9", Interface: "wan1", Source: "192.0.2.2", Mark: 0x1234}
	ip, options, err := lookupInputs(req)
	if err != nil {
		t.Fatal(err)
	}
	if ip.String() != req.Target || options.Oif != req.Interface || options.SrcAddr.String() != req.Source || options.Mark != req.Mark || options.Iif != "" {
		t.Fatal("route lookup changed probe path", ip, options)
	}
	for _, req := range []RouteLookupRequest{
		{Target: "example.com", Interface: "wan1"},
		{Target: "192.0.2.9"},
		{Target: "192.0.2.9", Interface: "wan1", Source: "2001:db8::1"},
		{Target: "224.0.0.1", Interface: "wan1"},
	} {
		if _, _, err := lookupInputs(req); err == nil {
			t.Fatal("invalid scope accepted", req)
		}
	}
}
