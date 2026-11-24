package main

import (
	"strings"
	"testing"
)

// Строки /proc/net/route; адреса — little-endian hex: FD01F40A = 10.244.1.253 (cilium_host).
const (
	routeHeader  = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	routeDefault = "eth0\t00000000\tFD01F40A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	routeOnLink  = "eth0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n"
	routeGwHost  = "eth0\tFD01F40A\t00000000\t0005\t0\t0\t0\tFFFFFFFF\t0\t0\t0\n"
)

func TestDefaultGateway(t *testing.T) {
	for name, tc := range map[string]struct{ table, want string }{
		"cilium pod":              {routeHeader + routeDefault + routeGwHost, "10.244.1.253"},
		"no default route":        {routeHeader + routeGwHost, ""},
		"default without gateway": {routeHeader + routeOnLink + routeGwHost, ""},
	} {
		gw, err := defaultGateway(strings.NewReader(tc.table))
		if gw != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("%s: defaultGateway = %q, %v", name, gw, err)
		}
	}
}
