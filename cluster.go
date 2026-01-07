package tcredis

import (
	"fmt"
	"strings"
)

// buildAddrParams builds query parameters for additional addresses
func buildAddrParams(addrs []string) string {
	var params []string
	for _, addr := range addrs {
		params = append(params, fmt.Sprintf("addr=%s", addr))
	}
	return strings.Join(params, "&")
}
