package server

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestActivityConnTimesOutWhenIdle(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	conn := newActivityConn(serverConn)
	const timeout = 40 * time.Millisecond
	if err := conn.enable(timeout); err != nil {
		t.Fatalf("enable idle timeout: %v", err)
	}

	started := time.Now()
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("idle read returned without a timeout error")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("idle read error = %v, want network timeout", err)
	}
	if elapsed := time.Since(started); elapsed < timeout/2 {
		t.Fatalf("idle read timed out too early after %v", elapsed)
	}
}

func TestActivityConnReadExtendsDeadline(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	conn := newActivityConn(serverConn)
	const timeout = 100 * time.Millisecond
	if err := conn.enable(timeout); err != nil {
		t.Fatalf("enable idle timeout: %v", err)
	}

	go func() {
		delay := time.NewTimer(60 * time.Millisecond)
		defer delay.Stop()
		<-delay.C
		_, _ = clientConn.Write([]byte{'x'})
	}()

	started := time.Now()
	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		t.Fatalf("read activity: %v", err)
	}
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read after activity returned without a timeout error")
	}
	if elapsed := time.Since(started); elapsed < 130*time.Millisecond {
		t.Fatalf("activity did not extend deadline; total lifetime was %v", elapsed)
	}
}

func TestActivityConnWriteExtendsDeadline(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer func() { _ = serverConn.Close() }()
	defer func() { _ = clientConn.Close() }()

	conn := newActivityConn(serverConn)
	const timeout = 80 * time.Millisecond
	if err := conn.enable(timeout); err != nil {
		t.Fatalf("enable idle timeout: %v", err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(clientConn, make([]byte, 1))
		readDone <- err
	}()
	started := time.Now()
	if _, err := conn.Write([]byte{'x'}); err != nil {
		t.Fatalf("write activity: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read peer payload: %v", err)
	}
	_, err := conn.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read after write activity returned without a timeout error")
	}
	if elapsed := time.Since(started); elapsed < timeout/2 {
		t.Fatalf("write activity did not extend deadline; timeout followed after %v", elapsed)
	}
}
