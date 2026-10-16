// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// The ingress-direction run (ingress.sh): which Pods may open a connection
// to a slot Pod. One Pod in a slot listens; client Pods elsewhere each try to
// connect to it with the same burst the egress run uses, and the script
// judges each client's summary against what the slot NetworkPolicy should
// do: refuse an agent Pod and a release-namespace Pod without the gateway
// label, admit one with it.

// The ingress summaries.
const (
	outcomeBlocked      = "BLOCKED"
	outcomeReached      = "REACHED"
	outcomeInconclusive = "INCONCLUSIVE"
)

// listenAddr is the slot listener's address: its containerPort named http,
// the one port the slot policy admits the gateway on.
const listenAddr = ":8080"

// ingressMode runs `listen` or `connect <ipv4>:<port>`; false when args name
// neither, so main runs the egress burst as before.
func ingressMode(args []string) (handled bool, code int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "listen":
		return true, listen(listenAddr)
	case "connect":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: probe connect <ipv4>:<port>")
			return true, 2
		}
		t, err := connectTarget(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return true, 2
		}
		start := time.Now()
		reachable, inconclusive := burst(start, []target{t}, emit)
		emit(result{Name: "summary", Outcome: ingressVerdict(reachable, inconclusive),
			ElapsedMS: time.Since(start).Milliseconds()})
		return true, 0
	}
	return false, 0
}

// connectTarget is the slot listener's address: an IPv4 address and a port,
// never a name.
func connectTarget(addr string) (target, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return target{}, fmt.Errorf("connect target %q: %w", addr, err)
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || ip.To4() == nil || err != nil || n < 1 || n > 65535 {
		return target{}, fmt.Errorf("connect target %q is not <ipv4>:<port>", addr)
	}
	return target{name: "slot-pod", addr: net.JoinHostPort(ip.String(), port)}, nil
}

// ingressVerdict is a client's summary: REACHED when any attempt connected,
// BLOCKED when every attempt timed out or was unreachable, INCONCLUSIVE
// otherwise (a refusal says nothing about policy: the listener might be
// down).
func ingressVerdict(reachable, inconclusive bool) string {
	switch {
	case reachable:
		return outcomeReached
	case inconclusive:
		return outcomeInconclusive
	}
	return outcomeBlocked
}

// listen serves 200 on every path until the Pod is deleted.
func listen(addr string) int {
	srv := &http.Server{Addr: addr, Handler: okHandler(), ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("netprobe\n"))
	})
}
