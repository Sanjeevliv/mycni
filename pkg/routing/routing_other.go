//go:build !linux

package routing

import (
	"errors"
	"net"
)

func EnsureDirectRoute(linkIndex int, dst *net.IPNet, gw net.IP) error {
	return errors.New("routing is only supported on Linux")
}

func DeleteDirectRoute(dst *net.IPNet) error {
	return nil
}
