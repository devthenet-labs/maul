// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"fmt"
	"strings"
)

// PodTunnel is what a profile Job's agent runner reads of the profile's
// tunnel (step 3): whether the public Go route serves beside it, and the
// tunnel's hosts, so the prompt describes exactly the routes the pod has.
// The zero value is a Job without a tunnel.
type PodTunnel struct {
	// GoRoute is set when EnvRoutes lists the go route beside the tunnel.
	GoRoute bool
	// Hosts are the tunnel's hosts, sorted; none means no tunnel.
	Hosts []string
}

// ParsePodTunnel reads EnvRoutes' and EnvHosts' values as the jobs package
// sets them on a profile Job with tunnel hosts: routes exactly "tunnel" or
// "go,tunnel", and hosts comma-joined, each ValidHost, strictly ascending
// (sorted, no repeat), at most MaxTunnelHosts. Both empty is no tunnel;
// one without the other, or anything else, is an error: only the
// controller sets these, so a value that does not hold together is a
// configuration error, never a prompt.
func ParsePodTunnel(routes, hosts string) (PodTunnel, error) {
	switch {
	case routes == "" && hosts == "":
		return PodTunnel{}, nil
	case routes == "":
		return PodTunnel{}, fmt.Errorf("%s is set without %s", EnvHosts, EnvRoutes)
	case hosts == "":
		return PodTunnel{}, fmt.Errorf("%s is set without %s", EnvRoutes, EnvHosts)
	}
	var t PodTunnel
	switch routes {
	case RouteTunnel:
	case RouteGo + "," + RouteTunnel:
		t.GoRoute = true
	default:
		return PodTunnel{}, fmt.Errorf("%s: %q is neither %q nor %q", EnvRoutes, routes, RouteTunnel,
			RouteGo+","+RouteTunnel)
	}
	list := strings.Split(hosts, ",")
	if len(list) > MaxTunnelHosts {
		return PodTunnel{}, fmt.Errorf("%s lists %d hosts; at most %d", EnvHosts, len(list), MaxTunnelHosts)
	}
	for i, h := range list {
		if !ValidHost(h) {
			return PodTunnel{}, fmt.Errorf("%s: %q is not an exact lowercase host name", EnvHosts, h)
		}
		if i > 0 && h <= list[i-1] {
			return PodTunnel{}, fmt.Errorf("%s: %q is repeated or out of order (the list is sorted)", EnvHosts, h)
		}
	}
	t.Hosts = list
	return t, nil
}
