package stream

import (
	"errors"
	"fmt"
	"net"
)

// LocalAddressFor reports the address on this machine that can reach the
// given host -- the one to hand a receiver.
//
// No packet is sent: connecting a UDP socket only asks the routing table which
// interface would be used. That is the question being asked, and it answers it
// without depending on the receiver being up.
func LocalAddressFor(host string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return "", fmt.Errorf("no route to %s: %w", host, err)
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", errors.New("could not determine the local address")
	}
	return addr.IP.String(), nil
}
