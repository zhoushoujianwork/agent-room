package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	baseURL := flag.String("base-url", "http://127.0.0.1:8080", "URL reachable by all agents")
	dbPath := flag.String("db", "data/board.db", "new message board SQLite file")
	flag.Parse()
	u, err := url.Parse(*baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("base-url must be an absolute http(s) origin without credentials, path, query or fragment")
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
		return err
	}
	s, err := newServer(*dbPath, strings.TrimRight(*baseURL, "/"))
	if err != nil {
		return err
	}
	defer s.db.Close()
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("Agent Room listening on %s; board: %s; agent guide: %s/api/agent-guide", listener.Addr(), s.baseURL, s.baseURL)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
