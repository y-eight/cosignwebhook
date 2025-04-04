package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/golang/glog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	port = "8080"
	mport = "8081"
)

var (
	tlscert, tlskey string
	opsProcessed = promauto.NewCounter(prometheus.CounterOpts{
                Name: "cosign_processed_ops_total",
                Help: "The total number of processed events",
        })
	verifiedProcessed = promauto.NewCounter(prometheus.CounterOpts{
                Name: "cosign_processed_verified_total",
                Help: "The number of verfified events",
        })
)

func main() {
	flag.StringVar(&tlscert, "tlsCertFile", "/etc/certs/tls.crt", "File containing the x509 Certificate for HTTPS.")
	flag.StringVar(&tlskey, "tlsKeyFile", "/etc/certs/tls.key", "File containing the x509 private key to --tlsCertFile.")

	flag.Parse()

	certs, err := tls.LoadX509KeyPair(tlscert, tlskey)
	if err != nil {
		glog.Errorf("Filed to load key pair: %v", err)
	}

	server := &http.Server{
		Addr:      fmt.Sprintf(":%v", port),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certs}},
	}

	mserver := &http.Server{
		Addr:      fmt.Sprintf(":%v", mport),
	}

	// define http server and server handler
	cs := CosignServerHandler{}
	mux := http.NewServeMux()
	mux.HandleFunc("/validate", cs.serve)
	server.Handler = mux

	mmux := http.NewServeMux()
	mmux.HandleFunc("/healthz", cs.healthz)
	mmux.Handle("/metrics", promhttp.Handler())
	mserver.Handler = mmux

	// start webhook server in new rountine
	go func() {
		if err := server.ListenAndServeTLS("", ""); err != nil {
			glog.Errorf("Failed to listen and serve webhook server: %v", err)
		}
	}()
	go func() {
		if err := mserver.ListenAndServe(); err != nil {
			glog.Errorf("Failed to listen and serve minitor server: %v", err)
		}
	}()

	glog.Infof("Server running listening in port: %s,%s", port, mport)

	// listening shutdown singal
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan

	glog.Info("Got shutdown signal, shutting down webhook server gracefully...")
	server.Shutdown(context.Background())
	mserver.Shutdown(context.Background())
}
