// Copyright (c) 2021 GoDaddy Operating Company, LLC. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package lambda

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"

	"github.com/open-policy-agent/opa/bundle"
	"github.com/open-policy-agent/opa/logging"
	"github.com/open-policy-agent/opa/metrics"
	"github.com/open-policy-agent/opa/plugins"
	"github.com/open-policy-agent/opa/plugins/discovery"
	"github.com/open-policy-agent/opa/plugins/logs"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/server"
	"github.com/open-policy-agent/opa/storage/inmem"
	"github.com/open-policy-agent/opa/util"
)

func TestPluginFactoryValidate(t *testing.T) {
	manager, err := plugins.New(nil, "test", inmem.New())
	if err != nil {
		t.Fatal(err)
	}
	factory := PluginFactory{}
	c, err := factory.Validate(manager, []byte(`{
    "minimum_trigger_threshold": 100,
    "plugin_start_priority": [
      "foo"
    ],
    "plugin_stop_priority": [
      "bar"
    ],
    trigger_timeout: 50
  }`))
	if err != nil {
		t.Fatal(err)
	}
	config, ok := c.(*Config)
	if !ok {
		t.Fatalf("Validate returned an unexpected type for config, expected 'Config', got '%T'", config)
	}
	expectedConfig := &Config{
		MinimumTriggerThreshold: getIntPointer(100),
		TriggerTimeout:          getIntPointer(50),
		PluginStartPriority: &[]string{
			"foo",
		},
		PluginStopPriority: &[]string{
			"bar",
		},
	}
	if !reflect.DeepEqual(config, expectedConfig) {
		t.Fatalf("Expected\n%v Got\n%v", spew.Sdump(expectedConfig), spew.Sdump(config))
	}
}

func TestPluginFactoryValidateDefaults(t *testing.T) {
	manager, err := plugins.New(nil, "test", inmem.New())
	if err != nil {
		t.Fatal(err)
	}
	factory := PluginFactory{}
	c, err := factory.Validate(manager, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	config, ok := c.(*Config)
	if !ok {
		t.Fatalf("Validate returned an unexpected type for config, expected 'Config', got '%T'", config)
	}
	expectedConfig := defaultConfig()
	if !reflect.DeepEqual(config, &expectedConfig) {
		t.Fatalf("Expected\n%v Got\n%v", spew.Sdump(expectedConfig), spew.Sdump(config))
	}
}

// This is an integration test that runs through the full lifecycle
// of the lambda_extension plugin using a mocked http server that
// stands in for the Lambda API, the discovery API, the bundle API,
// the status API, and the decision log API. There's a lot going
// on here, but the comments should help you piece it together.
func TestPluginLifecycle(t *testing.T) {
	ctx := context.Background()

	// the test fixture takes care of setting up the mock http server, the opa
	// plugin manager, and the discovery and lambda plugins
	fixture := newTestFixture(t, ctx)
	defer fixture.stop()

	// fixture must be started in a goroutine to avoid deadlocks
	go func() {
		fixture.start()
	}()

	// the manager has finished initialization inside the fixture and parts of
	// it are ready to be called
	<-fixture.managerReady

	// make sure the lambda_extension plugin hasn't finished it's start process yet
	// by verifying that the lambda_extension is in a NotReady state
	lambdaExtensionPluginState := fixture.manager.PluginStatus()["lambda_extension"].State
	if lambdaExtensionPluginState != plugins.StateNotReady {
		t.Fatal("Expected lambda_extension plugin state to be NotReady before extension is registered with Lambda API")
	}

	// verify that the foo bundle has not been loaded, because the lambda_extension
	// plugin has not triggered any plugins yet
	preTriggerFooBundle, err := fixture.runQuery(ctx, "data.foo.bar")
	if err != nil {
		t.Fatal(err)
	}
	if preTriggerFooBundle != nil {
		t.Fatalf("Expected foo bundle query to result in nil, but got %v", preTriggerFooBundle)
	}

	// register the lambda extension with the Lambdd API. once its registered,
	// the lambda plugin will complete it's start process.
	fixture.server.registerChannel <- struct{}{}

	// wait for the manager to finish its start process. all plugins
	// must complete their start process for this to happen.
	<-fixture.managerStarted

	// now that the manager has started, the lambda_extension should report
	// its state as OK
	lambdaExtensionPluginState = fixture.manager.PluginStatus()["lambda_extension"].State
	if lambdaExtensionPluginState != plugins.StateOK {
		t.Fatal("Expected lambda_extension plugin state to be OK after extension is registered with Lambda API")
	}

	// now that all the plugins have started, we can signal that the server
	// has fully initiatlized, which will cause the lambda_extension plugin
	// to request the next event from the Lambda API
	fixture.manager.ServerInitialized()

	// wait for the lambda_extension plugin to request the next event
	// from the Lambda API. At this point, all plugins specified in
	// the PluginStartPriority should have been triggered
	<-fixture.server.nextEventEnterChannel

	// verify that the foo bundle has been loaded, which verifies that both
	// the discovery and bundle plugins have been triggered
	postTriggerFooBundle, err := fixture.runQuery(ctx, "data.foo.bar")
	if err != nil {
		t.Fatal(err)
	}
	if postTriggerFooBundle != "baz" {
		t.Fatalf("Expected foo bundle query to result in 'baz', but got %v", postTriggerFooBundle)
	}

	// verify that a single status log has been generated from
	// the initial trigger at startup
	if len(fixture.server.statusEvents) != 1 {
		t.Fatalf("Expected 1 status update, but got %v", len(fixture.server.statusEvents))
	}

	// verify that no decision logs have been shipped, because none were generated
	// prior to the initial trigger at startup
	if len(fixture.server.logEvents) != 0 {
		t.Fatalf("Expected 0 decision logs, but got %v", len(fixture.server.logEvents))
	}

	// generate a decision log we can verify after the next event
	fixture.generateDecisionLog(ctx)

	// pass the next event to the lambda_extension plugin
	// this should trigger all plugins and set the inital timestamp
	// against which the minimum trigger threshold will be applied by the next
	// trigger
	fixture.server.nextEventExitChannel <- "INVOKE"

	// wait for the next event
	<-fixture.server.nextEventEnterChannel

	// verify that a second status log has been generated from the triggers
	// generated by the first event sent by the Lambda API
	if len(fixture.server.statusEvents) != 2 {
		t.Fatalf("Expected 2 status update, but got %v", len(fixture.server.statusEvents))
	}

	// verify that a decision log has been shipped
	if len(fixture.server.logEvents) != 1 {
		t.Fatalf("Expected 1 decision logs, but got %v", len(fixture.server.logEvents))
	}

	// generate another decision log
	fixture.generateDecisionLog(ctx)

	// pass the next event
	fixture.server.nextEventExitChannel <- "INVOKE"

	// wait for the next event
	<-fixture.server.nextEventEnterChannel

	// at this point, we should not trigger more plugins because we have
	// not exceeded the minimum trigger threshold

	// verify we are still at 2 status events
	if len(fixture.server.statusEvents) != 2 {
		t.Fatalf("Expected 2 status update, but got %v", len(fixture.server.statusEvents))
	}

	// verify that we are still at a single decision log shipped
	if len(fixture.server.logEvents) != 1 {
		t.Fatalf("Expected 1 decision logs, but got %v", len(fixture.server.logEvents))
	}

	// wait for 3 seconds to pass the minimum trigger threshold of 2 seconds
	time.Sleep(time.Second * 3)

	// pass the next event
	fixture.server.nextEventExitChannel <- "INVOKE"

	// wait for the next event
	<-fixture.server.nextEventEnterChannel

	// verify we are now at 3 status events, because plugins were triggered again
	if len(fixture.server.statusEvents) != 3 {
		t.Fatalf("Expected 3 status update, but got %v", len(fixture.server.statusEvents))
	}

	// verify that 2 decision logs have been shipped, because plugins were triggered again
	if len(fixture.server.logEvents) != 2 {
		t.Fatalf("Expected 2 decision logs, but got %v", len(fixture.server.logEvents))
	}

	// generate another decision log
	fixture.generateDecisionLog(ctx)

	// pass the the shutdown event.
	// the shutdown event will trigger the plugins in the PluginStopPriority
	// list
	fixture.server.nextEventExitChannel <- "SHUTDOWN"

	// sleep for a second to give the shutdown time to finish.
	// this is hacky, but there does not appear to be a better
	// way to hook into the shutdown process
	time.Sleep(time.Second * 1)

	// verify we are now at 4 status events, because plugins were
	// triggered again by shutdown
	if len(fixture.server.statusEvents) != 4 {
		t.Fatalf("Expected 4 status update, but got %v", len(fixture.server.statusEvents))
	}

	// verify that 3 decision logs have been shipped, because plugins were
	// triggered again by shutdown
	if len(fixture.server.logEvents) != 3 {
		t.Fatalf("Expected 3 decision logs, but got %v", len(fixture.server.logEvents))
	}
}

func getIntPointer(i int) *int {
	return &i
}

type testFixture struct {
	ctx            context.Context
	manager        *plugins.Manager
	managerReady   chan struct{}
	managerStarted chan struct{}
	server         *testFixtureServer
	t              *testing.T
}

func newTestFixture(t *testing.T, ctx context.Context) *testFixture {
	tf := testFixture{
		ctx:            ctx,
		managerReady:   make(chan struct{}),
		managerStarted: make(chan struct{}),
		server:         newTestFixtureServer(t),
		t:              t,
	}
	// strip http:// prefix
	os.Setenv("AWS_LAMBDA_RUNTIME_API", tf.server.server.URL[7:])
	return &tf
}

func (t *testFixture) runQuery(ctx context.Context, query string) (interface{}, error) {
	r := rego.New(
		rego.Query(query),
		rego.Store(t.manager.Store),
		rego.Metrics(metrics.New()),
	)
	rs, err := r.Eval(ctx)

	if err != nil {
		return nil, err
	}

	if len(rs) == 0 {
		return nil, nil
	}

	return rs[0].Expressions[0].Value, nil
}

func (t *testFixture) generateDecisionLog(ctx context.Context) {
	plugin := logs.Lookup(t.manager)
	if plugin == nil {
		return
	}
	if err := plugin.Log(ctx, &server.Info{}); err != nil {
		t.t.Fatal("Failed to generate a decision log")
	}
}

func (t *testFixture) start() {
	managerConfig := []byte(fmt.Sprintf(`{
    "discovery": {
      "decision": "config",
      "name": "discovery",
      "resource": "/bundles/discovery",
      "service": "integration-test",
      "trigger": "manual"
    },
    "services": [
      {
        "name": "integration-test",
        "url": %q
      }
    ]
  }`, t.server.server.URL))
	manager, err := plugins.New(managerConfig, "test-fixture", inmem.New())
	if err != nil {
		t.t.Fatal(err)
	}
	t.manager = manager
	t.manager.Logger().SetLevel(logging.Debug)
	discoveryPlugin, err := discovery.New(t.manager)
	if err != nil {
		t.t.Fatal(err)
	}
	t.manager.Register("discovery", discoveryPlugin)
	lambdaPluginFactory := PluginFactory{}
	lambdaPluginConfig := defaultConfig()
	// set the threshold to 2 seconds so that we can test it without sleeping too long
	lambdaPluginConfig.MinimumTriggerThreshold = getIntPointer(2)
	lambdaPlugin := lambdaPluginFactory.New(t.manager, &lambdaPluginConfig)
	t.manager.Register(Name, lambdaPlugin)
	if err := t.manager.Init(t.ctx); err != nil {
		t.t.Fatal(err)
	}
	t.managerReady <- struct{}{}
	if err := t.manager.Start(t.ctx); err != nil {
		t.t.Fatal(err)
	}
	t.managerStarted <- struct{}{}
}

func (t *testFixture) stop() {
	t.server.stop()
}

type testFixtureServer struct {
	logEvents             []logs.EventV1
	server                *httptest.Server
	statusEvents          []interface{}
	nextEventEnterChannel chan struct{}
	nextEventExitChannel  chan string
	registerChannel       chan struct{}
	t                     *testing.T
}

func newTestFixtureServer(t *testing.T) *testFixtureServer {
	tfs := testFixtureServer{
		logEvents:             []logs.EventV1{},
		nextEventEnterChannel: make(chan struct{}),
		nextEventExitChannel:  make(chan string),
		registerChannel:       make(chan struct{}),
		statusEvents:          []interface{}{},
		t:                     t,
	}
	tfs.server = httptest.NewServer(http.HandlerFunc(tfs.handle))
	return &tfs
}

func (t *testFixtureServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/2020-01-01/extension/register" {
		<-t.registerChannel
		fmt.Fprintf(w, `{
      "functionName": "foo",
      "functionVersion": "1",
      "handler": "bar"
    }`)
	} else if r.URL.Path == "/2020-01-01/extension/event/next" {
		t.nextEventEnterChannel <- struct{}{}
		eventType := <-t.nextEventExitChannel
		fmt.Fprintf(w, `{
      "eventType": %q,
      "deadlineMs": 60000,
      "requestId": "bar",
      "invokedFunctionArn": "baz",
      "tracing": {
        "type": "boo",
        "value": "bah"
      }
    }`, eventType)
	} else if r.URL.Path == "/bundles/discovery" {
		discoveryBundle := bundle.Bundle{
			Manifest: bundle.Manifest{Revision: "foo"},
			Data: util.MustUnmarshalJSON([]byte(`{
        "config": {
          "bundles": {
            "foo": {
              "resource": "/bundles/foo",
              "service": "integration-test"
            }
          },
          "status": {"service": "integration-test"},
          "decision_logs": {"service": "integration-test"}
        }
      }`)).(map[string]interface{}),
		}
		err := bundle.NewWriter(w).Write(discoveryBundle)
		if err != nil {
			t.t.Fatal(err)
		}
	} else if r.URL.Path == "/bundles/foo" {
		fooBundle := bundle.Bundle{
			Manifest: bundle.Manifest{Revision: "foo"},
			Data: util.MustUnmarshalJSON([]byte(`{
        "foo": {
          "bar": "baz"
        }
      }`)).(map[string]interface{}),
		}
		err := bundle.NewWriter(w).Write(fooBundle)
		if err != nil {
			t.t.Fatal(err)
		}
	} else if r.URL.Path == "/status" {
		var event interface{}
		if err := util.NewJSONDecoder(r.Body).Decode(&event); err != nil {
			t.t.Fatal(err)
		}
		t.statusEvents = append(t.statusEvents, event)
	} else if r.URL.Path == "/logs" {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.t.Fatal(err)
		}
		var events []logs.EventV1
		if err := json.NewDecoder(gr).Decode(&events); err != nil {
			t.t.Fatal(err)
		}
		if err := gr.Close(); err != nil {
			t.t.Fatal(err)
		}
		t.logEvents = append(t.logEvents, events...)
	} else {
		t.t.Fatal("unexpected path sent to test fixture server")
	}
}

func (t *testFixtureServer) stop() {
	t.server.Close()
}
