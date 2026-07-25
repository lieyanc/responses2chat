package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/lieyan/responses2chat/internal/gateway"
)

func main() {
	upstream := os.Getenv("UPSTREAM_CHAT_COMPLETIONS_URL")
	if upstream == "" {
		base := os.Getenv("UPSTREAM_BASE_URL")
		if base == "" {
			log.Fatal("UPSTREAM_CHAT_COMPLETIONS_URL or UPSTREAM_BASE_URL is required")
		}
		upstream = gateway.ChatCompletionsURL(base)
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	handler, err := gateway.New(gateway.Config{
		UpstreamURL: upstream,
		MaxBodySize: 32 << 20,
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 5 * time.Minute,
			},
		},
		Logger: log.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("responses2chat listening on %s, upstream=%s", addr, upstream)
	log.Fatal(server.ListenAndServe())
}
