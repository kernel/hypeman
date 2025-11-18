package main

import (
	"bufio"
	"fmt"
	"net"
)

func main() {
	conn, err := net.Dial("unix", "/tmp/repro-vsock.sock")
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "CONNECT 2222\n")
	
	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		fmt.Printf("Response: %s\n", scanner.Text())
	} else {
		fmt.Printf("No response, error: %v\n", scanner.Err())
	}
}

