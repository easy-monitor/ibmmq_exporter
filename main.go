package main

/*
  Copyright (c) IBM Corporation 2016, 2021

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific

   Contributors:
     Mark Taylor - Initial Contribution
*/

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ibm-messaging/mq-golang/v5/ibmmq"
	"github.com/ibm-messaging/mq-golang/v5/mqmetric"
	cf "github.com/ibm-messaging/mq-metric-samples/v5/pkg/config"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"strings"
	"sync"
)

var (
	BuildStamp     string
	GitCommit      string
	BuildPlatform  string
	discoverConfig mqmetric.DiscoverConfig
	usingTLS       = false
	server         *http.Server
	startChannel   = make(chan bool)
	collector      prometheus.Collector
	mutex          sync.RWMutex
	retryCount     = 0 // Might use this with a maxRetry to force a quit out of collector
)

func main() {
	var err error
	cf.PrintInfo("IBM MQ metrics exporter for Prometheus monitoring", BuildStamp, GitCommit, BuildPlatform)

	err = initConfig()
	config.cf.QMgrName = "d1a6082f9ec5"

	//if err == nil && config.cf.QMgrName == "" {
	//	log.Errorln("Must provide a queue manager name to connect to.")
	//	os.Exit(72) // Same as strmqm "queue manager name error"
	//}

	if err != nil {
		log.Error(err)
	}
	log.Info("Done.")

	if err != nil {
		os.Exit(10)
	}

	// Start the webserver in a separate thread
	startServer()
}

func handler(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	qmgr_name := query.Get("qmgr_name")
	channel := query.Get("channel")
	conn_name := query.Get("conn_name")

	if qmgr_name == "" || channel == "" || conn_name == "" {
		http.Error(w, "missing parameter: qmgr_name, channel, conn_name", 400)
		return
	}

	moduleName := query.Get("module")
	if moduleName == "" {
		moduleName = "default"
	}

	conf, err := loadConfig()
	if err != nil {
		e := fmt.Sprintf("get config file failed, err: ", err)
		http.Error(w, e, 400)
		return
	}
	var user_id string
	var password string
	isBreak := false
	for i := 0; i < len(conf.Modules); i++ {
		if conf.Modules[i].Name == moduleName {
			user_id = conf.Modules[i].UserId
			password = conf.Modules[i].Password
			isBreak = true
			break
		}
	}
	if !isBreak {
		e := fmt.Sprintf("module not found in conf.yml")
		http.Error(w, e, 400)
		return
	}

	//config.cf.QMgrName = "d1a6082f9ec5"
	config.cf.QMgrName = qmgr_name

	setConnectedOnce(false)
	setConnectedQMgr(true)
	setCollectorEnd(false)
	//setFirstCollection(true)
	setFirstCollection(false)
	setCollectorSilent(false)

	// This is the main loop that tries to keep the collector connected to a queue manager
	// even after a failure.
	log.Debugf("In main loop: qMgrConnected=%v", isConnectedQMgr())
	err = nil // Start clean on each loop

	// The callback will set this flag to false if there's an error while
	// processing the messages.

	//mutex.Lock()
	if err == nil {
		mqmetric.EndConnection()
		// Connect and open standard queues. If we're going to manage reconnection from
		// this collector, then turn off the MQ client automatic option
		if config.keepRunning {
			config.cf.CC.SingleConnect = true
		} else {
			config.cf.CC.SingleConnect = false
		}

		config.cf.CC.Channel = channel
		config.cf.CC.ConnName = conn_name
		config.cf.CC.UserId = user_id
		config.cf.CC.Password = password
		config.cf.CC.UseStatus = true
		//config.cf.CC.ClientMode = true

		err = mqmetric.InitConnection(config.cf.QMgrName, config.cf.ReplyQ, &config.cf.CC)
		if err == nil {
			log.Infoln("Connected to queue manager " + config.cf.QMgrName)
		} else {
			if mqe, ok := err.(mqmetric.MQMetricError); ok {
				mqrc := mqe.MQReturn.MQRC
				mqcc := mqe.MQReturn.MQCC
				if mqrc == ibmmq.MQRC_STANDBY_Q_MGR {
					log.Errorln(err)
					os.Exit(30) // This is the same as the strmqm return code for "active instance running elsewhere"
				} else if mqcc == ibmmq.MQCC_WARNING {
					log.Infoln("Connected to queue manager " + config.cf.QMgrName)
					log.Errorln(err)
					err = nil
				}
			}
		}
	}

	if err == nil {
		retryCount = 0
		defer mqmetric.EndConnection()
	}

	// What metrics can the queue manager provide? Find out, and subscribe.
	if err == nil {
		// Do we need to expand wildcarded queue names
		// or use the wildcard as-is in the subscriptions
		wildcardResource := true
		if config.cf.MetaPrefix != "" {
			wildcardResource = false
		}

		mqmetric.SetLocale(config.cf.Locale)
		discoverConfig.MonitoredQueues.ObjectNames = config.cf.MonitoredQueues
		discoverConfig.MonitoredQueues.SubscriptionSelector = strings.ToUpper(config.cf.QueueSubscriptionSelector)
		discoverConfig.MonitoredQueues.UseWildcard = wildcardResource
		discoverConfig.MetaPrefix = config.cf.MetaPrefix
		err = mqmetric.DiscoverAndSubscribe(discoverConfig)
		mqmetric.RediscoverAttributes(ibmmq.MQOT_CHANNEL, config.cf.MonitoredChannels)
	}

	if err == nil {
		var compCode int32
		compCode, err = mqmetric.VerifyConfig()
		// We could choose to fail after a warning, but instead will continue for now
		if compCode == ibmmq.MQCC_WARNING {
			log.Println(err)
			err = nil
		}
	}


	// Once everything has been discovered, and the subscriptions
	// created, allocate the Prometheus gauges for each resource. If this is
	// a reconnect, then we clean up and create a new collector with new gauges
	if err == nil {
		allocateAllGauges()

		if collector != nil {
			setCollectorSilent(true)
			prometheus.Unregister(collector)
			setCollectorSilent(false)
		}

		registry := prometheus.NewRegistry()
		collector = newExporter()
		//setFirstCollection(true)
		//prometheus.MustRegister(collector)
		registry.MustRegister(collector)
		gatherers := prometheus.Gatherers{prometheus.DefaultGatherer, registry}
		// Delegate http serving to Prometheus client library, which will call collector.Collect.
		h := promhttp.HandlerFor(gatherers, promhttp.HandlerOpts{})
		h.ServeHTTP(w, r)
	}
	//mutex.Unlock()
}

func loadConfig() (*ModuleConf, error) {
	path, _ := os.Getwd()
	path = filepath.Join(path, "conf/conf.yml")
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, errors.New("read conf.yml fail:" + path)
	}
	conf := new(ModuleConf)
	err = yaml.Unmarshal(data, conf)
	if err != nil {
		return nil, errors.New("unmarshal conf.yml fail")
	}
	return conf, nil
}

//func main() {
//	var err error
//	cf.PrintInfo("IBM MQ metrics exporter for Prometheus monitoring", BuildStamp, GitCommit, BuildPlatform)
//
//	err = initConfig()
//	config.cf.QMgrName = "d1a6082f9ec5"
//
//	if err == nil && config.cf.QMgrName == "" {
//		log.Errorln("Must provide a queue manager name to connect to.")
//		os.Exit(72) // Same as strmqm "queue manager name error"
//	}
//
//	if err != nil {
//		log.Error(err)
//	} else {
//		setConnectedOnce(false)
//		setConnectedQMgr(false)
//		setCollectorEnd(false)
//		setFirstCollection(false)
//		setCollectorSilent(false)
//
//		// Start the webserver in a separate thread
//		go startServer()
//
//		// This is the main loop that tries to keep the collector connected to a queue manager
//		// even after a failure.
//		for !isCollectorEnd() {
//			log.Debugf("In main loop: qMgrConnected=%v", isConnectedQMgr())
//			err = nil // Start clean on each loop
//
//			// The callback will set this flag to false if there's an error while
//			// processing the messages.
//			if !isConnectedQMgr() {
//				mutex.Lock()
//				if err == nil {
//					mqmetric.EndConnection()
//					// Connect and open standard queues. If we're going to manage reconnection from
//					// this collector, then turn off the MQ client automatic option
//					if config.keepRunning {
//						config.cf.CC.SingleConnect = true
//					} else {
//						config.cf.CC.SingleConnect = false
//					}
//					config.cf.CC.Channel = "DEV.ADMIN.SVRCONN"
//					config.cf.CC.ConnName = "192.168.88.134(49155)"
//					config.cf.CC.UserId = "admin"
//					config.cf.CC.Password = "passw0rd"
//					//config.cf.CC.ClientMode = true
//
//					err = mqmetric.InitConnection(config.cf.QMgrName, config.cf.ReplyQ, &config.cf.CC)
//					if err == nil {
//						log.Infoln("Connected to queue manager " + config.cf.QMgrName)
//					} else {
//						if mqe, ok := err.(mqmetric.MQMetricError); ok {
//							mqrc := mqe.MQReturn.MQRC
//							mqcc := mqe.MQReturn.MQCC
//							if mqrc == ibmmq.MQRC_STANDBY_Q_MGR {
//								log.Errorln(err)
//								os.Exit(30) // This is the same as the strmqm return code for "active instance running elsewhere"
//							} else if mqcc == ibmmq.MQCC_WARNING {
//								log.Infoln("Connected to queue manager " + config.cf.QMgrName)
//								log.Errorln(err)
//								err = nil
//							}
//						}
//					}
//				}
//
//				if err == nil {
//					retryCount = 0
//					defer mqmetric.EndConnection()
//				}
//
//				// What metrics can the queue manager provide? Find out, and subscribe.
//				if err == nil {
//					// Do we need to expand wildcarded queue names
//					// or use the wildcard as-is in the subscriptions
//					wildcardResource := true
//					if config.cf.MetaPrefix != "" {
//						wildcardResource = false
//					}
//
//					mqmetric.SetLocale(config.cf.Locale)
//					discoverConfig.MonitoredQueues.ObjectNames = config.cf.MonitoredQueues
//					discoverConfig.MonitoredQueues.SubscriptionSelector = strings.ToUpper(config.cf.QueueSubscriptionSelector)
//					discoverConfig.MonitoredQueues.UseWildcard = wildcardResource
//					discoverConfig.MetaPrefix = config.cf.MetaPrefix
//					err = mqmetric.DiscoverAndSubscribe(discoverConfig)
//					mqmetric.RediscoverAttributes(ibmmq.MQOT_CHANNEL, config.cf.MonitoredChannels)
//				}
//
//				if err == nil {
//					var compCode int32
//					compCode, err = mqmetric.VerifyConfig()
//					// We could choose to fail after a warning, but instead will continue for now
//					if compCode == ibmmq.MQCC_WARNING {
//						log.Println(err)
//						err = nil
//					}
//				}
//
//				// Once everything has been discovered, and the subscriptions
//				// created, allocate the Prometheus gauges for each resource. If this is
//				// a reconnect, then we clean up and create a new collector with new gauges
//				if err == nil {
//					allocateAllGauges()
//
//					if collector != nil {
//						setCollectorSilent(true)
//						prometheus.Unregister(collector)
//						setCollectorSilent(false)
//					}
//
//					collector = newExporter()
//					setFirstCollection(true)
//					prometheus.MustRegister(collector)
//					setConnectedQMgr(true)
//
//					if !isConnectedOnce() {
//						startChannel <- true
//						setConnectedOnce(true)
//					}
//				} else {
//					if !isConnectedOnce() || !config.keepRunning {
//						// If we've never successfully connected, then exit instead
//						// of retrying as it probably means a config error
//						log.Errorf("Connection to %s has failed. %v", config.cf.QMgrName, err)
//						setCollectorEnd(true)
//					} else {
//						log.Debug("Sleeping a bit after a failure")
//						retryCount++
//						time.Sleep(config.reconnectIntervalDuration)
//					}
//				}
//				mutex.Unlock()
//			} else {
//				log.Debug("Sleeping a bit while connected")
//				time.Sleep(config.reconnectIntervalDuration)
//			}
//		}
//	}
//	log.Info("Done.")
//
//	if err != nil {
//		os.Exit(10)
//	} else {
//		os.Exit(0)
//	}
//}

func startServer() {
	var err error
	// This function starts a new thread to handle the web server that will then run
	// permanently and drive the exporter callback that processes the metric messages

	// Need to wait until signalled by the main thread that it's setup the gauges
	log.Debug("HTTP server - waiting until MQ connection ready")
	//<-startChannel

	//http.Handle(config.httpMetricPath, promhttp.Handler())
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		handler(w, r)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(landingPage())
	})

	address := config.httpListenHost + ":" + config.httpListenPort
	if config.httpsKeyFile == "" && config.httpsCertFile == "" {
		usingTLS = false
	} else {
		usingTLS = true
	}

	if usingTLS {
		// TLS has been enabled for the collector (which is acting as a TLS Server)
		// So we setup the TLS configuration from the keystores and let Prometheus
		// contact us over the https protocol.
		cert, err := tls.LoadX509KeyPair(config.httpsCertFile, config.httpsKeyFile)
		if err == nil {
			server = &http.Server{Addr: address,
				Handler: nil,
				// More fields could be added here for further control of the connection
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{cert},
					MinVersion:   tls.VersionTLS12,
				},
			}
		} else {
			log.Fatal(err)
		}
	} else {
		server = &http.Server{Addr: address,
			Handler: nil,
		}
	}

	// And now we can start the protocol-appropriate server
	if usingTLS {
		log.Infoln("Listening on https address", address)
		err = server.ListenAndServeTLS("", "")
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Metrics Error: Failed to handle metrics request: %v", err)
			stopServer()
		}
	} else {
		log.Infoln("Listening on http address", address)
		err = server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("Metrics Error: Failed to handle metrics request: %v", err)
			stopServer()
		}
	}
}

// Shutdown HTTP server
func stopServer() {
	timeout, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := server.Shutdown(timeout)
	if err != nil {
		log.Errorf("Failed to shutdown metrics server: %v", err)
	}
	setCollectorEnd(true)
}

/*
landingPage gives a very basic response if someone just connects to our port.
The only link on it jumps to the list of available metrics.
*/
func landingPage() []byte {
	return []byte(
		`<html>
<head><title>IBM MQ metrics exporter for Prometheus</title></head>
<body>
<h1>IBM MQ metrics exporter for Prometheus</h1>
<p><a href='` + config.httpMetricPath + `'>Metrics</a></p>
</body>
</html>
`)
}
