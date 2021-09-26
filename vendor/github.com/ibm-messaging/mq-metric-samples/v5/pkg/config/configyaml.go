package config

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

// This package provides a set of common routines that can used by all the
// sample metric monitor programs to get the configuration from a YAML file.
// Settings in that file can be overridden on the command line or via environment variable
import (
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
)

type ConfigYGlobal struct {
	UseObjectStatus    bool   `yaml:"useObjectStatus" default:true`
	UseResetQStats     bool   `yaml:"useResetQStats" default:false`
	UsePublications    bool   `yaml:"usePublications" default:true`
	LogLevel           string `yaml:"logLevel"`
	MetaPrefix         string
	PollInterval       string `yaml:"pollInterval"`
	RediscoverInterval string `yaml:"rediscoverInterval"`
	TZOffset           string `yaml:"tzOffset"`
	Locale             string
}
type ConfigYConnection struct {
	QueueManager string `yaml:"queueManager"`
	User         string
	Client       bool `yaml:"clientConnection"`
	Password     string
	ReplyQueue   string `yaml:"replyQueue"`
	CcdtUrl      string `yaml:"ccdtUrl"`
	ConnName     string `yaml:"connName"`
	Channel      string `yaml:"channel"`
}
type ConfigYObjects struct {
	Queues                    []string
	QueueSubscriptionSelector []string `yaml:"queueSubscriptionSelector"`
	Channels                  []string
	Topics                    []string
	Subscriptions             []string
	ShowInactiveChannels      bool `yaml:"showInactiveChannels"`
}

func ReadConfigFile(f string, cmy interface{}) error {

	data, e2 := ioutil.ReadFile(f)
	if e2 == nil {
		e2 = yaml.Unmarshal(data, cmy)
	}

	return e2
}

// This handles the configuration parameters that are common to all the collectors. The individual
// collectors call similar code for their own specific attributes
func CopyYamlConfig(cm *Config, cyg ConfigYGlobal, cyc ConfigYConnection, cyo ConfigYObjects) {
	cm.CC.UseStatus = CopyParmIfNotSetBool("global", "useObjectStatus", cyg.UseObjectStatus)
	cm.CC.UseResetQStats = CopyParmIfNotSetBool("global", "useResetQStats", cyg.UseResetQStats)
	cm.CC.UsePublications = CopyParmIfNotSetBool("global", "usePublications", cyg.UsePublications)
	cm.CC.ShowInactiveChannels = CopyParmIfNotSetBool("objects", "showInactiveChannels", cyo.ShowInactiveChannels)

	cm.LogLevel = CopyParmIfNotSetStr("global", "logLevel", cyg.LogLevel)
	cm.MetaPrefix = CopyParmIfNotSetStr("global", "metaprefix", cyg.MetaPrefix)
	cm.pollInterval = CopyParmIfNotSetStr("global", "pollInterval", cyg.PollInterval)
	cm.rediscoverInterval = CopyParmIfNotSetStr("global", "rediscoverInterval", cyg.RediscoverInterval)
	cm.TZOffsetString = CopyParmIfNotSetStr("global", "tzOffset", cyg.TZOffset)
	cm.Locale = CopyParmIfNotSetStr("global", "locale", cyg.Locale)

	cm.QMgrName = CopyParmIfNotSetStr("connection", "queueManager", cyc.QueueManager)
	cm.CC.CcdtUrl = CopyParmIfNotSetStr("connection", "ccdtUrl", cyc.CcdtUrl)
	cm.CC.ConnName = CopyParmIfNotSetStr("connection", "connName", cyc.ConnName)
	cm.CC.Channel = CopyParmIfNotSetStr("connection", "channel", cyc.Channel)
	cm.CC.ClientMode = CopyParmIfNotSetBool("connection", "clientConnection", cyc.Client)
	cm.CC.UserId = CopyParmIfNotSetStr("connection", "user", cyc.User)
	cm.CC.Password = CopyParmIfNotSetStr("connection", "password", cyc.Password)
	cm.ReplyQ = CopyParmIfNotSetStr("connection", "replyQueue", cyc.ReplyQueue)

	cm.MonitoredQueues = CopyParmIfNotSetStrArray("objects", "queues", cyo.Queues)
	cm.MonitoredChannels = CopyParmIfNotSetStrArray("objects", "channels", cyo.Channels)
	cm.MonitoredTopics = CopyParmIfNotSetStrArray("objects", "topics", cyo.Topics)
	cm.MonitoredSubscriptions = CopyParmIfNotSetStrArray("objects", "subscriptions", cyo.Subscriptions)
	cm.QueueSubscriptionSelector = CopyParmIfNotSetStrArray("objects", "queueSubscriptionSelector", cyo.QueueSubscriptionSelector)

	return
}

// If the parameter has already been set by env var or cli, then the value in the main config structure returned. Otherwise
// the value passed as the "val" parameter - from the YAML version of the configuration elements - is returned
func CopyParmIfNotSetBool(section string, name string, val bool) bool {
	v, s := copyParmIfNotSet(section, name)
	if s {
		return *(v).(*bool)
	} else {
		return val
	}
}

func CopyParmIfNotSetStr(section string, name string, val string) string {
	v, s := copyParmIfNotSet(section, name)
	if s {
		return *(v).(*string)
	} else {
		return val
	}
}

func CopyParmIfNotSetStrArray(section string, name string, val []string) string {
	v, s := copyParmIfNotSet(section, name)
	if s {
		return *(v).(*string)
	} else {
		// Convert YAML arrays into the single string expected by the mqmetric package
		s := ""
		for i := 0; i < len(val); i++ {
			if i == 0 {
				s = val[0]
			} else {
				s += "," + val[i]
			}
		}
		return s
	}
}

func CopyParmIfNotSetInt(section string, name string, val int) int {
	v, s := copyParmIfNotSet(section, name)
	if s {
		return *(v).(*int)
	} else {
		return val
	}
}

// Gets the value set in the YAML structure, but only if it has not previously been
// set by the user via CLI or environment variable
//
// Debug of this is handled by direct Printfs as it's run before the logger is configured
func copyParmIfNotSet(section string, name string) (interface{}, bool) {
	k := envVarKey(section, name)
	if p, ok := configParms[k]; ok {
		if p.userSet {
			//fmt.Printf("Returning data from %v\n",p)
			return p.loc, true
		} else {
			//fmt.Printf("Key %s has not been set by user\n",k)
		}
	} else {
		// If this happens, it indicates a problem in one of the config.go files so we leave it in.
		fmt.Printf("Key %s not found in parms map\n", k)
	}
	return nil, false
}
