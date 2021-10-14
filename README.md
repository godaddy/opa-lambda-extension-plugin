# opa-lambda-extension-plugin

[![ci](https://github.com/godaddy/opa-lambda-extension-plugin/actions/workflows/ci.yml/badge.svg?branch=main&event=push)](https://github.com/godaddy/opa-lambda-extension-plugin/actions/workflows/ci.yml?query=branch%3Amain+event%3Apush)

A custom plugin for running [Open Policy Agent (OPA)](https://github.com/open-policy-agent/opa) in [AWS Lambda](https://aws.amazon.com/lambda/) as a [Lambda Extension](https://docs.aws.amazon.com/lambda/latest/dg/using-extensions.html).

To learn more about how Lambda Extensions work, check out [these AWS docs](https://docs.aws.amazon.com/lambda/latest/dg/runtimes-extensions-api.html), which provide a helpful graphic depicting the Lambda lifecycle.

## Usage

This project provides an OPA plugin that integrates OPA with the Lambda Extension API. To use this plugin, you must compile a custom version of the OPA binary. Instructions to do this are available in [OPA's documentation](https://www.openpolicyagent.org/docs/latest/extensions/#custom-plugins-for-opa-runtime). In the future, we hope to contribute/release changes that provide a simpler experience, similar to the [opa-envoy-plugin project](https://github.com/open-policy-agent/opa-envoy-plugin).

This plugin can be tricky to implement, depending on whether or not you use the OPA [discovery plugin](https://www.openpolicyagent.org/docs/latest/management-discovery/).

### Usage without Discovery

If you don't need to use the discovery plugin, then you don't need to make many changes in your custom OPA compilation. Just modify [main.go](https://github.com/open-policy-agent/opa/blob/main/main.go) to import the plugin module (see snippet just below). If you're unfamiliar with custom plugins in OPA, check out the [OPA docs on the subject](https://www.openpolicyagent.org/docs/latest/extensions/#custom-plugins-for-opa-runtime).

```go
// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package main

import (
  "fmt"
  "os"

  // Import the plugin so that it can register itself with the OPA runtime on init
  _ "github.com/godaddy/opa-lambda-extension-plugin/plugins/lambda"

  "github.com/open-policy-agent/opa/cmd"
)

func main() {
  if err := cmd.RootCommand.Execute(); err != nil {
    fmt.Println(err)
    os.Exit(1)
  }
}

// Capabilities file generation:
//go:generate build/gen-run-go.sh internal/cmd/genopacapabilities/main.go capabilities.json

// WASM base binary generation:
//go:generate build/gen-run-go.sh internal/cmd/genopawasm/main.go -o internal/compiler/wasm/opa/opa.go internal/compiler/wasm/opa/opa.wasm  internal/compiler/wasm/opa/callgraph.csv
```

Everything else can be wired up in [OPA configuration](https://www.openpolicyagent.org/docs/latest/configuration/). Note the `trigger: manual` on the `bundles`, `decision_logs`, and `status` plugins.

```yaml
services:
  acmecorp:
    url: https://example.com/control-plane-api/v1

bundles:
  authz:
    service: acmecorp
    resource: bundles/http/example/authz.tar.gz
    polling:
      trigger: manual
      scope: write

decision_logs:
  service: acmecorp
  reporting:
    trigger: manual

status:
  service: acmecorp
  trigger: manual

plugins:
  lambda_extension:
    minimum_trigger_threshold: 30
    trigger_timeout: 7
    plugin_start_priority:
      - bundle
      - decision_logs
      - status
    plugin_stop_priority:
      - decision_logs
      - status
      - bundle
```

### Usage with Discovery

The discovery plugin prevents other plugins from being registered in the bootstrap configuration. This prevents the lambda extension plugin from being registered via configuration, because the lambda extension plugin must run _before_ the discovery plugin. To use the lambda extension plugin with discovery, you must compile a custom OPA binary wherein you register the lambda extension plugin directly with the runtime, e.g.

```go
lambdaPluginFactory := lambda.PluginFactory{}
rt.Manager.Register(lambda.Name, lambdaPluginFactory.New(rt.Manager, nil))
```

This is less than ideal and the complexity involved with this implementation is outside the scope of this document. For now, just know that if you really need to implement both the discovery and lambda extension plugins, it is possible to do so. In the future, we hope to contribute/release changes that make this implementation simpler.

## Configuration

```yaml
plugins:
  lambda_extension:
    # The number of seconds that must elapse before plugins will be triggered by a lambda function invocation
    minimum_trigger_threshold: 30
    # The number of seconds that ALL plugins have to complete their trigger before they are canceled.
    trigger_timeout: 7
    # The order in which plugins will be started while the Lambda Extension is in its init phase.
    plugin_start_priority:
      - bundle
      - decision_logs
      - status
    # The order in which plugins will be stopped while the Lambda Extension is in its shutdown phase.
    plugin_stop_priority:
      - decision_logs
      - status
      - bundle
```

## Development

```
make fmt

make lint

make test
```

## More Information 

[The plugin](plugins/lambda/plugin.go) is heavily commented with useful information.
