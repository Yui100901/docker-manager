package pull

import (
	"strings"
	"testing"
)

func TestPullProxyRejectsUnsupportedSchemeBeforeRequest(t *testing.T) {
	_, err := proxyFuncFromSetting("ftp://proxy.example")
	if err == nil || !strings.Contains(err.Error(), "不支持 scheme") {
		t.Fatalf("proxyFuncFromSetting() error = %v, want unsupported scheme", err)
	}
}
