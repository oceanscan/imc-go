package main

import (
	"fmt"
	"imc-go"
	"log"
	"sync"
)

func listen(proto *imc.Protocol, port int, group string) {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	trans, err := imc.NewUDPTransporter(proto, addr)
	if err != nil {
		log.Printf("[Port %d] Failed to create transporter: %v", port, err)
		return
	}
	defer trans.Close()

	groupAddr := fmt.Sprintf("%s:%d", group, port)
	if err := trans.JoinMulticast(groupAddr); err != nil {
		log.Printf("[Port %d] Failed to join multicast group %s: %v", port, groupAddr, err)
		return
	}

	fmt.Printf("[Port %d] Listening for multicast messages on %s...\n", port, group)

	for {
		msg, src, err := trans.Receive()
		if err != nil {
			log.Printf("[Port %d] Receive error: %v", port, err)
			continue
		}

		msgAbbrev := proto.Messages[msg.Header.MGID].Abbrev
		fmt.Printf("[Port %d] Received %s from %v\n", port, msgAbbrev, src)

		if msgAbbrev == "Announce" {
			fmt.Printf("  SysName: %v, SysType: %v, Lat: %v, Lon: %v\n",
				msg.Fields["sys_name"], msg.Fields["sys_type"], msg.Fields["lat"], msg.Fields["lon"])
		}
	}
}

func main() {
	xmlProto, err := imc.ParseXML("IMC.xml")
	if err != nil {
		log.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	group := "224.0.75.69"
	ports := []int{30100, 30101}

	var wg sync.WaitGroup
	for _, port := range ports {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			listen(proto, p, group)
		}(port)
	}

	wg.Wait()
}
