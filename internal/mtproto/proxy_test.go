package mtproto

import "testing"

func TestNewMTProtoProxyResolver(t *testing.T) {
	resolver, err := newMTProtoProxyResolver("socks5://user:secret@127.0.0.1:7890")
	if err != nil {
		t.Fatalf("newMTProtoProxyResolver returned error: %v", err)
	}
	if resolver == nil {
		t.Fatal("newMTProtoProxyResolver returned a nil resolver")
	}
}

func TestMTProtoProxyDescriptionOmitsCredentials(t *testing.T) {
	scheme, address := mtProtoProxyDescription("socks5://user:secret@proxy.internal:7890")
	if scheme != "SOCKS5" || address != "proxy.internal:7890" {
		t.Fatalf("mtProtoProxyDescription = %q, %q", scheme, address)
	}
}

func TestMTProtoProxyDescriptionUsesDefaultPort(t *testing.T) {
	_, address := mtProtoProxyDescription("socks5h://proxy.internal")
	if address != "proxy.internal:1080" {
		t.Fatalf("mtProtoProxyDescription address = %q", address)
	}
}
