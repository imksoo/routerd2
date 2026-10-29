// SPDX-License-Identifier: BSD-3-Clause

// routerd-dhcp-event forwards dnsmasq's direct hook to its owning worker.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/imksoo/routerd2/internal/protocol/dhcpevent"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 3 {
		return errors.New("DHCP event requires action, MAC and address")
	}
	event := dhcpevent.Event{Action: args[0], MAC: args[1], IP: args[2], At: time.Now().UTC(), FromFile: os.Getenv("DNSMASQ_DATA_MISSING") == "1"}
	if len(args) > 3 && args[3] != "*" {
		event.Hostname = args[3]
	}
	conn, err := net.DialTimeout("unix", os.Getenv("ROUTERD_DHCP_EVENT_SOCKET"), time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(6 * time.Second)); err != nil {
		return err
	}
	if err := json.NewEncoder(conn).Encode(event); err != nil {
		return err
	}
	var reply dhcpevent.Reply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return err
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	return nil
}
