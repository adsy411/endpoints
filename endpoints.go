// Copyright 2016 Google Inc. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	endpointsPath      = "/api/v1/namespaces/%s/endpoints/%s"
	endpointsWatchPath = "/api/v1/watch/namespaces/%s/endpoints/%s"
)

const (
	defaultSyncInterval = 30 * time.Second
	defaultRetryDelay   = 5 * time.Second
)

var (
	DefaultAPIAddr   = "127.0.0.1:8001"
	DefaultNamespace = "default"
)

var (
	// ErrNoEndpoints is returned by LoadBalancer.Next calls when a named service
	// has no backends.
	ErrNoEndpoints = errors.New("endpoints: no endpoints available")

	// ErrMissingServiceName is returned by SyncEndpoints and StartBackgoundSync
	// when attempting to synchronize endpoints with a config that contains a
	// missing or blank service name.
	ErrMissingServiceName = errors.New("endpoints: missing service name")
)

// Endpoint holds a Kubernetes endpoint.
type Endpoint struct {
	Host  string
	Port  string
	Ports map[string]string
}

// A Config structure is used to configure a LoadBalancer.
type Config struct {
	// APIAddr specifies the Kubernetes API "IP:port" address to use
	// when synchronizing enpoints. If empty, DefaultAPIAddr is used.
	APIAddr string

	// The http.Client used to perform requests to the Kubernetes API.
	// If nil, http.DefaultClient is used. Using the http.DefaultClient
	// will require the use of kubectl running in proxy mode:
	//
	//    $ kubectl proxy
	//    Starting to serve on 127.0.0.1:8001
	//
	// For more advanced communication schemes clients must provide an
	// http.Client with a custom transport to handle any authentication
	// requirements or custom behavior.
	Client *http.Client

	// ErrorLog specifies an optional logger for errors that occur when
	// attempting to sync endpoints. If nil, logging goes to os.Stderr via
	// the log package's standard logger.
	ErrorLog *log.Logger

	// The Kubernetes namespace to search for services.
	// If empty, DefaultNamespace is used.
	Namespace string

	// RetryDelay is the amount of time to wait between API calls after an error
	// occurs. If empty, DefaultRetryDelay is used.
	RetryDelay time.Duration

	// The Kubernetes service to monitor.
	Service string

	// SyncInterval is the amount of time between request to reconcile the list
	// of endpoint backends from Kubernetes.
	SyncInterval time.Duration
}

// LoadBalancer represents a Kubernetes endpoints round-robin load balancer.
type LoadBalancer struct {
	apiAddr      string
	client       *http.Client
	errorLog     *log.Logger
	namespace    string
	retryDelay   time.Duration
	service      string
	syncInterval time.Duration
	quit         chan struct{}
	wg           sync.WaitGroup

	mu              sync.RWMutex // protects currentEndpoint and endpoints
	currentEndpoint int
	endpoints       []Endpoint
}

// New configures and returns a new *LoadBalancer. The LoadBalancer endpoints
// list is populated by the Sync and StartBackgroundSync methods.
func New(config *Config) *LoadBalancer {
	config.setDefaults()

	return &LoadBalancer{
		apiAddr:      config.APIAddr,
		client:       config.Client,
		errorLog:     config.ErrorLog,
		namespace:    config.Namespace,
		retryDelay:   config.RetryDelay,
		service:      config.Service,
		syncInterval: config.SyncInterval,
		quit:         make(chan struct{}),
	}
}

// Endpoints returns a copy of the current set of endpoints.
func (lb *LoadBalancer) Endpoints() []Endpoint {
	lb.mu.RLock()
	eps := make([]Endpoint, len(lb.endpoints))
	copy(eps, lb.endpoints)
	lb.mu.RUnlock()
	return eps
}

// Next returns the next Kubernetes endpoint.
func (lb *LoadBalancer) Next() (Endpoint, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if len(lb.endpoints) <= 0 {
		return Endpoint{}, ErrNoEndpoints
	}
	if lb.currentEndpoint >= len(lb.endpoints) {
		lb.currentEndpoint = 0
	}
	endpoint := lb.endpoints[lb.currentEndpoint]
	lb.currentEndpoint++
	return endpoint, nil
}

// Shutdown shuts down the loadbalancer. Shutdown works by stopping
// any watches and reconciliation loops against the Kubernetes API
// server.
func (lb *LoadBalancer) Shutdown() error {
	close(lb.quit)
	lb.wg.Wait()
	return nil
}

// SyncEndpoints syncs the endpoints for the configured Kubernetes service.
func (lb *LoadBalancer) SyncEndpoints() error {
	if lb.service == "" {
		return ErrMissingServiceName
	}
	return lb.syncEndpoints()
}

// StartBackgroundSync starts a watch loop that synchronizes the list of
// endpoints asynchronously.
func (lb *LoadBalancer) StartBackgroundSync() error {
	if lb.service == "" {
		return ErrMissingServiceName
	}

	// Start watch loop.
	lb.wg.Add(1)
	go lb.watchEndpoints()

	// Start reconciliation loop.
	lb.wg.Add(1)
	go lb.reconcile()

	return nil
}

func (c *Config) setDefaults() {
	if c.APIAddr == "" {
		c.APIAddr = DefaultAPIAddr
	}
	if c.Client == nil {
		c.Client = http.DefaultClient
	}
	if c.ErrorLog == nil {
		c.ErrorLog = log.New(os.Stderr, "", log.LstdFlags)
	}
	if c.Namespace == "" {
		c.Namespace = DefaultNamespace
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = defaultRetryDelay
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = defaultSyncInterval
	}
}

func (lb *LoadBalancer) update(endpoints []Endpoint) {
	lb.mu.Lock()
	lb.endpoints = endpoints
	lb.mu.Unlock()
}

func (lb *LoadBalancer) reconcile() {
	defer lb.wg.Done()
	for {
		select {
		case <-time.After(lb.syncInterval):
			err := lb.syncEndpoints()
			if err != nil {
				lb.errorLog.Println(err)
			}
		case <-lb.quit:
			return
		}
	}
}

func (lb *LoadBalancer) syncEndpoints() error {
	var eps endpoints
	r, err := lb.get(context.TODO(), fmt.Sprintf(endpointsPath, lb.namespace, lb.service))
	if err != nil {
		return err
	}
	defer r.Close()

	err = json.NewDecoder(r).Decode(&eps)
	if err != nil {
		return err
	}

	lb.update(formatEndpoints(eps))
	return nil
}

func (lb *LoadBalancer) watchEndpoints() {
	defer lb.wg.Done()

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())

	wg.Add(1)
	go lb.watch(ctx, &wg)

	<-lb.quit
	cancel()
	wg.Wait()
}

func (lb *LoadBalancer) watch(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	path := fmt.Sprintf(endpointsWatchPath, lb.namespace, lb.service)

	for {
		r, err := lb.get(ctx, path)
		if ctx.Err() == context.Canceled {
			return
		}
		if err != nil {
			lb.errorLog.Println(err)
			time.Sleep(lb.retryDelay)
			continue
		}

		// endpoint watches return a stream of JSON objects which
		// must be processed one at a time to ensure consistency.
		decoder := json.NewDecoder(r)
		for {
			if ctx.Err() == context.Canceled {
				r.Close()
				return
			}

			var o object
			err := decoder.Decode(&o)
			if err != nil {
				lb.errorLog.Printf("endpoints watch %s: %s", path, err)
				r.Close()
				break
			}
			if o.Type == "ERROR" {
				lb.errorLog.Printf("endpoints watch %s: %s", path, o.Object.Message)
				r.Close()
				break
			}
			lb.update(formatEndpoints(o.Object))
		}
	}
}

// SyncError records an error during an Kubernetes API call and the HTTP
// request that caused it.
type SyncError struct {
	URL     string // URL used
	Message string // description of the error
	Code    int    // remote status code
}

func (e *SyncError) Error() string {
	return fmt.Sprintf("endpoints Get %s: %s %d", e.URL, e.Message, e.Code)
}

func (lb *LoadBalancer) get(ctx context.Context, path string) (io.ReadCloser, error) {
	r := &http.Request{
		Header: make(http.Header),
		Method: http.MethodGet,
		URL: &url.URL{
			Host:   lb.apiAddr,
			Path:   path,
			Scheme: "http",
		},
	}
	r.Header.Set("Accept", "application/json, */*")

	url := r.URL.String()

	resp, err := lb.client.Do(r.WithContext(ctx))
	if err != nil {
		return nil, errors.New("endpoints: " + err.Error())
	}

	if resp.StatusCode != 200 {
		d, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, &SyncError{url, err.Error(), resp.StatusCode}
		}

		// Decode the remote error.
		var s status
		err = json.Unmarshal(d, &s)
		if err != nil {
			return nil, &SyncError{url, err.Error(), resp.StatusCode}
		}
		return nil, &SyncError{url, s.Message, s.Code}
	}

	return resp.Body, nil
}

func formatEndpoints(endpoints endpoints) []Endpoint {
	eps := make([]Endpoint, 0)
	if len(endpoints.Subsets) == 0 {
		return eps
	}

	port := ""
	ports := make(map[string]string)
	if len(endpoints.Subsets[0].Ports) > 0 {
		port = strconv.FormatInt(int64(endpoints.Subsets[0].Ports[0].Port), 10)
		for _, p := range endpoints.Subsets[0].Ports {
			if p.Name != "" {
				ports[p.Name] = strconv.FormatInt(int64(p.Port), 10)
			}
		}
	}

	for _, address := range endpoints.Subsets[0].Addresses {
		ep := Endpoint{
			Host:  address.IP,
			Port:  port,
			Ports: ports,
		}
		eps = append(eps, ep)
	}
	return eps
}
