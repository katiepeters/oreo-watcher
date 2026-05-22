package main

import (
	"context"

	"go.viam.com/rdk/components/audioin"
	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	utils "go.viam.com/utils"

	barkmonitor "github.com/katie-viam/oreo-watcher/barkmonitor"
	filteredmic "github.com/katie-viam/oreo-watcher/filteredmic"
	spectrogramcam "github.com/katie-viam/oreo-watcher/spectrogramcam"
	waveformcam "github.com/katie-viam/oreo-watcher/waveformcam"
)

func main() {
	utils.ContextualMain(mainWithArgs, module.NewLoggerFromArgs("oreo-watcher"))
}

func mainWithArgs(ctx context.Context, args []string, logger logging.Logger) error {
	myMod, err := module.NewModuleFromArgs(ctx)
	if err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, audioin.API, filteredmic.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, camera.API, waveformcam.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, camera.API, spectrogramcam.Model); err != nil {
		return err
	}

	if err = myMod.AddModelFromRegistry(ctx, sensor.API, barkmonitor.Model); err != nil {
		return err
	}

	err = myMod.Start(ctx)
	defer myMod.Close(ctx)
	if err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}
