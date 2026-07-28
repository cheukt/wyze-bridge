// viam-module is the Viam module entry point. It registers the
// cheukt:wyze-bridge:manager generic service and the
// cheukt:wyze-bridge:conditional-camera component (see internal/viammod) and
// hands control to the rdk module runtime.
package main

import (
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/module"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/IDisposable/docker-wyze-bridge/internal/viammod"
)

func main() {
	module.ModularMain(
		resource.APIModel{API: generic.API, Model: viammod.Model},
		resource.APIModel{API: camera.API, Model: viammod.ConditionalModel},
	)
}
