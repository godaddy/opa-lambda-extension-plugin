// Copyright (c) 2021 GoDaddy Operating Company, LLC. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

// Package lambda implements a plugin to run OPA as an AWS Lambda Extension.
package lambda

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/open-policy-agent/opa/logging"
	"github.com/open-policy-agent/opa/plugins"
	"github.com/open-policy-agent/opa/runtime"
	"github.com/open-policy-agent/opa/util"
)

const (
	// Name is the name of the plugin
	Name                           = "lambda_extension"
	defaultTriggerTimeout          = int(7)
	defaultMinimumTriggerThreshold = int(30)
)

var (
	extensionName = filepath.Base(os.Args[0]) // extension name has to match the filename
	// Decision logs are typically the most important plugin to trigger when lambda is shutting down.
	// Status is nice to have, but not critical as the instance will disappear in just a second.
	// Bundle and discovery don't do anything on shutdown.
	defaultPluginStopPriority = []string{
		"decision_logs",
		"status",
		"bundle",
		"discovery",
	}
	// Discovery should always be started first, followed by bundle.
	// Status should be last so that the statuses of all other plugins are available to report.
	defaultPluginStartPriority = []string{
		"discovery",
		"bundle",
		"decision_logs",
		"status",
	}
)

// Config represents the plugin configuration.
type Config struct {
	// The minimum time in seconds that must elapse before plugins will be triggered. Right now,
	// all plugins are triggered at once, and they all must use the same minimum threshold.
	MinimumTriggerThreshold *int `json:"minimum_trigger_threshold,omitempty"`
	// The maximum time in seconds that ALL plugins have to run. Once the timeout elapses, all plugin
	// runs will be cancelled and the lambda_extension plugin will move on to the next event.
	TriggerTimeout *int `json:"trigger_timeout,omitempty"`
	// The order, from first to last, that plugins will be started during intialization.
	PluginStartPriority *[]string `json:"plugin_start_priority,omitempty"`
	// the order, from first to last, that plugins will be stopped during shutdown.
	PluginStopPriority *[]string `json:"plugin_stop_priority,omitempty"`
}

// PluginFactory is used by the plugin manager to create plugins and their configuration
type PluginFactory struct{}

// Validate validates configuration and populates defaults as needed
func (p *PluginFactory) Validate(manager *plugins.Manager, config []byte) (interface{}, error) {

	var parsedConfig Config

	if err := util.Unmarshal(config, &parsedConfig); err != nil {
		return nil, err
	}

	triggerTimeout := defaultTriggerTimeout
	if parsedConfig.TriggerTimeout == nil {
		parsedConfig.TriggerTimeout = &triggerTimeout
	}

	minimumTriggerThreshold := defaultMinimumTriggerThreshold
	if parsedConfig.MinimumTriggerThreshold == nil {
		parsedConfig.MinimumTriggerThreshold = &minimumTriggerThreshold
	}

	pluginStartPriority := defaultPluginStartPriority
	if parsedConfig.PluginStartPriority == nil {
		parsedConfig.PluginStartPriority = &pluginStartPriority
	}

	pluginStopPriority := defaultPluginStopPriority
	if parsedConfig.PluginStopPriority == nil {
		parsedConfig.PluginStopPriority = &pluginStopPriority
	}

	return &parsedConfig, nil
}

func defaultConfig() Config {
	minimumTriggerThreshold := defaultMinimumTriggerThreshold
	triggerTimeout := defaultTriggerTimeout
	pluginStartPriority := defaultPluginStartPriority
	pluginStopPriority := defaultPluginStopPriority
	return Config{
		MinimumTriggerThreshold: &minimumTriggerThreshold,
		TriggerTimeout:          &triggerTimeout,
		PluginStartPriority:     &pluginStartPriority,
		PluginStopPriority:      &pluginStopPriority,
	}
}

// New creates a new instances of the lambda extension plugin.
func (p *PluginFactory) New(manager *plugins.Manager, config interface{}) plugins.Plugin {
	parsedConfig := defaultConfig()
	if config != nil {
		parsedConfig = *config.(*Config)
	}
	logger := manager.Logger().WithFields(map[string]interface{}{"plugin": Name})

	plugin := &Plugin{
		manager: manager,
		config:  parsedConfig,
		stop:    make(chan chan struct{}),
		logger:  logger,
		client:  NewClient(os.Getenv("AWS_LAMBDA_RUNTIME_API")),
	}

	manager.UpdatePluginStatus(Name, &plugins.Status{State: plugins.StateNotReady})

	return plugin
}

// Plugin implements a Lambda Extension client and controls OPA in a manner that is
// compatible with the Lambda Extension lifecycle. Instead of plugins being triggered
// periodically after a configured delay, they are triggered by this plugin after a
// minimum threshold of time has passed. When the minimum threshold has elapsed, the
// plugins will be triggered as soon as the Lambda Service indicates the next event
// is ready to process. This happens in parallel with the Lambda function running, so
// there is no guarantee that triggers will complete before the function completes its
// processing. The function is capable of responding to the client while the lambda extension
// is still triggering plugins. However, this plugin won't request the next event from the
// Lambda service until all triggers are complete (or timeout). This has ramifications for
// bundle loading and log shipping. When a bundle is changed, there will always be at least one
// lambda event that does not process with the new bundle. Likewise, it is possible for some
// logs to not ship until the Lambda shuts down from idleness (i.e. if X requests are processed
// before the minimum threshold of time has elapsed, then they will sit in the buffer until the
// next request, or when this plugin processes the lambda shutdown event).
type Plugin struct {
	manager         *plugins.Manager
	config          Config
	stop            chan chan struct{}
	logger          logging.Logger
	client          *Client
	lastTriggerTime time.Time
}

// Start starts the plugin.
func (p *Plugin) Start(ctx context.Context) error {
	p.logger.Info("Starting %s.", Name)
	res, err := p.client.Register(ctx, extensionName)
	p.logger.Debug("Registered extension, %v", res)
	if err != nil {
		return err
	}
	// Most of the initialization needs to be done in a goroutine because the plugin manager must
	// finish starting all the plugins before the server is initialized, and the server must be
	// initialized before the Lambda Service is called for the first event. Plugin state must also
	// be set to OK for the server to initialize.
	p.manager.UpdatePluginStatus(Name, &plugins.Status{State: plugins.StateOK})
	go func() {
		p.triggerPlugins(ctx, *p.config.PluginStartPriority)
		// Wait for OPA server to fully initialize before starting the loop
		<-p.manager.ServerInitializedChannel()
		// When loop starts, plugin signals to lambda that is is ready for events, so all
		// OPA initialization should be complete by this point
		p.loop()
	}()
	return nil
}

// Stop stops the plugin.
func (p *Plugin) Stop(ctx context.Context) {
	p.logger.Info("Stopping %s.", Name)
	done := make(chan struct{})
	p.stop <- done
	<-done
	p.manager.UpdatePluginStatus(Name, &plugins.Status{State: plugins.StateNotReady})
}

// Reconfigure does nothing for this plugin.
func (p *Plugin) Reconfigure(ctx context.Context, config interface{}) {
	// no-op
}

func (p *Plugin) loop() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for {
		select {
		case done := <-p.stop:
			done <- struct{}{}
			return
		default:
			// Tell the lambda service that the extension is ready for the next event
			res, err := p.client.NextEvent(ctx)

			if err != nil {
				p.logger.Error("Extension failed to get next event, %v", err)
				return
			}

			p.logger.Debug("Received event, %v", res)

			// Shutdown event happens once, when Lambda is destroying the lambda instance. No further
			// events will be received after this one.
			if res.EventType == Shutdown {
				p.manager.UpdatePluginStatus(Name, &plugins.Status{State: plugins.StateNotReady})
				tCtx, cancel := context.WithTimeout(ctx, time.Duration(*p.config.TriggerTimeout)*time.Second)
				defer cancel()
				// When Lambda is shutting down an instance, this extension has ~2 seconds to complete
				// shutdown tasks before a sigkill is sent. Therefore, it is important to prioritize plugin
				// cleanup steps to ensure critical steps have the best chance to finish successfully.
				for _, pluginName := range *p.config.PluginStopPriority {
					// Trigger status and decision_logs plugins during stop to dump all remaining status
					// updates and decision logs, because stopping these plugins does not do this
					// automatically.
					if pluginName == "status" || pluginName == "decision_logs" {
						p.triggerPlugin(tCtx, pluginName)
					}
					plugin := p.manager.Plugin(pluginName)
					if plugin != nil {
						plugin.Stop(tCtx)
					}
				}
				return
			} else {
				// If the minimum trigger threshold has elapsed, then trigger all the plugins
				if time.Since(p.lastTriggerTime).Seconds() > float64(*p.config.MinimumTriggerThreshold) {
					p.lastTriggerTime = time.Now()
					p.triggerAllPlugins(ctx)
				}
			}
		}
	}
}

func (p *Plugin) triggerAllPlugins(ctx context.Context) {
	p.triggerPlugins(ctx, p.manager.Plugins())
}

func (p *Plugin) triggerPlugins(ctx context.Context, pluginNames []string) {
	tCtx, cancel := context.WithTimeout(ctx, time.Duration(*p.config.TriggerTimeout)*time.Second)
	defer cancel()
	for _, pluginName := range pluginNames {
		p.triggerPlugin(tCtx, pluginName)
	}
}

func (p *Plugin) triggerPlugin(ctx context.Context, pluginName string) {
	plugin := p.manager.Plugin(pluginName)
	if plugin == nil {
		return
	}
	triggerable, ok := plugin.(plugins.Triggerable)
	if !ok {
		return
	}
	err := triggerable.Trigger(ctx)
	if err != nil {
		p.logger.Error("Error while triggering plugin: %s, %v", pluginName, err)
	}
}

func init() {
	runtime.RegisterPlugin(Name, &PluginFactory{})
}
