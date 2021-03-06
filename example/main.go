// Copyright 2016 Google Inc. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kelseyhightower/endpoints"
)

var (
	namespace string
	service   string
)

func main() {
	flag.StringVar(&namespace, "namespace", "default", "The Kubernetes namespace")
	flag.StringVar(&service, "service", "", "The Kubernetes service name")
	flag.Parse()

	config := &endpoints.Config{
		Namespace: namespace,
		Service:   service,
	}

	lb := endpoints.New(config)

	if err := lb.SyncEndpoints(); err != nil {
		log.Fatal(err)
	}

	if err := lb.StartBackgroundSync(); err != nil {
		log.Fatal(err)
	}

	go func() {
		c := http.Client{Timeout: time.Second}
		for {
			endpoint, err := lb.Next()
			if err != nil {
				log.Println(err)
				time.Sleep(time.Second)
				continue
			}
			urlStr := fmt.Sprintf("http://%s:%s", endpoint.Host, endpoint.Port)
			resp, err := c.Get(urlStr)
			if err != nil {
				log.Println(err)
				continue
			}
			resp.Body.Close()
			log.Printf("Endpoint %s response code: %s", urlStr, resp.Status)
			time.Sleep(time.Second)
		}
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	<-signalChan
	log.Printf("Shutdown signal received, exiting...")
	lb.Shutdown()
	os.Exit(0)
}
