package main

import (
	"fmt"
	"imc-go"
	"log"
	"time"
)

func main() {
	xmlProto, err := imc.ParseXML("IMC.xml")
	if err != nil {
		log.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	// Listener
	listener, err := imc.NewUDPTransporter(proto, "0.0.0.0:6002")
	if err != nil {
		log.Fatalf("Failed to create listener: %v", err)
	}
	defer listener.Close()

	fmt.Println("Listening for IMC messages on :6002...")

	// Sender in a goroutine
	go func() {
		sender, err := imc.NewUDPTransporter(proto, "0.0.0.0:0")
		if err != nil {
			log.Fatalf("Failed to create sender: %v", err)
		}
		defer sender.Close()

		time.Sleep(1 * time.Second)

		msg, err := proto.CreateMessage("Announce")
		if err != nil {
			log.Fatalf("Failed to create message: %v", err)
		}
		msg.Fields["sys_name"] = "GO-IMC-TEST"
		msg.Fields["sys_type"] = uint8(2)

		fmt.Println("Sending Announce message...")
		if err := sender.Send(msg, "127.0.0.1:6002"); err != nil {
			log.Fatalf("Failed to send message: %v", err)
		}
	}()

	// Receive
	for {
		msg, addr, err := listener.Receive()
		if err != nil {
			log.Printf("Error receiving: %v", err)
			continue
		}

		fmt.Printf("Received %s message from %v\n", proto.Messages[msg.Header.MGID].Abbrev, addr)
		if msg.Header.MGID == 151 { // Announce
			fmt.Printf("  SysName: %v\n", msg.Fields["sys_name"])
		}
		break // Exit after one message for verification
	}
}
