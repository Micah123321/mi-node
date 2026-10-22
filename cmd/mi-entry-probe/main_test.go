package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/entryprobe"
)

func TestRunGenericErrors(t *testing.T) {
	for _, input := range []string{
		"{", "null", strings.Repeat("x", entryprobe.MaxInputBytes+1),
		`{"method":"password-must-not-appear","password":"sensitive-test-password","entries":[{"host":"127.0.0.1","port":1}],"timeout_seconds":1}`,
		`{"method":"2022-blake3-aes-128-gcm","password":"sensitive-test-password","entries":[{"host":"127.0.0.1","port":1}],"timeout_seconds":1}`,
		`{"password":"sensitive-test-password","entries":[{"host":"127.0.0.1","port":1}],"timeout_seconds":1}`,
	} {
		var output, stderr bytes.Buffer
		if code := run(strings.NewReader(input), &output, &stderr); code == 0 || output.Len() != 0 || stderr.String() != "invalid probe configuration\n" {
			t.Fatal("invalid input did not produce sanitized nonzero failure")
		}
	}
}

func TestRunOfflineJSON(t *testing.T) {
	input := `{"method":"aes-128-gcm","password":"sensitive-test-password","entries":[{"host":"127.0.0.1","port":1},{"host":"::1","port":1}],"timeout_seconds":1}`
	var output, stderr bytes.Buffer
	if code := run(strings.NewReader(input), &output, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatal("network error should not fail command")
	}
	want := `{"results":[{"host":"127.0.0.1","port":1,"online":false},{"host":"::1","port":1,"online":false}]}` + "\n"
	if output.String() != want {
		t.Fatal("incorrect JSON contract")
	}
	stderr.Reset()
	if code := run(strings.NewReader(input), failWriter{}, &stderr); code == 0 || stderr.String() != "cannot write probe result\n" {
		t.Fatal("output failure was not sanitized")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("sensitive output failure") }

func TestRunTCPOnlyEntry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot listen locally")
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
	var input, output, stderr bytes.Buffer
	input.WriteString(`{"method":"aes-128-gcm","password":"test-only-password","entries":[{"host":"127.0.0.1","port":`)
	input.WriteString(strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	input.WriteString(`}],"timeout_seconds":1}`)
	if code := run(&input, &output, &stderr); code != 0 || stderr.Len() != 0 || !strings.Contains(output.String(), `"online":false`) {
		t.Fatal("TCP-only entry CLI result incorrect")
	}
	<-done
}
