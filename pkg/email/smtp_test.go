package email

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
)

// TestSMTPSender_QuitFailureAfterAcceptIsSuccess proves a connection dropped
// on QUIT, after the server accepted DATA, is not reported as a send failure;
// otherwise the email job would retry and deliver a duplicate.
func TestSMTPSender_QuitFailureAfterAcceptIsSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			accepted <- false
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		r := bufio.NewReader(conn)
		write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		write("220 test ESMTP")
		gotData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				accepted <- gotData
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				write("250 test")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				write("250 ok")
			case cmd == "DATA":
				write("354 go ahead")
				for {
					body, err := r.ReadString('\n')
					if err != nil {
						accepted <- false
						return
					}
					if body == ".\r\n" {
						break
					}
				}
				gotData = true
				write("250 queued")
			case cmd == "QUIT":
				// Drop the connection without replying.
				accepted <- gotData
				return
			default:
				write("502 unsupported")
			}
		}
	}()

	addr := ln.Addr().(*net.TCPAddr)
	sender := NewSMTPSender(config.EmailConfig{
		SMTPHost:    "127.0.0.1",
		SMTPPort:    addr.Port,
		FromAddress: "noreply@example.com",
	})
	err = sender.Send(context.Background(), Message{
		To:      []string{"user@example.com"},
		Subject: "hello",
		Body:    "body",
	})
	if err != nil {
		t.Fatalf("Send returned %v after the server accepted the message", err)
	}
	if !<-accepted {
		t.Fatal("server did not accept the message")
	}
}
