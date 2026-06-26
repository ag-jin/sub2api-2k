package kiro

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBuildHTTPClient_Socks5Egress(t *testing.T) {
	if os.Getenv("KIRO_PROXY_LIVE") != "1" {
		t.Skip("set KIRO_PROXY_LIVE=1 to run live proxy egress test")
	}
	proxy := os.Getenv("KIRO_TEST_PROXY")
	if proxy == "" {
		t.Fatal("KIRO_TEST_PROXY not set")
	}
	direct, _ := buildHTTPClient("", 15*time.Second)
	directIP := getIP(t, direct)
	viaProxy, err := buildHTTPClient(proxy, 15*time.Second)
	if err != nil {
		t.Fatalf("build proxy client: %v", err)
	}
	proxyIP := getIP(t, viaProxy)
	t.Logf("direct egress=%s  proxy egress=%s", directIP, proxyIP)
	if proxyIP == "" {
		t.Fatal("no egress IP via proxy")
	}
	if proxyIP == directIP {
		t.Errorf("proxy egress IP (%s) equals direct (%s) - proxy NOT applied!", proxyIP, directIP)
	}
}

func getIP(t *testing.T, c *http.Client) string {
	req, _ := http.NewRequest("GET", "https://api.ipify.org", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Logf("request error: %v", err)
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}
