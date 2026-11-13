// fake-tailcat stands in for `tailcat socks` in tests: it parses
// --listen=host:port and runs a minimal SOCKS5 server that accepts any
// CONNECT, replies success, and echoes data back.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	// With FAKE_TAILCAT_IGNORE_TERM set the process shrugs off SIGTERM,
	// letting tests pin the SIGKILL leg of the supervisor's shutdown ladder.
	if os.Getenv("FAKE_TAILCAT_IGNORE_TERM") != "" {
		signal.Ignore(syscall.SIGTERM)
	}
	// Like the real `tailcat socks --listen=...`, the subcommand comes before
	// the flag; Go's flag package stops at the first non-flag argument, so
	// drop a leading "socks" before parsing.
	if len(os.Args) > 1 && os.Args[1] == "socks" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	listen := flag.String("listen", "127.0.0.1:0", "listen address host:port")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fake tailcat socks listening on %s", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return
	}
	io.ReadFull(conn, make([]byte, hdr[1]))
	conn.Write([]byte{0x05, 0x00})

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	switch req[3] {
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(conn, l)
		io.ReadFull(conn, make([]byte, l[0]))
	case 0x01:
		io.ReadFull(conn, make([]byte, 4))
	case 0x04:
		io.ReadFull(conn, make([]byte, 16))
	}
	io.ReadFull(conn, make([]byte, 2))

	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	io.Copy(conn, conn) // echo
}
